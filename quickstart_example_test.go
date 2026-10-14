package entitlements_test

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	play "google.golang.org/api/androidpublisher/v3"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/google/androidpublisher"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/httpx/pubsubauth"
	"github.com/tggo/go-entitlements/store/memory"
	"github.com/tggo/go-entitlements/store/postgres"
)

// These examples are compiled by `go test`, and README.md must repeat their
// bodies verbatim (TestReadmeSnippetsMatchExamples), so the quickstart cannot
// drift from the API.

func Example_appleOnly() {
	ctx := context.Background()
	logger := slog.Default()

	svc := entitlements.NewService(memory.New(), entitlements.WithLogger(logger))

	verifier, err := apple.NewVerifier("com.example.app", // the bundle ID is required
		apple.WithEnvironment(entitlements.EnvironmentProduction)) // reject Sandbox and TestFlight
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/apple", httpx.NewAppleHandler(apple.NewParser(verifier), svc, httpx.WithLogger(logger)))
	go func() { log.Fatal(http.ListenAndServe(":8080", mux)) }()

	// Later, in your own handlers: does this user have access?
	ok, err := svc.IsActive(ctx, "7e3fb20b-4cdb-47cc-936d-99d65f608138", "com.example.pro.monthly")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("has access:", ok)
}

func Example_postgres() {
	ctx := context.Background()
	logger := slog.Default()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	store := postgres.New(pool)
	if err := store.Migrate(ctx); err != nil { // idempotent, safe from several instances
		log.Fatal(err)
	}
	svc := entitlements.NewService(store, entitlements.WithLogger(logger))

	list, err := svc.ListByUser(ctx, "7e3fb20b-4cdb-47cc-936d-99d65f608138")
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range list {
		log.Println(e.ProductID, e.State, e.ExpiresAt)
	}
}

func Example_google() {
	ctx := context.Background()
	logger := slog.Default()
	svc := entitlements.NewService(memory.New(), entitlements.WithLogger(logger))

	playAPI, err := play.NewService(ctx) // credentials from GOOGLE_APPLICATION_CREDENTIALS
	if err != nil {
		log.Fatal(err)
	}
	parser, err := google.NewParser(androidpublisher.NewFetcher(playAPI), "com.example.app")
	if err != nil {
		log.Fatal(err)
	}
	// The Pub/Sub push subscription must send an OIDC token for this audience
	// and service account; the handler refuses to start without a check.
	auth, err := pubsubauth.New("https://example.com/webhooks/google", "push@my-project.iam.gserviceaccount.com")
	if err != nil {
		log.Fatal(err)
	}
	handler, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(logger), httpx.WithPushAuthenticator(auth))
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/webhooks/google", handler)
}
