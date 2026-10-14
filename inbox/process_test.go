package inbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/inbox"
	"github.com/tggo/go-entitlements/inbox/memory"
	mocksent "github.com/tggo/go-entitlements/internal/mocks/entitlements"
	mocksgoogle "github.com/tggo/go-entitlements/internal/mocks/google"
	mocksinbox "github.com/tggo/go-entitlements/internal/mocks/inbox"
	"github.com/tggo/go-entitlements/internal/testutil"
	storemem "github.com/tggo/go-entitlements/store/memory"
)

type rig struct {
	t     *testing.T
	ca    *testutil.AppleCA
	in    *memory.Inbox
	store *storemem.Store
	svc   *entitlements.Service
	p     inbox.Processors
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ca := testutil.NewAppleCA(t)
	v, err := apple.NewVerifier("com.example.app", apple.WithRootPool(ca.Pool))
	require.NoError(t, err)
	st := storemem.New()
	svc := entitlements.NewService(st, entitlements.WithLogger(testutil.NewLogger(t)))
	r := &rig{t: t, ca: ca, in: memory.New(), store: st, svc: svc}
	r.p = inbox.Processors{Apple: apple.NewParser(v), Service: svc}
	return r
}

func (r *rig) appleBody(name string, mut func(f *testutil.AppleFixture)) []byte {
	r.t.Helper()
	f := testutil.LoadAppleFixture(r.t, name)
	if mut != nil {
		mut(&f)
	}
	return f.SignBody(r.t, r.ca)
}

func (r *rig) save(src entitlements.Source, body []byte) string {
	r.t.Helper()
	id, err := r.in.Save(context.Background(), inbox.Raw{Source: src, Body: body})
	require.NoError(r.t, err)
	return id
}

func (r *rig) run(id string) (inbox.Status, error) {
	r.t.Helper()
	return inbox.ProcessStored(context.Background(), r.in, id, r.p)
}

func (r *rig) item(id string) *inbox.Stored {
	r.t.Helper()
	s, err := r.in.Get(context.Background(), id)
	require.NoError(r.t, err)
	return s
}

func at(ms int64) func(f *testutil.AppleFixture) {
	return func(f *testutil.AppleFixture) { f.Notification["signedDate"] = float64(ms) }
}

