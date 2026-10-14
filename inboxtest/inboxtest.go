// Package inboxtest is a conformance suite for inbox.Inbox implementations.
// Run it from your store's tests:
//
//	func TestConformance(t *testing.T) {
//		inboxtest.Run(t, func(t *testing.T) inbox.Inbox { return mystore.New(cleanDatabase(t)) })
//	}
//
// Like storetest it uses only the standard library. Every subtest calls
// newInbox for a fresh, empty inbox, and subtests run one after another.
package inboxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/inbox"
)

// base is a whole-second time: stores such as Postgres keep microseconds, so tests avoid
// finer values.
var base = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Run executes the whole suite against inboxes built by newInbox.
func Run(t *testing.T, newInbox func(t *testing.T) inbox.Inbox) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, in inbox.Inbox)
	}{
		{"SaveThenGet", saveThenGet},
		{"SaveStampsReceivedAtAndGeneratesIDs", saveDefaults},
		{"SaveWithIDIsIdempotent", saveIdempotent},
		{"SaveRejectsInvalidItems", saveInvalid},
		{"SaveDropsCredentialHeaders", headersRedacted},
		{"ReturnedValuesDoNotAlias", noAliasing},
		{"GetUnknown", getUnknown},
		{"Mark", mark},
		{"ListFilters", listFilters},
		{"ListOrderAndCursor", listCursor},
		{"Prune", prune},
		{"LargeBody", largeBody},
		{"ConcurrentSaveAndMark", concurrent},
		{"CancelledContext", cancelled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newInbox(t)) })
	}
}

func raw(id string, src entitlements.Source, at time.Time) inbox.Raw {
	return inbox.Raw{
		ID: id, Source: src, Route: inbox.RouteProduction, ReceivedAt: at,
		Body:    []byte(`{"signedPayload":"` + id + `"}`),
		Headers: map[string][]string{"Content-Type": {"application/json"}},
	}
}

func mustSave(t *testing.T, in inbox.Inbox, r inbox.Raw) string {
	t.Helper()
	id, err := in.Save(context.Background(), r)
	if err != nil {
		t.Fatalf("Save(%q): %v", r.ID, err)
	}
	return id
}

func mustGet(t *testing.T, in inbox.Inbox, id string) *inbox.Stored {
	t.Helper()
	s, err := in.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return s
}

func mustMark(t *testing.T, in inbox.Inbox, id string, st inbox.Status, errText string, attempts int) {
	t.Helper()
	if err := in.Mark(context.Background(), id, st, errText, attempts); err != nil {
		t.Fatalf("Mark(%q, %s): %v", id, st, err)
	}
}

func wantErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error = %v, want %v", what, err, want)
	}
}

func ids(items []inbox.Stored) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = s.ID
	}
	return out
}

