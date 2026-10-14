package serverapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

const hour = int64(3600_000)

func renewTx(id string, expires int64) map[string]any {
	return tx(id, map[string]any{"expiresDate": expires, "appAccountToken": "7e3fb20b-4cdb-47cc-936d-99d65f608138"})
}

// replayFixture serves a notification history of three notifications for one
// chain, newest first and split over two pages the way an unordered listing
// might arrive.
func replayFixture(t *testing.T, f *fakeApple) {
	t.Helper()
	sub := notificationSigned(t, f.ca, "SUBSCRIBED", "INITIAL_BUY", "uuid-1", 1791633600000-2*hour, renewTx("t1", 1791633600000+10*hour), 1)
	renew := notificationSigned(t, f.ca, "DID_RENEW", "", "uuid-2", 1791633600000-hour, renewTx("t2", 1791633600000+40*hour), 1)
	exp := notificationSigned(t, f.ca, "EXPIRED", "VOLUNTARY", "uuid-3", 1791633600000, renewTx("t2", 1791633600000+40*hour), 2)
	f.script(
		ok(map[string]any{"notificationHistory": []map[string]any{historyRecord(exp), historyRecord(sub)}, "hasMore": true, "paginationToken": "p2"}),
		ok(map[string]any{"notificationHistory": []map[string]any{historyRecord(renew)}, "hasMore": false}),
	)
}

func replayWindow() (time.Time, time.Time) {
	return snapshotAt.Add(-72 * time.Hour), snapshotAt.Add(time.Hour)
}

func TestReplay_RepairsMissedNotificationsInOrder(t *testing.T) {
	f := newFakeApple(t)
	replayFixture(t, f)
	c, _ := f.client()
	svc, _ := newService()
	start, end := replayWindow()

	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(f.verifier(entitlements.EnvironmentSandbox)), svc, start, end)
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Total)
	assert.Equal(t, 3, rep.Applied, "oldest first, so every step applies instead of the newest making the rest stale")
	assert.Zero(t, rep.Stale+rep.Duplicate+rep.Failed)

	ent, err := svc.Get(ctx, entitlements.SourceApple, origTx)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, ent.State)
	assert.EqualValues(t, 3, ent.Version)

	var body map[string]any
	require.NoError(t, json.Unmarshal(f.requests()[0].Body, &body))
	assert.EqualValues(t, start.UnixMilli(), body["startDate"])
	assert.EqualValues(t, end.UnixMilli(), body["endDate"])
}

func TestReplay_IsIdempotentAndDedupesAgainstLiveWebhooks(t *testing.T) {
	f := newFakeApple(t)
	replayFixture(t, f)
	c, _ := f.client()
	svc, _ := newService()
	parser := apple.NewParser(f.verifier(entitlements.EnvironmentSandbox))
	start, end := replayWindow()

	// uuid-1 already arrived as a live webhook.
	sub := notificationSigned(t, f.ca, "SUBSCRIBED", "INITIAL_BUY", "uuid-1", 1791633600000-2*hour, renewTx("t1", 1791633600000+10*hour), 1)
	body, err := json.Marshal(apple.ResponseBodyV2{SignedPayload: sub})
	require.NoError(t, err)
	ev, err := parser.Parse(body)
	require.NoError(t, err)
	_, err = svc.Process(ctx, ev)
	require.NoError(t, err)

	rep, err := ReplayNotificationHistory(ctx, c, parser, svc, start, end)
	require.NoError(t, err)
	assert.Equal(t, ReplayReport{Total: 3, Applied: 2, Duplicate: 1}, rep)

	replayFixture(t, f) // same history again
	rep, err = ReplayNotificationHistory(ctx, c, parser, svc, start, end)
	require.NoError(t, err)
	assert.Equal(t, ReplayReport{Total: 3, Duplicate: 3}, rep, "a second replay changes nothing")
}

func TestReplay_CannotRollBackNewerState(t *testing.T) {
	f := newFakeApple(t)
	replayFixture(t, f)
	c, _ := f.client()
	svc, _ := newService()
	// A reconcile already moved the chain past every historic notification.
	_, err := svc.Process(ctx, entitlements.Event{
		ID: "reconcile-x", Source: entitlements.SourceApple, OriginalTxID: origTx, State: entitlements.StateActive,
		ExpiresAt: snapshotAt.Add(30 * 24 * time.Hour), OccurredAt: snapshotAt.Add(time.Minute), Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)
	start, end := replayWindow()

	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(f.verifier(entitlements.EnvironmentSandbox)), svc, start, end)
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Stale)
	ent, _ := svc.Get(ctx, entitlements.SourceApple, origTx)
	assert.Equal(t, entitlements.StateActive, ent.State)
}