func TestProcessStored_Apple(t *testing.T) {
	t.Parallel()
	t.Run("a good notification is processed and the entitlement exists", func(t *testing.T) {
		r := newRig(t)
		id := r.save(entitlements.SourceApple, r.appleBody("subscribed", nil))
		st, err := r.run(id)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusProcessed, st)
		got := r.item(id)
		assert.Equal(t, inbox.StatusProcessed, got.Status)
		assert.Equal(t, 1, got.Attempts)
		assert.Empty(t, got.Error)
		e, err := r.svc.Get(context.Background(), entitlements.SourceApple, "2000000000001")
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateActive, e.State)
	})

	t.Run("the same notification saved twice is a duplicate the second time", func(t *testing.T) {
		r := newRig(t)
		body := r.appleBody("subscribed", nil)
		first, second := r.save(entitlements.SourceApple, body), ""
		var err error
		second, err = r.in.Save(context.Background(), inbox.Raw{ID: "redelivery", Source: entitlements.SourceApple, Body: body})
		require.NoError(t, err)
		_, err = r.run(first)
		require.NoError(t, err)
		st, err := r.run(second)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusDuplicate, st)
	})

	t.Run("an older notification is stale", func(t *testing.T) {
		r := newRig(t)
		newer := r.save(entitlements.SourceApple, r.appleBody("subscribed", at(1760000010000)))
		older := r.save(entitlements.SourceApple, r.appleBody("did_renew", at(1760000000000)))
		_, err := r.run(newer)
		require.NoError(t, err)
		st, err := r.run(older)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusStale, st)
	})

	t.Run("outcomes without a retry", func(t *testing.T) {
		tests := []struct {
			name string
			body func(r *rig) []byte
			want inbox.Status
		}{
			{"TEST notification is ignored", func(r *rig) []byte { return r.appleBody("test", nil) }, inbox.StatusIgnored},
			{"price increase is ignored", func(r *rig) []byte { return r.appleBody("price_increase", nil) }, inbox.StatusIgnored},
			{"unknown type is unsupported", func(r *rig) []byte {
				return r.appleBody("subscribed", func(f *testutil.AppleFixture) { f.Notification["notificationType"] = "BRAND_NEW" })
			}, inbox.StatusUnsupported},
			{"granting event without expiry is unsupported", func(r *rig) []byte {
				return r.appleBody("subscribed", func(f *testutil.AppleFixture) { delete(f.Transaction, "expiresDate") })
			}, inbox.StatusUnsupported},
			{"sandbox event on a production service is ignored", func(r *rig) []byte {
				return r.appleBody("subscribed", func(f *testutil.AppleFixture) {
					f.Data()["environment"], f.Transaction["environment"], f.Renewal["environment"] = "Sandbox", "Sandbox", "Sandbox"
				})
			}, inbox.StatusIgnored},
			{"tampered signature", func(r *rig) []byte {
				b := r.appleBody("subscribed", nil)
				var rb apple.ResponseBodyV2
				require.NoError(t, json.Unmarshal(b, &rb))
				rb.SignedPayload = rb.SignedPayload[:len(rb.SignedPayload)-4] + "AAAA"
				out, _ := json.Marshal(rb)
				return out
			}, inbox.StatusInvalidSignature},
			{"not json", func(*rig) []byte { return []byte("not json") }, inbox.StatusInvalidSignature},
			{"wrong bundle", func(r *rig) []byte {
				return r.appleBody("subscribed", func(f *testutil.AppleFixture) {
					f.Data()["bundleId"], f.Transaction["bundleId"] = "com.other", "com.other"
				})
			}, inbox.StatusInvalidSignature},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				r := newRig(t)
				id := r.save(entitlements.SourceApple, tc.body(r))
				st, err := r.run(id)
				require.NoError(t, err, "a final outcome is not an error: retrying cannot help")
				assert.Equal(t, tc.want, st)
				got := r.item(id)
				assert.Equal(t, tc.want, got.Status)
				assert.Equal(t, 1, got.Attempts)
				assert.True(t, got.Status.Final())
				if tc.want != inbox.StatusProcessed {
					assert.NotEmpty(t, got.Error, "the reason is recorded")
				}
			})
		}
	})
}

func TestProcessStored_RetryableFailureThenSuccess(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	boom := errors.New("database is down")
	failing := mocksent.NewMockStore(t)
	failing.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything).Return(nil, boom)
	r.p.Service = entitlements.NewService(failing)
	id := r.save(entitlements.SourceApple, r.appleBody("subscribed", nil))

	st, err := r.run(id)
	require.ErrorIs(t, err, boom, "a transient error is returned so the worker retries")
	assert.Equal(t, inbox.StatusFailed, st)
	got := r.item(id)
	assert.Equal(t, inbox.StatusFailed, got.Status)
	assert.Equal(t, 1, got.Attempts)
	assert.Contains(t, got.Error, "database is down")
	assert.False(t, got.Status.Final())

	r.p.Service = r.svc // the database is back
	st, err = r.run(id)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusProcessed, st)
	got = r.item(id)
	assert.Equal(t, 2, got.Attempts)
	assert.Empty(t, got.Error, "a success clears the error")
}

func TestProcessStored_FinalItemsAreNotProcessedAgain(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.save(entitlements.SourceApple, r.appleBody("subscribed", nil))
	_, err := r.run(id)
	require.NoError(t, err)

	untouched := mocksinbox.NewMockProcessor(t) // no expectations: any call fails the test
	r.p.Service = untouched
	r.p.Apple = mocksinbox.NewMockAppleParser(t)
	st, err := r.run(id)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusProcessed, st)
	assert.Equal(t, 1, r.item(id).Attempts, "attempts do not move")
}