func sameIDs(got []inbox.Stored, want ...string) bool {
	g := ids(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

func saveThenGet(t *testing.T, in inbox.Inbox) {
	r := raw("n-1", entitlements.SourceGoogle, base)
	r.Route = inbox.RouteReconcile
	r.Body = []byte("{\"message\":{\"data\":\"\\u0000 binary \\xff ok\"}}")
	id := mustSave(t, in, r)
	if id != "n-1" {
		t.Errorf("id = %q, want the one given", id)
	}
	got := mustGet(t, in, id)
	if got.ID != "n-1" || got.Source != entitlements.SourceGoogle || got.Route != inbox.RouteReconcile {
		t.Errorf("stored = %+v", got)
	}
	if !bytes.Equal(got.Body, r.Body) {
		t.Errorf("body = %q, want it byte for byte", got.Body)
	}
	if !got.ReceivedAt.Equal(base) {
		t.Errorf("ReceivedAt = %v, want %v", got.ReceivedAt, base)
	}
	if got.Status != inbox.StatusReceived || got.Attempts != 0 || got.Error != "" {
		t.Errorf("a saved item is received with no attempts and no error: %+v", got)
	}
	if got.Headers["Content-Type"][0] != "application/json" {
		t.Errorf("headers = %v", got.Headers)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

func saveDefaults(t *testing.T, in inbox.Inbox) {
	before := time.Now().Add(-time.Minute)
	r := inbox.Raw{Source: entitlements.SourceApple, Body: []byte("{}")}
	a, b := mustSave(t, in, r), mustSave(t, in, r)
	if a == "" || b == "" || a == b {
		t.Fatalf("generated ids must be non-empty and distinct: %q %q", a, b)
	}
	got := mustGet(t, in, a)
	if got.ReceivedAt.Before(before) || got.ReceivedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("ReceivedAt = %v, want about now", got.ReceivedAt)
	}
	if got.Route == "" {
		t.Error("Route has no default")
	}
}

func saveIdempotent(t *testing.T, in inbox.Inbox) {
	first := raw("same", entitlements.SourceApple, base)
	mustSave(t, in, first)
	mustMark(t, in, "same", inbox.StatusProcessed, "", 1)

	again := raw("same", entitlements.SourceApple, base.Add(time.Hour))
	again.Body = []byte(`{"different":true}`)
	if id := mustSave(t, in, again); id != "same" {
		t.Errorf("second Save returned %q, want the existing id", id)
	}
	got := mustGet(t, in, "same")
	if !bytes.Equal(got.Body, first.Body) || got.Status != inbox.StatusProcessed || got.Attempts != 1 {
		t.Errorf("a repeated Save changed the item: %+v", got)
	}
	if items, err := in.List(context.Background(), inbox.Filter{}); err != nil || len(items) != 1 {
		t.Errorf("List = %d items, %v; want 1", len(items), err)
	}
}

func saveInvalid(t *testing.T, in inbox.Inbox) {
	ctx := context.Background()
	for name, r := range map[string]inbox.Raw{
		"empty body":     {Source: entitlements.SourceApple},
		"unknown source": {Source: "stripe", Body: []byte("x")},
		"no source":      {Body: []byte("x")},
		"oversized body": {Source: entitlements.SourceApple, Body: make([]byte, inbox.MaxBodyBytes+1)},
		"NUL in id":      {ID: "a\x00b", Source: entitlements.SourceApple, Body: []byte("x")},
	} {
		id, err := in.Save(ctx, r)
		wantErr(t, err, inbox.ErrInvalid, name)
		if id != "" {
			t.Errorf("%s: returned id %q", name, id)
		}
	}
	if items, _ := in.List(ctx, inbox.Filter{}); len(items) != 0 {
		t.Errorf("rejected items were stored: %v", ids(items))
	}
}

func headersRedacted(t *testing.T, in inbox.Inbox) {
	r := raw("h", entitlements.SourceGoogle, base)
	r.Headers = map[string][]string{
		"Authorization": {"Bearer secret-token"}, "authorization": {"Bearer other"}, "Cookie": {"session=1"},
		"Proxy-Authorization": {"Basic x"}, "X-Api-Key": {"k"}, "X-Goog-Signature": {"s"},
		"content-type": {"application/json"}, "User-Agent": {"Google-Cloud-Pub/Sub"},
	}
	mustSave(t, in, r)
	got := mustGet(t, in, "h")
	for k, v := range got.Headers {
		switch k {
		case "Content-Type", "User-Agent":
		default:
			t.Errorf("stored header %q = %v: only non-credential headers may be kept", k, v)
		}
	}
	if len(got.Headers["Content-Type"]) != 1 || got.Headers["User-Agent"][0] != "Google-Cloud-Pub/Sub" {
		t.Errorf("harmless headers were lost or not canonicalised: %v", got.Headers)
	}
	if bytes.Contains(got.Body, []byte("secret-token")) {
		t.Error("body unexpectedly contains the token")
	}
	if r.Headers["Authorization"][0] != "Bearer secret-token" {
		t.Error("Save modified the caller's header map")
	}
}

func noAliasing(t *testing.T, in inbox.Inbox) {
	r := raw("alias", entitlements.SourceApple, base)
	body := append([]byte(nil), r.Body...)
	mustSave(t, in, r)
	r.Body[0] = 'X' // the caller reuses its buffer
	r.Headers["Content-Type"][0] = "tampered"
	got := mustGet(t, in, "alias")
	if !bytes.Equal(got.Body, body) || got.Headers["Content-Type"][0] != "application/json" {
		t.Errorf("Save kept references to the caller's data: %q %v", got.Body, got.Headers)
	}
	got.Body[0] = 'Y'
	got.Headers["Content-Type"][0] = "tampered"
	again := mustGet(t, in, "alias")
	if !bytes.Equal(again.Body, body) || again.Headers["Content-Type"][0] != "application/json" {
		t.Error("Get returned the stored slices")
	}
	items, err := in.List(context.Background(), inbox.Filter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %v, %v", items, err)
	}
	items[0].Body[0] = 'Z'
	if !bytes.Equal(mustGet(t, in, "alias").Body, body) {
		t.Error("List returned the stored slices")
	}
}

func getUnknown(t *testing.T, in inbox.Inbox) {
	_, err := in.Get(context.Background(), "nope")
	wantErr(t, err, entitlements.ErrNotFound, "Get unknown")
}

func mark(t *testing.T, in inbox.Inbox) {
	ctx := context.Background()
	mustSave(t, in, raw("m", entitlements.SourceApple, base))
	first := mustGet(t, in, "m")

	mustMark(t, in, "m", inbox.StatusFailed, "store down", 1)
	got := mustGet(t, in, "m")
	if got.Status != inbox.StatusFailed || got.Error != "store down" || got.Attempts != 1 {
		t.Errorf("after Mark = %+v", got)
	}
	if got.UpdatedAt.Before(first.UpdatedAt) {
		t.Errorf("UpdatedAt went back: %v -> %v", first.UpdatedAt, got.UpdatedAt)
	}
	if !bytes.Equal(got.Body, first.Body) || !got.ReceivedAt.Equal(first.ReceivedAt) {
		t.Error("Mark changed the payload or the receive time")
	}

	mustMark(t, in, "m", inbox.StatusFailed, "store down", 1) // idempotent
	if again := mustGet(t, in, "m"); again.Status != inbox.StatusFailed || again.Attempts != 1 {
		t.Errorf("repeated Mark = %+v", again)
	}

	mustMark(t, in, "m", inbox.StatusProcessed, "", 2)
	if done := mustGet(t, in, "m"); done.Status != inbox.StatusProcessed || done.Error != "" || done.Attempts != 2 {
		t.Errorf("final Mark = %+v", done)
	}

	wantErr(t, in.Mark(ctx, "nope", inbox.StatusProcessed, "", 1), entitlements.ErrNotFound, "Mark unknown")
	wantErr(t, in.Mark(ctx, "m", "bogus", "", 1), inbox.ErrInvalid, "Mark invalid status")
	if still := mustGet(t, in, "m"); still.Status != inbox.StatusProcessed {
		t.Errorf("a rejected Mark changed the item: %+v", still)
	}
}

func listFilters(t *testing.T, in inbox.Inbox) {
	ctx := context.Background()
	mustSave(t, in, raw("a1", entitlements.SourceApple, base))
	mustSave(t, in, raw("a2", entitlements.SourceApple, base.Add(time.Hour)))
	mustSave(t, in, raw("g1", entitlements.SourceGoogle, base.Add(2*time.Hour)))
	mustSave(t, in, raw("g2", entitlements.SourceGoogle, base.Add(3*time.Hour)))
	mustMark(t, in, "a1", inbox.StatusProcessed, "", 1)
	mustMark(t, in, "g1", inbox.StatusFailed, "x", 1)

	tests := []struct {
		name string
		f    inbox.Filter
		want []string
	}{
		{"everything", inbox.Filter{}, []string{"a1", "a2", "g1", "g2"}},
		{"status received", inbox.Filter{Status: inbox.StatusReceived}, []string{"a2", "g2"}},
		{"status failed", inbox.Filter{Status: inbox.StatusFailed}, []string{"g1"}},
		{"status with no items", inbox.Filter{Status: inbox.StatusStale}, nil},
		{"source apple", inbox.Filter{Source: entitlements.SourceApple}, []string{"a1", "a2"}},
		{"source google and status received", inbox.Filter{Source: entitlements.SourceGoogle, Status: inbox.StatusReceived}, []string{"g2"}},
		{"since is inclusive", inbox.Filter{Since: base.Add(time.Hour)}, []string{"a2", "g1", "g2"}},
		{"since after everything", inbox.Filter{Since: base.Add(10 * time.Hour)}, nil},
		{"limit", inbox.Filter{Limit: 2}, []string{"a1", "a2"}},
		{"limit larger than the set", inbox.Filter{Limit: 50}, []string{"a1", "a2", "g1", "g2"}},
	}
	for _, tc := range tests {
		got, err := in.List(ctx, tc.f)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !sameIDs(got, tc.want...) {
			t.Errorf("%s: got %v, want %v", tc.name, ids(got), tc.want)
		}
	}
}

func listCursor(t *testing.T, in inbox.Inbox) {
	ctx := context.Background()
	// Several items share one ReceivedAt: the order must still be total (ReceivedAt, ID).
	order := []string{"b1", "b2", "b3", "a-late", "z-late"}
	mustSave(t, in, raw("b3", entitlements.SourceApple, base))
	mustSave(t, in, raw("b1", entitlements.SourceApple, base))
	mustSave(t, in, raw("b2", entitlements.SourceApple, base))
	mustSave(t, in, raw("z-late", entitlements.SourceApple, base.Add(time.Second)))
	mustSave(t, in, raw("a-late", entitlements.SourceApple, base.Add(time.Second)))

	all, err := in.List(ctx, inbox.Filter{})
	if err != nil || !sameIDs(all, order...) {
		t.Fatalf("List = %v, %v; want %v", ids(all), err, order)
	}
	var seen []string
	after := ""
	for page := 0; page < 10; page++ {
		items, err := in.List(ctx, inbox.Filter{Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		seen = append(seen, ids(items)...)
		after = items[len(items)-1].ID
	}
	if fmt.Sprint(seen) != fmt.Sprint(order) {
		t.Errorf("paging with After visited %v, want %v exactly once each", seen, order)
	}
	// A larger tie group, saved in reverse ID order, still pages in ID order.
	var tied []string
	for i := 59; i >= 0; i-- {
		mustSave(t, in, raw(fmt.Sprintf("t-%02d", i), entitlements.SourceGoogle, base.Add(time.Hour)))
	}
	for i := range 60 {
		tied = append(tied, fmt.Sprintf("t-%02d", i))
	}
	var pagedTies []string
	after = ""
	for page := 0; page < 20; page++ {
		items, err := in.List(ctx, inbox.Filter{Source: entitlements.SourceGoogle, Limit: 7, After: after})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		pagedTies = append(pagedTies, ids(items)...)
		after = items[len(items)-1].ID
	}
	if fmt.Sprint(pagedTies) != fmt.Sprint(tied) {
		t.Errorf("paging a group with one ReceivedAt gave %v, want ascending IDs", pagedTies)
	}
	// After combines with the other filters
	mustMark(t, in, "b2", inbox.StatusProcessed, "", 1)
	got, err := in.List(ctx, inbox.Filter{After: "b1", Status: inbox.StatusReceived, Source: entitlements.SourceApple})
	if err != nil || !sameIDs(got, "b3", "a-late", "z-late") {
		t.Errorf("After + Status = %v, %v", ids(got), err)
	}
	_, err = in.List(ctx, inbox.Filter{After: "unknown"})
	wantErr(t, err, entitlements.ErrNotFound, "After an unknown id")
}

func prune(t *testing.T, in inbox.Inbox) {
	ctx := context.Background()
	for _, c := range []struct {
		id string
		st inbox.Status
		at time.Time
	}{
		{"old-processed", inbox.StatusProcessed, base},
		{"old-ignored", inbox.StatusIgnored, base},
		{"old-invalid", inbox.StatusInvalidSignature, base},
		{"old-received", inbox.StatusReceived, base},
		{"old-failed", inbox.StatusFailed, base},
		{"edge-processed", inbox.StatusProcessed, base.Add(24 * time.Hour)},
		{"new-processed", inbox.StatusProcessed, base.Add(48 * time.Hour)},
	} {
		mustSave(t, in, raw(c.id, entitlements.SourceApple, c.at))
		if c.st != inbox.StatusReceived {
			mustMark(t, in, c.id, c.st, "", 1)
		}
	}
	n, err := in.Prune(ctx, base.Add(24*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("Prune = %d, %v; want 3 old final items", n, err)
	}
	left, _ := in.List(ctx, inbox.Filter{})
	if !sameIDs(left, "old-failed", "old-received", "edge-processed", "new-processed") {
		t.Errorf("left = %v: retryable items and items at or after the cutoff must stay", ids(left))
	}
	if n, err = in.Prune(ctx, base.Add(24*time.Hour)); err != nil || n != 0 {
		t.Errorf("second Prune = %d, %v; want 0", n, err)
	}
}

func largeBody(t *testing.T, in inbox.Inbox) {
	// exactly the limit is accepted
	edge := raw("edge", entitlements.SourceApple, base)
	edge.Body = make([]byte, inbox.MaxBodyBytes)
	mustSave(t, in, edge)
	if got := mustGet(t, in, "edge"); len(got.Body) != inbox.MaxBodyBytes {
		t.Errorf("a body of exactly MaxBodyBytes came back with %d bytes", len(got.Body))
	}

	r := raw("big", entitlements.SourceApple, base)
	r.Body = make([]byte, 3<<20)
	for i := range r.Body {
		r.Body[i] = byte(i*7 + i>>8)
	}
	mustSave(t, in, r)
	if got := mustGet(t, in, "big"); !bytes.Equal(got.Body, r.Body) {
		t.Errorf("a %d byte body did not round trip (got %d bytes)", len(r.Body), len(got.Body))
	}
}

func concurrent(t *testing.T, in inbox.Inbox) {
	ctx := context.Background()
	const n = 40
	idsOut := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := in.Save(ctx, inbox.Raw{Source: entitlements.SourceApple, Body: []byte(fmt.Sprint("body-", i)), ReceivedAt: base.Add(time.Duration(i) * time.Second)})
			if err != nil {
				t.Errorf("Save: %v", err)
				return
			}
			idsOut[i] = id
			if err := in.Mark(ctx, id, inbox.StatusProcessed, "", 1); err != nil {
				t.Errorf("Mark: %v", err)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range idsOut {
		if id == "" || seen[id] {
			t.Fatalf("ids are not unique: %v", idsOut)
		}
		seen[id] = true
	}
	items, err := in.List(ctx, inbox.Filter{Limit: 1000})
	if err != nil || len(items) != n {
		t.Fatalf("List = %d items, %v; want %d", len(items), err, n)
	}
	for _, s := range items {
		if s.Status != inbox.StatusProcessed || s.Attempts != 1 {
			t.Errorf("%s = %s/%d", s.ID, s.Status, s.Attempts)
		}
	}

	// the same id saved by many goroutines at once: one item
	var wg2 sync.WaitGroup
	for range 20 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if id, err := in.Save(ctx, raw("dup", entitlements.SourceApple, base)); err != nil || id != "dup" {
				t.Errorf("Save dup = %q, %v", id, err)
			}
		}()
	}
	wg2.Wait()
	if dups, _ := in.List(ctx, inbox.Filter{Since: base.Add(-time.Hour), Limit: 1000}); len(dups) != n+1 {
		t.Errorf("%d items after concurrent saves of one id, want %d", len(dups), n+1)
	}
}

func cancelled(t *testing.T, in inbox.Inbox) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := in.Save(ctx, raw("c", entitlements.SourceApple, base)); err == nil {
		t.Error("Save with a cancelled context must fail")
	}
	if _, err := in.Get(ctx, "c"); err == nil {
		t.Error("Get with a cancelled context must fail")
	}
	// the cancelled Save must not have left an item behind
	if _, err := in.Get(context.Background(), "c"); !errors.Is(err, entitlements.ErrNotFound) {
		t.Errorf("Get after a cancelled Save = %v, want ErrNotFound", err)
	}
}