func TestReplay_NonStateNotificationsAreIgnored(t *testing.T) {
	f := newFakeApple(t)
	test := notificationSigned(t, f.ca, "TEST", "", "uuid-test", 1791633600000, nil, 0)
	price := notificationSigned(t, f.ca, "PRICE_INCREASE", "ACCEPTED", "uuid-price", 1791633600000, renewTx("t1", 1794268800000), 1)
	f.respond(ok(map[string]any{"notificationHistory": []map[string]any{historyRecord(test), historyRecord(price)}}))
	c, _ := f.client()
	svc, _ := newService()
	start, end := replayWindow()

	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(f.verifier(entitlements.EnvironmentSandbox)), svc, start, end)
	require.NoError(t, err)
	assert.Equal(t, ReplayReport{Total: 2, Ignored: 2}, rep)
}

func TestReplay_UnverifiableRecordsFailLoudlyButDoNotBlockTheRest(t *testing.T) {
	f := newFakeApple(t)
	rogue := testutil.NewAppleCA(t)
	good := notificationSigned(t, f.ca, "SUBSCRIBED", "INITIAL_BUY", "uuid-1", 1791633600000, renewTx("t1", 1794268800000), 1)
	forged := notificationSigned(t, rogue, "EXPIRED", "VOLUNTARY", "uuid-forged", 1791633600000+hour, renewTx("t1", 1794268800000), 2)
	f.respond(ok(map[string]any{"notificationHistory": []map[string]any{historyRecord(forged), historyRecord(good)}}))
	c, _ := f.client()
	svc, _ := newService()
	start, end := replayWindow()

	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(f.verifier(entitlements.EnvironmentSandbox)), svc, start, end)
	require.Error(t, err)
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Equal(t, 2, rep.Total)
	assert.Equal(t, 1, rep.Applied)
	assert.Equal(t, 1, rep.Failed)
	require.Len(t, rep.Errors, 1)
	ent, _ := svc.Get(ctx, entitlements.SourceApple, origTx)
	assert.Equal(t, entitlements.StateActive, ent.State, "the forged EXPIRED did not apply")
}

// A parser with its own clock can read records the Client's Verifier cannot
// (an old signing certificate); the Client's VerifyErr must not stop it.
func TestReplay_ParserDecidesNotTheClientsVerifier(t *testing.T) {
	f := newFakeApple(t)
	other := testutil.NewAppleCA(t) // the Client does not trust this CA...
	signed := notificationSigned(t, other, "SUBSCRIBED", "INITIAL_BUY", "uuid-1", 1791633600000, renewTx("t1", 1794268800000), 1)
	f.respond(ok(map[string]any{"notificationHistory": []map[string]any{historyRecord(signed)}}))
	c, _ := f.client()
	svc, _ := newService()
	v, err := apple.NewVerifier(testBundle, apple.WithRootPool(other.Pool)) // ...but the parser does
	require.NoError(t, err)
	start, end := replayWindow()

	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(v), svc, start, end)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Applied)
}

func TestReplay_Options(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"notificationHistory": []any{}}))
	c, _ := f.client()
	svc, _ := newService()
	start, end := replayWindow()
	parser := apple.NewParser(f.verifier(entitlements.EnvironmentSandbox))

	_, err := ReplayNotificationHistory(ctx, c, parser, svc, start, end, ReplayOnlyFailures(), ReplayTransaction("T1"), nil)
	require.NoError(t, err)
	_, err = ReplayNotificationHistory(ctx, c, parser, svc, start, end, ReplayType("DID_RENEW", "BILLING_RECOVERY"))
	require.NoError(t, err)

	reqs := f.requests()
	var b1, b2 map[string]any
	require.NoError(t, json.Unmarshal(reqs[0].Body, &b1))
	require.NoError(t, json.Unmarshal(reqs[1].Body, &b2))
	assert.Equal(t, true, b1["onlyFailures"])
	assert.Equal(t, "T1", b1["transactionId"])
	assert.Equal(t, "DID_RENEW", b2["notificationType"])
	assert.Equal(t, "BILLING_RECOVERY", b2["notificationSubtype"])
	assert.NotContains(t, b2, "onlyFailures")
}

func TestReplay_HistoryErrorsAreReturned(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(400, 4000012, "Invalid request. The start date is earlier than the allowed start date."))
	c, _ := f.client()
	svc, _ := newService()
	start, end := replayWindow()
	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(f.verifier(entitlements.EnvironmentSandbox)), svc, start, end)
	require.ErrorIs(t, err, ErrBadRequest)
	assert.ErrorContains(t, err, "read notification history")
	assert.Zero(t, rep.Total)
}

func TestReplay_StoreFailureIsReported(t *testing.T) {
	f := newFakeApple(t)
	replayFixture(t, f)
	c, _ := f.client()
	start, end := replayWindow()
	app := &stubApplier{processErr: assert.AnError}
	rep, err := ReplayNotificationHistory(ctx, c, apple.NewParser(f.verifier(entitlements.EnvironmentSandbox)), app, start, end)
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, 3, rep.Failed)
}