func TestProcessStored_Errors(t *testing.T) {
	t.Parallel()
	t.Run("unknown id", func(t *testing.T) {
		r := newRig(t)
		_, err := r.run("nope")
		require.ErrorIs(t, err, entitlements.ErrNotFound)
	})
	t.Run("no parser for the source is retryable once configured", func(t *testing.T) {
		r := newRig(t)
		r.p.Google = nil
		id := r.save(entitlements.SourceGoogle, []byte(`{"message":{}}`))
		st, err := r.run(id)
		require.ErrorIs(t, err, inbox.ErrNoProcessor)
		assert.Equal(t, inbox.StatusFailed, st)
		assert.Equal(t, inbox.StatusFailed, r.item(id).Status)
	})
	t.Run("no service", func(t *testing.T) {
		r := newRig(t)
		r.p.Service = nil
		id := r.save(entitlements.SourceApple, r.appleBody("subscribed", nil))
		_, err := r.run(id)
		require.ErrorIs(t, err, inbox.ErrNoProcessor)
	})
	t.Run("a Mark failure after processing is returned and the re-run is safe", func(t *testing.T) {
		r := newRig(t)
		id := r.save(entitlements.SourceApple, r.appleBody("subscribed", nil))
		boom := errors.New("inbox write failed")
		flaky := mocksinbox.NewMockInbox(t)
		flaky.EXPECT().Get(mock.Anything, id).Return(r.item(id), nil)
		flaky.EXPECT().Mark(mock.Anything, id, inbox.StatusProcessed, "", 1).Return(boom)
		_, err := inbox.ProcessStored(context.Background(), flaky, id, r.p)
		require.ErrorIs(t, err, boom)

		st, err := r.run(id) // the real inbox still says "received": process again
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusDuplicate, st, "the entitlement was already applied")
	})
	t.Run("a cancelled context is returned", func(t *testing.T) {
		r := newRig(t)
		id := r.save(entitlements.SourceApple, r.appleBody("subscribed", nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := inbox.ProcessStored(ctx, r.in, id, r.p)
		require.Error(t, err)
	})
}

// ---- Google

func googleEvent(id, tx string, at time.Time) entitlements.Event {
	return entitlements.Event{
		ID: id, Source: entitlements.SourceGoogle, UserID: "u1", OriginalTxID: tx, ProductID: "pro",
		State: entitlements.StateActive, ExpiresAt: at.Add(time.Hour), OccurredAt: at,
	}
}

var gAck = &google.Ack{PackageName: "com.example.app", SubscriptionID: "premium.monthly", PurchaseToken: "tok-1"}

func googleRig(t *testing.T, res google.Result, parseErr error) (*rig, *mocksgoogle.MockAcknowledger, string) {
	t.Helper()
	r := newRig(t)
	gp := mocksinbox.NewMockGoogleParser(t)
	gp.EXPECT().ParseResult(mock.Anything, mock.Anything).Return(res, parseErr)
	ack := mocksgoogle.NewMockAcknowledger(t)
	r.p.Google, r.p.Acknowledger = gp, ack
	return r, ack, r.save(entitlements.SourceGoogle, []byte(`{"message":{"messageId":"m1"}}`))
}

func TestProcessStored_GoogleAcknowledgesAfterStoring(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	res := google.Result{Events: []entitlements.Event{googleEvent("m1", "tok-1", now)}, Ack: gAck}

	t.Run("acknowledged once the event is stored", func(t *testing.T) {
		r, ack, id := googleRig(t, res, nil)
		ack.EXPECT().Acknowledge(mock.Anything, "com.example.app", "premium.monthly", "tok-1").Return(nil).Once()
		st, err := r.run(id)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusProcessed, st)
	})
	t.Run("a transient acknowledge failure is retried, and the re-run acknowledges again", func(t *testing.T) {
		r, ack, id := googleRig(t, res, nil)
		ack.EXPECT().Acknowledge(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(google.ErrFetchTransient).Once()
		st, err := r.run(id)
		require.ErrorIs(t, err, google.ErrFetchTransient)
		assert.Equal(t, inbox.StatusFailed, st)
		assert.Equal(t, 1, r.item(id).Attempts)

		ack.EXPECT().Acknowledge(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
		st, err = r.run(id)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusDuplicate, st, "the event was stored by the first run")
		assert.Equal(t, 2, r.item(id).Attempts)
	})
	t.Run("a configuration error from the Play API is retryable", func(t *testing.T) {
		r, ack, id := googleRig(t, res, nil)
		ack.EXPECT().Acknowledge(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(google.ErrFetchConfig).Once()
		st, err := r.run(id)
		require.ErrorIs(t, err, google.ErrFetchConfig)
		assert.Equal(t, inbox.StatusFailed, st)
	})
	for _, final := range []error{google.ErrAlreadyAcknowledged, google.ErrPurchaseNotFound, google.ErrPurchaseInvalid} {
		t.Run("permanent acknowledge outcome is final: "+final.Error(), func(t *testing.T) {
			r, ack, id := googleRig(t, res, nil)
			ack.EXPECT().Acknowledge(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(final).Once()
			st, err := r.run(id)
			require.NoError(t, err)
			assert.Equal(t, inbox.StatusProcessed, st)
		})
	}
	t.Run("no acknowledger configured: nothing is acknowledged", func(t *testing.T) {
		r, _, id := googleRig(t, res, nil)
		r.p.Acknowledger = nil
		st, err := r.run(id)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusProcessed, st)
	})
	t.Run("no Ack requested: nothing is acknowledged", func(t *testing.T) {
		r, _, id := googleRig(t, google.Result{Events: res.Events}, nil)
		st, err := r.run(id)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusProcessed, st)
	})
	t.Run("no acknowledge when an event did not store", func(t *testing.T) {
		r, _, id := googleRig(t, res, nil)
		boom := errors.New("store down")
		failing := mocksent.NewMockStore(t)
		failing.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything).Return(nil, boom)
		r.p.Service = entitlements.NewService(failing)
		st, err := r.run(id)
		require.ErrorIs(t, err, boom)
		assert.Equal(t, inbox.StatusFailed, st)
	})
	t.Run("an unsupported event is not acknowledged", func(t *testing.T) {
		unsupported := res
		unsupported.Events = []entitlements.Event{{ID: "m1", Source: entitlements.SourceGoogle, OriginalTxID: "tok-1", State: entitlements.StateActive}}
		r, _, id := googleRig(t, unsupported, nil)
		unsupportedSvc := mocksinbox.NewMockProcessor(t)
		unsupportedSvc.EXPECT().Process(mock.Anything, mock.Anything).Return(nil, entitlements.ErrUnsupported)
		r.p.Service = unsupportedSvc
		st, err := r.run(id)
		require.NoError(t, err)
		assert.Equal(t, inbox.StatusUnsupported, st)
	})
}

