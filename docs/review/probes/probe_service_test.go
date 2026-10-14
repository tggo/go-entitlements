//go:build probe

package entitlements_test

import (
	"context"
	"crypto/x509"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/memory"
)

var pT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func pev(id, tx string, at time.Time, st entitlements.State) entitlements.Event {
	return entitlements.Event{ID: id, Source: entitlements.SourceGoogle, UserID: "u", OriginalTxID: tx, ProductID: "p",
		State: st, OccurredAt: at, ExpiresAt: pT0.Add(30 * 24 * time.Hour)}
}

// Supersede arrives before the old chain was ever seen: the synthetic retire
// event is recorded as processed and nothing persists; the old chain's events
// that arrive later create it active and nothing retires it.
func TestProbe_SupersedeBeforeOldChainSeen_IsLost(t *testing.T) {
	st := memory.New()
	svc := entitlements.NewService(st, entitlements.WithTimeFn(func() time.Time { return pT0.Add(time.Hour) }))
	ctx := context.Background()
	n := pev("new1", "NEW", pT0.Add(10*time.Minute), entitlements.StateActive)
	n.SupersedesTxID = "OLD"
	if _, err := svc.Process(ctx, n); err != nil {
		t.Fatal(err)
	}
	// old chain's (delayed/out-of-order) event, older than the supersede
	if _, err := svc.Process(ctx, pev("old1", "OLD", pT0, entitlements.StateActive)); err != nil {
		t.Fatal(err)
	}
	old, _ := st.Get(ctx, entitlements.SourceGoogle, "OLD")
	t.Logf("OLD chain after delayed event: state=%s expires=%s", old.State, old.ExpiresAt)
	if old.State == entitlements.StateActive {
		t.Errorf("LOST SUPERSEDE: superseded chain OLD is %s alongside NEW (two active chains, both granting access)", old.State)
	}
	// redelivery of NEW cannot repair it: synthetic id already consumed
	if _, err := svc.Process(ctx, n); !errors.Is(err, entitlements.ErrDuplicate) {
		t.Logf("redelivery: %v", err)
	}
	old, _ = st.Get(ctx, entitlements.SourceGoogle, "OLD")
	t.Logf("OLD after redelivery of NEW: %s", old.State)
}

// failOnce fails the Nth Apply call (main event) transiently.
type failStore struct {
	entitlements.Store
	mu    sync.Mutex
	n     int
	failN int
	hook  func(call int)
}

func (f *failStore) Apply(ctx context.Context, ev entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	f.mu.Lock()
	f.n++
	c := f.n
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(c)
	}
	if c == f.failN {
		return nil, errors.New("transient db error")
	}
	return f.Store.Apply(ctx, ev, fn)
}

func TestProbe_SupersedePartialFailureAndRetry(t *testing.T) {
	mem := memory.New()
	ctx := context.Background()
	now := pT0.Add(time.Hour)
	svc0 := entitlements.NewService(mem)
	if _, err := svc0.Process(ctx, pev("o1", "OLD", pT0, entitlements.StateActive)); err != nil {
		t.Fatal(err)
	}
	fs := &failStore{Store: mem, failN: 2}
	svc := entitlements.NewService(fs, entitlements.WithTimeFn(func() time.Time { return now }))
	n := pev("n1", "NEW", pT0.Add(10*time.Minute), entitlements.StateActive)
	n.SupersedesTxID = "OLD"
	_, err := svc.Process(ctx, n)
	act, _ := svc.IsActive(ctx, "u", "p")
	o, _ := mem.Get(ctx, entitlements.SourceGoogle, "OLD")
	_, nerr := mem.Get(ctx, entitlements.SourceGoogle, "NEW")
	t.Logf("after partial failure: err=%v OLD=%s NEW exists=%v user active=%v", err, o.State, nerr == nil, act)
	if !act {
		t.Logf("GAP: user has NO access after main Apply failed (old retired, new not applied) until redelivery succeeds")
	}
	// retry
	if _, err := svc.Process(ctx, n); err != nil {
		t.Fatalf("retry: %v", err)
	}
	act, _ = svc.IsActive(ctx, "u", "p")
	o, _ = mem.Get(ctx, entitlements.SourceGoogle, "OLD")
	nn, _ := mem.Get(ctx, entitlements.SourceGoogle, "NEW")
	t.Logf("after retry: OLD=%s(v%d) NEW=%s(v%d) active=%v", o.State, o.Version, nn.State, nn.Version, act)
	if o.Version != 2 {
		t.Errorf("old chain version %d, want 2 (retired once)", o.Version)
	}
}

