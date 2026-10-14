package serverapi_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/apple/serverapi"
	"github.com/tggo/go-entitlements/store/memory"
)

// Repair a missed webhook and re-feed what Apple says it sent. It needs a real
// App Store Connect key, so it is compiled but not run.
func Example_reconcile() {
	ctx := context.Background()

	key, err := os.ReadFile("AuthKey_ABC123DEFG.p8")
	if err != nil {
		log.Fatal(err)
	}
	client, err := serverapi.New(serverapi.Config{
		IssuerID:      "57246542-96fe-1a63-e053-0824d011072a",
		KeyID:         "ABC123DEFG",
		PrivateKeyPEM: key,
		BundleID:      "com.example.app",
		Environment:   entitlements.EnvironmentProduction,
	})
	if err != nil {
		log.Fatal(err)
	}
	svc := entitlements.NewService(memory.New())

	// 1. A customer says they paid but have no access: ask Apple.
	results, err := serverapi.Reconcile(ctx, client, svc, "2000000000000001")
	for _, r := range results {
		fmt.Println(r.Event.OriginalTxID, r.Outcome)
	}
	if err != nil {
		log.Print(err)
	}

	// 2. After an outage, re-feed every notification Apple could not deliver.
	verifier, err := apple.NewVerifier("com.example.app", apple.WithEnvironment(entitlements.EnvironmentProduction))
	if err != nil {
		log.Fatal(err)
	}
	report, err := serverapi.ReplayNotificationHistory(ctx, client, apple.NewParser(verifier), svc,
		time.Now().Add(-48*time.Hour), time.Now(), serverapi.ReplayOnlyFailures())
	fmt.Printf("applied %d, duplicates %d, stale %d, failed %d\n", report.Applied, report.Duplicate, report.Stale, report.Failed)
	if err != nil {
		log.Print(err)
	}
}

// Walk a customer's whole transaction history; each transaction is verified
// before it is returned.
func Example_transactionHistory() {
	var client *serverapi.Client // from serverapi.New
	for tx, err := range client.TransactionHistory(context.Background(), "2000000000000001", serverapi.HistoryQuery{}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(tx.TransactionID, tx.ProductID)
	}
}

// Answer refund requests. The app decides, per purchase, whether the customer
// consented and what to say; the handler wraps the notification endpoint and
// handles only CONSUMPTION_REQUEST.
func Example_consumptionResponder() {
	var (
		client   *serverapi.Client // from serverapi.New
		verifier *apple.Verifier   // from apple.NewVerifier
		next     http.Handler      // httpx.NewAppleHandler(parser, svc)
	)
	responder := serverapi.ConsumptionResponderFunc(func(ctx context.Context, req serverapi.ConsumptionRequest) (serverapi.ConsumptionInfo, error) {
		consented, err := lookUpConsent(ctx, req.UserID, req.OriginalTransactionID)
		if err != nil {
			return serverapi.ConsumptionInfo{}, err // transient: Apple redelivers
		}
		if !consented {
			return serverapi.ConsumptionInfo{}, serverapi.ErrConsentRequired // nothing is sent
		}
		return serverapi.ConsumptionInfo{
			CustomerConsented:     true,
			DeliveryStatus:        serverapi.DeliveryDelivered,
			SampleContentProvided: true,
			RefundPreference:      serverapi.RefundDecline,
		}, nil
	})
	http.Handle("/apple", serverapi.NewConsumptionHandler(next, serverapi.ConsumptionHandlerConfig{
		Verifier: verifier, Responder: responder, Sender: client,
	}))
}

func lookUpConsent(context.Context, string, string) (bool, error) { return false, nil }