func TestProcessStored_GoogleParseOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		want    inbox.Status
		wantErr bool
	}{
		{"test notification", google.ErrTestNotification, inbox.StatusIgnored, false},
		{"ignored", entitlements.ErrIgnored, inbox.StatusIgnored, false},
		{"another package", google.ErrPackageMismatch, inbox.StatusIgnored, false},
		{"unsupported", entitlements.ErrUnsupported, inbox.StatusUnsupported, false},
		{"granting subscription without expiry", google.ErrNoExpiry, inbox.StatusUnsupported, false},
		{"purchase not found", google.ErrPurchaseNotFound, inbox.StatusUnsupported, false},
		{"purchase rejected by the Play API", google.ErrPurchaseInvalid, inbox.StatusUnsupported, false},
		{"malformed push", google.ErrMalformed, inbox.StatusInvalidSignature, false},
		{"invalid signature", entitlements.ErrInvalidSignature, inbox.StatusInvalidSignature, false},
		{"Play API credentials", google.ErrFetchConfig, inbox.StatusFailed, true},
		{"Play API outage", google.ErrFetchTransient, inbox.StatusFailed, true},
		{"anything unexpected is retryable", errors.New("weird"), inbox.StatusFailed, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _, id := googleRig(t, google.Result{}, tc.err)
			st, err := r.run(id)
			if tc.wantErr {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, st)
			assert.Equal(t, tc.want, r.item(id).Status)
			assert.Contains(t, r.item(id).Error, tc.err.Error())
		})
	}
}

