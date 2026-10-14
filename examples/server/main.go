// Command server is a runnable demo: Apple and Google webhooks backed by
// Postgres, plus a read endpoint for a user's entitlements.
//
// Environment:
//
//	DATABASE_URL                   required, e.g. postgres://postgres:postgres@localhost:5432/entitlements
//	APPLE_BUNDLE_ID                required, rejects notifications for other apps
//	GOOGLE_PACKAGE_NAME            optional, enables /webhooks/google
//	GOOGLE_APPLICATION_CREDENTIALS optional, service account key for the Play Developer API (else ADC)
//	GOOGLE_PUSH_AUDIENCE           required with GOOGLE_PACKAGE_NAME: audience of the Pub/Sub push OIDC token
//	GOOGLE_PUSH_SERVICE_ACCOUNT    required with GOOGLE_PACKAGE_NAME: service account email of the push subscription
//	APPLE_ROOT_CA_PEM              LOCAL DEMO ONLY: path to a PEM root replacing Apple's. Never set in production.
//	LISTEN_ADDR                    optional, default :8080
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	play "google.golang.org/api/androidpublisher/v3"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/google/androidpublisher"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/httpx/pubsubauth"
	"github.com/tggo/go-entitlements/store/postgres"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
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
	if err = store.Migrate(ctx); err != nil {
		return err
	}
	svc := entitlements.NewService(store, entitlements.WithLogger(log))

	var verifierOpts []apple.Option
	if path := os.Getenv("APPLE_ROOT_CA_PEM"); path != "" {
		pemBytes, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return errors.New("APPLE_ROOT_CA_PEM: no certificate found")
		}
		log.Warn("APPLE_ROOT_CA_PEM set: Apple's root CA is NOT trusted. Local demo only.", "path", path)
		verifierOpts = append(verifierOpts, apple.WithRootPool(roots))
	}

	mux := http.NewServeMux()
	appleVerifier, err := apple.NewVerifier(bundleID, verifierOpts...)
	if err != nil {
		return err
	}
	mux.Handle("/webhooks/apple", httpx.NewAppleHandler(apple.NewParser(appleVerifier), svc, httpx.WithLogger(log)))

	if pkg := os.Getenv("GOOGLE_PACKAGE_NAME"); pkg != "" {
		svcPlay, err := play.NewService(ctx) // honours GOOGLE_APPLICATION_CREDENTIALS
		if err != nil {
			return err
		}
		parser, err := google.NewParser(androidpublisher.NewFetcher(svcPlay), pkg)
		if err != nil {
			return err
		}
		aud, sa := os.Getenv("GOOGLE_PUSH_AUDIENCE"), os.Getenv("GOOGLE_PUSH_SERVICE_ACCOUNT")
		if aud == "" || sa == "" {
			return errors.New("GOOGLE_PACKAGE_NAME is set: GOOGLE_PUSH_AUDIENCE and GOOGLE_PUSH_SERVICE_ACCOUNT are required, an unauthenticated push endpoint lets anyone forge refunds")
		}
		auth, err := pubsubauth.New(aud, sa)
		if err != nil {
			return err
		}
		h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(log), httpx.WithPushAuthenticator(auth))
		if err != nil {
			return err
		}
		mux.Handle("/webhooks/google", h)
	}

	mux.HandleFunc("GET /entitlements", func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			http.Error(w, "user_id is required", http.StatusBadRequest)
			return
		}
		list, err := store.ListByUser(r.Context(), userID)
		if err != nil {
			log.ErrorContext(r.Context(), "list entitlements", "error", err)
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
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