// Observer between the two Applies sees neither chain active.
func TestProbe_SupersedeIntermediateStateVisible(t *testing.T) {
	mem := memory.New()
	ctx := context.Background()
	now := pT0.Add(time.Hour)
	var svc *entitlements.Service
	seenInactive := false
	fs := &failStore{Store: mem}
	svc = entitlements.NewService(fs, entitlements.WithTimeFn(func() time.Time { return now }))
	if _, err := svc.Process(ctx, pev("o1", "OLD", pT0, entitlements.StateActive)); err != nil {
		t.Fatal(err)
	}
	fs.n = 0
	fs.hook = func(call int) {
		if call == 2 { // just before the main event's Apply: old already retired
			act, _ := svc.IsActive(ctx, "u", "p")
			seenInactive = !act
		}
	}
	n := pev("n1", "NEW", pT0.Add(10*time.Minute), entitlements.StateActive)
	n.SupersedesTxID = "OLD"
	if _, err := svc.Process(ctx, n); err != nil {
		t.Fatal(err)
	}
	if seenInactive {
		t.Errorf("NON-ATOMIC SUPERSEDE: concurrent IsActive observed user with NO active chain between retire(OLD) and apply(NEW)")
	}
}

// RootCAG3Pool hands out the process-wide pool: callers can mutate it.
func TestProbe_ApplePoolSharedMutable(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.RootPEM())
	v, _ := apple.NewVerifier("com.example.app")
	token := ca.Sign(t, map[string]any{"bundleId": "com.example.app"})
	var out map[string]any
	if err := v.Verify(token, &out); err == nil {
		t.Fatal("test CA must not be trusted by default")
	}
	// someone (a test helper, another library) "just adds" a cert to Apple's pool
	apple.RootCAG3Pool().AppendCertsFromPEM(ca.RootPEM())
	v2, _ := apple.NewVerifier("com.example.app") // plain default verifier
	if err := v2.Verify(token, &out); err == nil {
		t.Errorf("PROCESS-GLOBAL TRUST POLLUTION: default verifier now trusts a foreign CA added through RootCAG3Pool()")
	}
	_ = roots
}

func TestProbe_ApplePoolRace(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	token := ca.Sign(t, map[string]any{"bundleId": "com.example.app"})
	v, _ := apple.NewVerifier("com.example.app")
	var extra [][]byte
	for i := 0; i < 40; i++ {
		extra = append(extra, testutil.NewAppleCA(t).RootPEM())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				var o map[string]any
				_ = v.Verify(token, &o)
			}
		}()
		go func() {
			defer wg.Done()
			for _, pem := range extra {
				apple.RootCAG3Pool().AppendCertsFromPEM(pem)
			}
		}()
	}
	wg.Wait()
}

// Verifier / Parser used from many goroutines, read-only: must be race-free.
func TestProbe_VerifierConcurrentUse(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.RootPEM())
	v, _ := apple.NewVerifier("com.example.app", apple.WithRootPool(roots), apple.WithTimeFn(time.Now))
	token := ca.Sign(t, map[string]any{"bundleId": "com.example.app"})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				var o map[string]any
				if err := v.Verify(token, &o); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