func TestProcessStored_GoogleAddOnsApplyEveryEvent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	boom := errors.New("store down")
	e1, e2, e3 := googleEvent("m1", "tok-1", now), googleEvent("m1#2", "tok-1", now), googleEvent("m1#3", "tok-1", now)
	r, ack, id := googleRig(t, google.Result{Events: []entitlements.Event{e1, e2, e3}, Ack: gAck}, nil)
	svc := mocksinbox.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, e1).Return(&entitlements.Entitlement{}, nil)
	svc.EXPECT().Process(mock.Anything, e2).Return(nil, boom) // a failing event does not stop the others
	svc.EXPECT().Process(mock.Anything, e3).Return(nil, entitlements.ErrDuplicate)
	r.p.Service = svc

	st, err := r.run(id)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, inbox.StatusFailed, st)
	_ = ack // no acknowledge expected: not every event is stored
}

func TestProcessStored_AggregateStatusPrecedence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	outcomes := map[string]error{
		"applied": nil, "duplicate": entitlements.ErrDuplicate, "stale": entitlements.ErrStale,
		"unsupported": entitlements.ErrUnsupported, "ignored": entitlements.ErrIgnored, "invalid": entitlements.ErrInvalidEvent,
	}
	tests := []struct {
		events []string
		want   inbox.Status
	}{
		{[]string{"applied", "duplicate"}, inbox.StatusProcessed},
		{[]string{"duplicate", "stale"}, inbox.StatusStale},
		{[]string{"duplicate", "duplicate"}, inbox.StatusDuplicate},
		{[]string{"duplicate", "unsupported"}, inbox.StatusDuplicate},
		{[]string{"unsupported", "ignored"}, inbox.StatusUnsupported},
		{[]string{"ignored", "ignored"}, inbox.StatusIgnored},
		{[]string{"invalid"}, inbox.StatusUnsupported},
	}
	for _, tc := range tests {
		t.Run(string(tc.want)+"/"+tc.events[0]+"+"+tc.events[len(tc.events)-1], func(t *testing.T) {
			var evs []entitlements.Event
			for i := range tc.events {
				evs = append(evs, googleEvent("e"+string(rune('a'+i)), "tok", now))
			}
			r, _, id := googleRig(t, google.Result{Events: evs}, nil)
			svc := mocksinbox.NewMockProcessor(t)
			for i, name := range tc.events {
				if outcomes[name] == nil {
					svc.EXPECT().Process(mock.Anything, evs[i]).Return(&entitlements.Entitlement{}, nil)
				} else {
					svc.EXPECT().Process(mock.Anything, evs[i]).Return(nil, outcomes[name])
				}
			}
			r.p.Service = svc
			st, err := r.run(id)
			require.NoError(t, err)
			assert.Equal(t, tc.want, st)
			if tc.want == inbox.StatusProcessed {
				assert.Empty(t, r.item(id).Error, "a processed item carries no error text even if one event was a duplicate")
			}
		})
	}
}

// The inbox reaches the real parser and service end to end.
func TestProcessStored_GoogleWithTheRealParser(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fetcher := mocksgoogle.NewMockSubscriptionsFetcher(t)
	var sub google.SubscriptionPurchaseV2
	require.NoError(t, json.Unmarshal(testutil.GoogleFixture(t, "sub_state_active.json"), &sub))
	sub.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_PENDING"
	fetcher.EXPECT().GetV2(mock.Anything, "com.example.app", "tok-1").Return(&sub, nil)
	parser, err := google.NewParser(fetcher, "com.example.app")
	require.NoError(t, err)
	ack := mocksgoogle.NewMockAcknowledger(t)
	ack.EXPECT().Acknowledge(mock.Anything, "com.example.app", "premium.monthly", "tok-1").Return(nil).Once()
	r.p.Google, r.p.Acknowledger = parser, ack

	id := r.save(entitlements.SourceGoogle, testutil.GooglePushBody(t, "msg-1", testutil.GoogleFixture(t, "notif_subscription.json")))
	st, err := r.run(id)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusProcessed, st)
	e, err := r.svc.Get(context.Background(), entitlements.SourceGoogle, "tok-1")
	require.NoError(t, err)
	assert.Equal(t, "user-1", e.UserID)
}

