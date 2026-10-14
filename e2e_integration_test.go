//go:build integration

package entitlements_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap/zaptest"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	googlemocks "github.com/tggo/go-entitlements/google/mocks"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/postgres"
)

const (
	appleUser   = "7e3fb20b-4cdb-47cc-936d-99d65f608138"
	appleTx     = "2000000000001"
	googlePkg   = "com.example.app"
	googleUser  = "user-1"
	appleBundle = "com.example.app"
)

// E2ESuite runs a real HTTP server -> handlers -> service -> Postgres.
type E2ESuite struct {
	suite.Suite
	dockerPool *dockertest.Pool
	resource   *dockertest.Resource
	db         *pgxpool.Pool
	store      *postgres.Store

	ca      *testutil.AppleCA
	fetcher *googlemocks.MockSubscriptionsFetcher
	srv     *httptest.Server
	base    time.Time
}

func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	suite.Run(t, new(E2ESuite))
}

func (s *E2ESuite) SetupSuite() {
	var err error
	s.dockerPool, err = dockertest.NewPool("")
	s.Require().NoError(err)
	s.Require().NoError(s.dockerPool.Client.Ping())

	s.resource, err = s.dockerPool.RunWithOptions(&dockertest.RunOptions{
		Name:       fmt.Sprintf("go-entitlements-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()),
		Repository: "postgres",
		Tag:        "17",
		Env:        []string{"POSTGRES_PASSWORD=secret", "POSTGRES_USER=test", "POSTGRES_DB=entitlements"},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	s.Require().NoError(err)
	s.Require().NoError(s.resource.Expire(300))

	dsn := fmt.Sprintf("postgres://test:secret@%s/entitlements?sslmode=disable", s.resource.GetHostPort("5432/tcp"))
	s.Require().NoError(s.dockerPool.Retry(func() error {
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return err
		}
		if err := p.Ping(context.Background()); err != nil {
			p.Close()
			return err
		}
		s.db = p
		return nil
	}))
	s.store = postgres.New(s.db)
	s.Require().NoError(s.store.Migrate(context.Background()))
}

func (s *E2ESuite) TearDownSuite() {
	if s.db != nil {
		s.db.Close()
	}
	if s.dockerPool != nil && s.resource != nil {
		_ = s.dockerPool.Purge(s.resource)
	}
}

func (s *E2ESuite) SetupTest() {
	log := zaptest.NewLogger(s.T())
	s.base = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.ca = testutil.NewAppleCA(s.T())
	s.fetcher = googlemocks.NewMockSubscriptionsFetcher(s.T())

	svc := entitlements.NewService(s.store, entitlements.WithLogger(log))
	appleVerifier, err := apple.NewVerifier(appleBundle, apple.WithRootPool(s.ca.Pool))
	s.Require().NoError(err)
	appleParser := apple.NewParser(appleVerifier)
	googleParser := google.NewParser(s.fetcher, google.WithPackageName(googlePkg))

	mux := http.NewServeMux()
	mux.Handle("/webhooks/apple", httpx.NewAppleHandler(appleParser, svc, log))
	mux.Handle("/webhooks/google", httpx.NewGoogleHandler(googleParser, svc, log))
	s.srv = httptest.NewServer(mux)
}

func (s *E2ESuite) TearDownTest() {
	s.srv.Close()
	_, err := s.db.Exec(context.Background(), `TRUNCATE entitlements, processed_events`)
	s.Require().NoError(err)
}

// --- helpers -----------------------------------------------------------

func (s *E2ESuite) post(path string, body []byte) int {
	resp, err := http.Post(s.srv.URL+path, "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// appleBody signs a fixture with a custom notification UUID, signedDate and expiry.
func (s *E2ESuite) appleBody(fixture, uuid string, signed, expires time.Time) []byte {
	f := testutil.LoadAppleFixture(s.T(), fixture)
	f.Notification["notificationUUID"] = uuid
	f.Notification["signedDate"] = signed.UnixMilli()
	f.Transaction["expiresDate"] = expires.UnixMilli()
	return f.SignBody(s.T(), s.ca)
}

func (s *E2ESuite) appleEnt() *entitlements.Entitlement {
	e, err := s.store.Get(context.Background(), entitlements.SourceApple, appleTx)
	s.Require().NoError(err)
	return e
}

func (s *E2ESuite) countEvents(applied bool) int {
	var n int
	s.Require().NoError(s.db.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_events WHERE applied = $1`, applied).Scan(&n))
	return n
}

func (s *E2ESuite) googlePush(messageID, token, subID string, at time.Time) []byte {
	payload := fmt.Sprintf(`{"version":"1.0","packageName":%q,"eventTimeMillis":"%d","subscriptionNotification":{"version":"1.0","notificationType":4,"purchaseToken":%q,"subscriptionId":%q}}`,
		googlePkg, at.UnixMilli(), token, subID)
	return testutil.GooglePushBody(s.T(), messageID, []byte(payload))
}

func (s *E2ESuite) googleSub(fixture string) *google.SubscriptionPurchaseV2 {
	var sub google.SubscriptionPurchaseV2
	s.Require().NoError(json.Unmarshal(testutil.GoogleFixture(s.T(), fixture), &sub))
	return &sub
}

// --- scenarios ---------------------------------------------------------

func (s *E2ESuite) TestApple_SubscribeRenewRefund() {
	t1, t2, t3 := s.base, s.base.Add(30*24*time.Hour), s.base.Add(40*24*time.Hour)
	exp1, exp2 := t1.Add(30*24*time.Hour), t2.Add(30*24*time.Hour)

	s.Equal(200, s.post("/webhooks/apple", s.appleBody("subscribed", "n-1", t1, exp1)))
	e := s.appleEnt()
	s.Equal(entitlements.StateActive, e.State)
	s.Equal(appleUser, e.UserID)
	s.Equal("com.example.pro.monthly", e.ProductID)
	s.Equal(exp1.UnixMilli(), e.ExpiresAt.UnixMilli())
	s.EqualValues(1, e.Version)

	s.Equal(200, s.post("/webhooks/apple", s.appleBody("did_renew", "n-2", t2, exp2)))
	e = s.appleEnt()
	s.Equal(entitlements.StateActive, e.State)
	s.Equal(exp2.UnixMilli(), e.ExpiresAt.UnixMilli())
	s.EqualValues(2, e.Version)

	s.Equal(200, s.post("/webhooks/apple", s.appleBody("refund", "n-3", t3, exp2)))
	e = s.appleEnt()
	s.Equal(entitlements.StateRefunded, e.State)
	s.EqualValues(3, e.Version)
	s.False(e.Active(t3))
	s.Equal(3, s.countEvents(true))
}

func (s *E2ESuite) TestApple_DuplicatePost() {
	body := s.appleBody("subscribed", "n-dup", s.base, s.base.Add(30*24*time.Hour))
	s.Equal(200, s.post("/webhooks/apple", body))
	s.Equal(200, s.post("/webhooks/apple", body))

	s.EqualValues(1, s.appleEnt().Version, "second delivery must not change the entitlement")
	s.Equal(1, s.countEvents(true))
	var total int
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT count(*) FROM processed_events`).Scan(&total))
	s.Equal(1, total)
}

func (s *E2ESuite) TestApple_OutOfOrder() {
	t1, t2, t3 := s.base, s.base.Add(30*24*time.Hour), s.base.Add(40*24*time.Hour)
	exp1, exp2 := t1.Add(30*24*time.Hour), t2.Add(30*24*time.Hour)

	// The renewal (t2) arrives before the original purchase (t1).
	s.Equal(200, s.post("/webhooks/apple", s.appleBody("did_renew", "n-2", t2, exp2)))
	s.Equal(200, s.post("/webhooks/apple", s.appleBody("subscribed", "n-1", t1, exp1)))
	e := s.appleEnt()
	s.Equal(exp2.UnixMilli(), e.ExpiresAt.UnixMilli(), "older event must not overwrite newer state")
	s.EqualValues(1, e.Version)
	s.Equal(1, s.countEvents(false), "stale event is recorded, not applied")

	// A refund (t3) wins over a late renewal that was signed before it.
	s.Equal(200, s.post("/webhooks/apple", s.appleBody("refund", "n-4", t3, exp2)))
	s.Equal(200, s.post("/webhooks/apple", s.appleBody("did_renew", "n-3", t3.Add(-time.Hour), exp2.Add(30*24*time.Hour))))
	e = s.appleEnt()
	s.Equal(entitlements.StateRefunded, e.State)
	s.Equal(exp2.UnixMilli(), e.ExpiresAt.UnixMilli())
}

func (s *E2ESuite) TestApple_RejectsForgedAndMalformed() {
	other := testutil.NewAppleCA(s.T())
	f := testutil.LoadAppleFixture(s.T(), "subscribed")
	s.Equal(401, s.post("/webhooks/apple", f.SignBody(s.T(), other)), "valid chain, but not our root")
	s.Equal(400, s.post("/webhooks/apple", []byte(`not json`)))
	s.Equal(401, s.post("/webhooks/apple", []byte(`{"signedPayload":"a.b.c"}`)))

	resp, err := http.Get(s.srv.URL + "/webhooks/apple")
	s.Require().NoError(err)
	_ = resp.Body.Close()
	s.Equal(405, resp.StatusCode)

	var n int
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT count(*) FROM entitlements`).Scan(&n))
	s.Zero(n)
}

func (s *E2ESuite) TestApple_TestNotificationIsAcknowledged() {
	f := testutil.LoadAppleFixture(s.T(), "test")
	s.Equal(200, s.post("/webhooks/apple", f.SignBody(s.T(), s.ca)))
}

func (s *E2ESuite) TestGoogle_UpgradeViaLinkedPurchaseToken() {
	t1, t2 := s.base, s.base.Add(24*time.Hour)
	s.fetcher.EXPECT().GetV2(mock.Anything, googlePkg, "old-token").Return(s.googleSub("sub_state_active.json"), nil)
	s.fetcher.EXPECT().GetV2(mock.Anything, googlePkg, "new-token").Return(s.googleSub("sub_linked_upgrade.json"), nil)

	s.Equal(200, s.post("/webhooks/google", s.googlePush("m-1", "old-token", "premium.monthly", t1)))
	old, err := s.store.Get(context.Background(), entitlements.SourceGoogle, "old-token")
	s.Require().NoError(err)
	s.Equal(entitlements.StateActive, old.State)

	s.Equal(200, s.post("/webhooks/google", s.googlePush("m-2", "new-token", "premium.yearly", t2)))

	old, err = s.store.Get(context.Background(), entitlements.SourceGoogle, "old-token")
	s.Require().NoError(err)
	s.Equal(entitlements.StateExpired, old.State, "superseded chain must be retired")
	cur, err := s.store.Get(context.Background(), entitlements.SourceGoogle, "new-token")
	s.Require().NoError(err)
	s.Equal(entitlements.StateActive, cur.State)
	s.Equal("premium.yearly", cur.ProductID)
	s.Equal(googleUser, cur.UserID)

	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	list, err := s.store.ListByUser(context.Background(), googleUser)
	s.Require().NoError(err)
	var active []string
	for _, e := range list {
		if e.Active(now) {
			active = append(active, e.ProductID)
		}
	}
	s.Equal([]string{"premium.yearly"}, active)
}

func (s *E2ESuite) TestGoogle_Rejections() {
	s.Equal(400, s.post("/webhooks/google", []byte(`{"message":{"data":"!!","messageId":"x"}}`)))
	s.Equal(200, s.post("/webhooks/google", testutil.GooglePushBody(s.T(), "m-test", testutil.GoogleFixture(s.T(), "notif_test.json"))), "test notification is acknowledged")

	s.fetcher.EXPECT().GetV2(mock.Anything, googlePkg, "tok-err").Return(nil, fmt.Errorf("play api 503"))
	s.Equal(500, s.post("/webhooks/google", s.googlePush("m-err", "tok-err", "premium.monthly", s.base)), "fetch failure must make Pub/Sub retry")
	var n int
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT count(*) FROM processed_events`).Scan(&n))
	s.Zero(n, "a failed fetch must not mark the message as processed")
}

func (s *E2ESuite) TestApple_ConcurrentAndDuplicateDeliveries() {
	const renewals, copies = 8, 4
	var bodies [][]byte
	var latest time.Time
	for i := 0; i < renewals; i++ {
		signed := s.base.Add(time.Duration(i) * 24 * time.Hour)
		fixture := "did_renew"
		if i == 0 {
			fixture = "subscribed"
		}
		b := s.appleBody(fixture, fmt.Sprintf("n-%d", i), signed, signed.Add(30*24*time.Hour))
		latest = signed
		for c := 0; c < copies; c++ {
			bodies = append(bodies, b)
		}
	}

	codes := make([]int, len(bodies))
	var wg sync.WaitGroup
	for i, b := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(s.srv.URL+"/webhooks/apple", "application/json", bytes.NewReader(b))
			if err != nil {
				codes[i] = -1
				return
			}
			_ = resp.Body.Close()
			codes[i] = resp.StatusCode
		}()
	}
	wg.Wait()
	for i, c := range codes {
		s.Equal(200, c, "request %d", i)
	}

	e := s.appleEnt()
	s.Equal(entitlements.StateActive, e.State)
	s.Equal(latest.UnixMilli(), e.UpdatedAt.UnixMilli(), "newest event must win regardless of arrival order")
	s.Equal(latest.Add(30*24*time.Hour).UnixMilli(), e.ExpiresAt.UnixMilli())

	var total, distinct int
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT count(*), count(DISTINCT event_id) FROM processed_events`).Scan(&total, &distinct))
	s.Equal(renewals, total, "each notificationUUID recorded exactly once")
	s.Equal(renewals, distinct)
	s.EqualValues(s.countEvents(true), e.Version, "version counts applied events only")
}
