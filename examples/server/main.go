// Command server is a runnable demo: Apple and Google webhooks backed by
// Postgres, plus a read endpoint for a user's entitlements.
//
// Environment:
//
//	DATABASE_URL                   required, e.g. postgres://postgres:postgres@localhost:5432/entitlements
//	APPLE_BUNDLE_ID                required, rejects notifications for other apps
//	GOOGLE_PACKAGE_NAME            optional, enables /webhooks/google
//	GOOGLE_APPLICATION_CREDENTIALS optional, service account key for the Play Developer API (else ADC)
//	GOOGLE_PUSH_AUDIENCE           optional, with GOOGLE_PUSH_SERVICE_ACCOUNT: verify the Pub/Sub OIDC token
//	GOOGLE_PUSH_SERVICE_ACCOUNT    optional, see above
//	APPLE_ROOT_CA_PEM              LOCAL DEMO ONLY: path to a PEM root replacing Apple's. Never set in production.
//	LISTEN_ADDR                    optional, default :8080
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"google.golang.org/api/androidpublisher/v3"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/store/postgres"
)

func main() {
	log, _ := zap.NewProduction()
	err := run(log)
	_ = log.Sync()
	if err != nil {
		log.Fatal("server failed", zap.Error(err))
	}
}

func run(log *zap.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn, bundleID := os.Getenv("DATABASE_URL"), os.Getenv("APPLE_BUNDLE_ID")
	if dsn == "" || bundleID == "" {
		return errors.New("DATABASE_URL and APPLE_BUNDLE_ID are required")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := postgres.New(pool)
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	svc := entitlements.NewService(store, entitlements.WithLogger(log))

	var verifierOpts []apple.Option
	if path := os.Getenv("APPLE_ROOT_CA_PEM"); path != "" {
		pemBytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return errors.New("APPLE_ROOT_CA_PEM: no certificate found")
		}
		log.Warn("APPLE_ROOT_CA_PEM set: Apple's root CA is NOT trusted. Local demo only.", zap.String("path", path))
		verifierOpts = append(verifierOpts, apple.WithRootPool(roots))
	}

	mux := http.NewServeMux()
	appleVerifier, err := apple.NewVerifier(bundleID, verifierOpts...)
	if err != nil {
		return err
	}
	mux.Handle("/webhooks/apple", httpx.NewAppleHandler(apple.NewParser(appleVerifier), svc, log))

	if pkg := os.Getenv("GOOGLE_PACKAGE_NAME"); pkg != "" {
		play, err := androidpublisher.NewService(ctx) // honours GOOGLE_APPLICATION_CREDENTIALS
		if err != nil {
			return err
		}
		parser := google.NewParser(google.NewAndroidPublisherFetcher(play), google.WithPackageName(pkg))
		var opts []httpx.Option
		if aud, sa := os.Getenv("GOOGLE_PUSH_AUDIENCE"), os.Getenv("GOOGLE_PUSH_SERVICE_ACCOUNT"); aud != "" && sa != "" {
			opts = append(opts, httpx.WithPushAuthenticator(httpx.NewPubSubAuthenticator(aud, sa)))
		} else {
			log.Warn("Pub/Sub push authentication is off: set GOOGLE_PUSH_AUDIENCE and GOOGLE_PUSH_SERVICE_ACCOUNT")
		}
		mux.Handle("/webhooks/google", httpx.NewGoogleHandler(parser, svc, log, opts...))
	}

	mux.HandleFunc("GET /entitlements", func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			http.Error(w, "user_id is required", http.StatusBadRequest)
			return
		}
		list, err := store.ListByUser(r.Context(), userID)
		if err != nil {
			log.Error("list entitlements", zap.Error(err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		now := time.Now()
		type item struct {
			ProductID    string    `json:"product_id"`
			Source       string    `json:"source"`
			OriginalTxID string    `json:"original_tx_id"`
			State        string    `json:"state"`
			ExpiresAt    time.Time `json:"expires_at"`
			Active       bool      `json:"active"`
		}
		out := make([]item, 0, len(list))
		for _, e := range list {
			out = append(out, item{e.ProductID, string(e.Source), e.OriginalTxID, string(e.State), e.ExpiresAt, e.Active(now)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("listening", zap.String("addr", addr))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