func TestStatus(t *testing.T) {
	t.Parallel()
	for _, s := range []inbox.Status{inbox.StatusReceived, inbox.StatusFailed} {
		assert.False(t, s.Final(), s)
		assert.True(t, s.Valid(), s)
	}
	for _, s := range []inbox.Status{inbox.StatusProcessed, inbox.StatusDuplicate, inbox.StatusStale, inbox.StatusIgnored, inbox.StatusUnsupported, inbox.StatusInvalidSignature} {
		assert.True(t, s.Final(), s)
		assert.True(t, s.Valid(), s)
	}
	assert.False(t, inbox.Status("bogus").Valid())
	assert.False(t, inbox.Status("").Valid())
}

func TestSafeHeaders(t *testing.T) {
	t.Parallel()
	got := inbox.SafeHeaders(map[string][]string{"authorization": {"x"}, "COOKIE": {"y"}, "content-type": {"a"}, "X-Request-Id": {"1"}})
	assert.Equal(t, map[string][]string{"Content-Type": {"a"}, "X-Request-Id": {"1"}}, got)
	assert.Nil(t, inbox.SafeHeaders(nil))
	assert.Nil(t, inbox.SafeHeaders(map[string][]string{"Authorization": {"x"}}))
}

func TestProcessStored_NoEventsIsIgnored(t *testing.T) {
	t.Parallel()
	r, _, id := googleRig(t, google.Result{}, nil)
	st, err := r.run(id)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusIgnored, st)
	assert.NotEmpty(t, r.item(id).Error)
}

// A sandbox purchase is ignored by a production Service, but it is still a
// real Google purchase that must be acknowledged or Google refunds it.
func TestProcessStored_SandboxEventIsStillAcknowledged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ev := googleEvent("m1", "tok-1", now)
	ev.Environment = entitlements.EnvironmentGoogleTest
	r, ack, id := googleRig(t, google.Result{Events: []entitlements.Event{ev}, Ack: gAck}, nil)
	ack.EXPECT().Acknowledge(mock.Anything, "com.example.app", "premium.monthly", "tok-1").Return(nil).Once()
	st, err := r.run(id)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusIgnored, st)
}

// The caller's context ending mid-run must neither lose the outcome nor
// keep processing the remaining events.
func TestProcessStored_CancelledWhileProcessing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e1, e2 := googleEvent("m1", "tok-1", now), googleEvent("m1#2", "tok-1", now)
	r, _, id := googleRig(t, google.Result{Events: []entitlements.Event{e1, e2}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	svc := mocksinbox.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, e1).RunAndReturn(func(context.Context, entitlements.Event) (*entitlements.Entitlement, error) {
		cancel() // the worker is shutting down after the first event; e2 must not be processed
		return &entitlements.Entitlement{}, nil
	})
	r.p.Service = svc

	st, err := inbox.ProcessStored(ctx, r.in, id, r.p)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, inbox.StatusFailed, st)
	got := r.item(id)
	assert.Equal(t, inbox.StatusFailed, got.Status, "the outcome is recorded although the context ended")
	assert.Equal(t, 1, got.Attempts)
}

// A forged delivery that carries a genuine notification's UUID must not use
// up that notification's dedupe key: it is rejected before anything is stored.
func TestProcessStored_ForgedItemDoesNotConsumeTheRealOnesKey(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	genuine := r.appleBody("subscribed", nil)
	var rb apple.ResponseBodyV2
	require.NoError(t, json.Unmarshal(genuine, &rb))
	rb.SignedPayload = rb.SignedPayload[:len(rb.SignedPayload)-4] + "AAAA" // same payload, same UUID, bad signature
	forgedBody, err := json.Marshal(rb)
	require.NoError(t, err)

	forged := r.save(entitlements.SourceApple, forgedBody)
	real := r.save(entitlements.SourceApple, genuine)
	require.NotEqual(t, forged, real, "Apple deliveries get distinct inbox IDs: the UUID is never used as one")

	st, err := r.run(forged)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusInvalidSignature, st)
	_, err = r.svc.Get(context.Background(), entitlements.SourceApple, "2000000000001")
	require.ErrorIs(t, err, entitlements.ErrNotFound, "nothing was applied")

	st, err = r.run(real)
	require.NoError(t, err)
	assert.Equal(t, inbox.StatusProcessed, st, "the genuine notification is processed, not dropped as a duplicate")
}
