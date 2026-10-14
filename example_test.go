package entitlements_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/store/memory"
)

func ExampleApply() {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ent, _ := entitlements.Apply(nil, entitlements.Event{
		ID: "n1", Source: entitlements.SourceApple, OriginalTxID: "tx1", ProductID: "pro",
		State: entitlements.StateActive, ExpiresAt: at.Add(30 * 24 * time.Hour), OccurredAt: at,
	})
	fmt.Println(ent.State, ent.Version)
	// Output: active 1
}

func ExampleService_IsActive() {
	ctx := context.Background()
	at := time.Now()
	svc := entitlements.NewService(memory.New())
	_, _ = svc.Process(ctx, entitlements.Event{
		ID: "n1", Source: entitlements.SourceApple, UserID: "u1", OriginalTxID: "tx1", ProductID: "pro",
		State: entitlements.StateActive, ExpiresAt: at.Add(time.Hour), OccurredAt: at,
	})
	ok, _ := svc.IsActive(ctx, "u1", "pro")
	fmt.Println(ok)
	// Output: true
}
