package entitlements_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/memory"
)

// The explainer site (site/) has a simulator that re-implements these rules in
// JavaScript. site/scenarios.json holds its cards and scenarios plus the
// outcome the real library gives for every step. This test replays them
// through the real code and fails if the stored expectations differ;
// site/check-scenarios.js replays the same file through the JavaScript port in
// CI. After an intended behaviour change, regenerate with
//
//	go test -run TestSiteScenarios -update-site .
//
// and fix site/engine.js until `node site/check-scenarios.js` passes.
//
// Path under test: Apple cards are signed with a throwaway CA and go through
// apple.Parser (real verifier), httpx.NewAppleHandler and Service on the memory
// store. A "forged" card is signed by a second CA the verifier does not trust.
// Google cards go through google.Parser with a fake subscriptionsv2 fetcher,
// httpx.NewGoogleHandler and the same Service. Only the Play API and the
// Pub/Sub token check are fakes.

var updateSite = flag.Bool("update-site", false, "rewrite the expect blocks in site/scenarios.json from the real library")

type siteApple struct {
	Type, Subtype, UUID, Tx, Txn, Product, UserID, Env, TxType string
	Status                                                     int
	Signed, Expires, GraceEnd, RevocationDate                  string
}

type siteGoogle struct {
	Kind, MessageID, Token, SubState, Linked, Product, UserID string
	EventTime, Expiry                                         string
	OrderMatches, TestPurchase                                bool
}

type siteCard struct {
	ID     string
	Src    string
	Forged bool
	NoAuth bool
	Apple  siteApple
	Google siteGoogle
}

func (c *siteCard) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID     string          `json:"id"`
		Src    string          `json:"src"`
		Forged bool            `json:"forged"`
		NoAuth bool            `json:"noAuth"`
		Apple  json.RawMessage `json:"apple"`
		Google json.RawMessage `json:"google"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.ID, c.Src, c.Forged, c.NoAuth = raw.ID, raw.Src, raw.Forged, raw.NoAuth
	if len(raw.Apple) > 0 {
		var a struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			UUID      string `json:"uuid"`
			Tx        string `json:"tx"`
			Txn       string `json:"txn"`
			Product   string `json:"product"`
			UserID    string `json:"userId"`
			Env       string `json:"env"`
			TxType    string `json:"txType"`
			Status    int    `json:"status"`
			Signed    string `json:"signed"`
			Expires   string `json:"expires"`
			GraceEnd  string `json:"graceEnd"`
			Revocated string `json:"revocationDate"`
		}
		if err := json.Unmarshal(raw.Apple, &a); err != nil {
			return err
		}
		c.Apple = siteApple{a.Type, a.Subtype, a.UUID, a.Tx, a.Txn, a.Product, a.UserID, a.Env, a.TxType, a.Status, a.Signed, a.Expires, a.GraceEnd, a.Revocated}
	}
	if len(raw.Google) > 0 {
		var g struct {
			Kind         string `json:"kind"`
			MessageID    string `json:"messageId"`
			Token        string `json:"token"`
			SubState     string `json:"subState"`
			Linked       string `json:"linked"`
			Product      string `json:"product"`
			UserID       string `json:"userId"`
			EventTime    string `json:"eventTime"`
			Expiry       string `json:"expiry"`
			OrderMatches bool   `json:"orderMatches"`
			TestPurchase bool   `json:"testPurchase"`
		}
		if err := json.Unmarshal(raw.Google, &g); err != nil {
			return err
		}
		c.Google = siteGoogle{g.Kind, g.MessageID, g.Token, g.SubState, g.Linked, g.Product, g.UserID, g.EventTime, g.Expiry, g.OrderMatches, g.TestPurchase}
	}
	return nil
}

type siteChain struct {
	State   string `json:"state"`
	Version int64  `json:"version"`
	Expires string `json:"expires"`
	Updated string `json:"updated"`
	User    string `json:"user"`
	// Refunded lists the refunded transactions, oldest first (never nil).
	Refunded []string `json:"refunded"`
}

type siteExpect struct {
	Code   int                  `json:"code"`
	Stamp  string               `json:"stamp"`
	Chains map[string]siteChain `json:"chains"`
	Access map[string]bool      `json:"access"`
}

type siteStep struct {
	Send     string          `json:"send"`
	DB       string          `json:"db"`
	PlayDown *bool           `json:"playDown"`
	Expect   json.RawMessage `json:"expect"`
}

type siteScenario struct {
	Name         string     `json:"name"`
	AllowSandbox bool       `json:"allowSandbox"`
	Steps        []siteStep `json:"steps"`
}

type siteFixture struct {
	WallClock  string         `json:"wallClock"`
	Product    string         `json:"product"`
	User       string         `json:"user"`
	CheckDates []string       `json:"checkDates"`
	Cards      []siteCard     `json:"cards"`
	Scenarios  []siteScenario `json:"scenarios"`
}

func siteDate(t *testing.T, s string) time.Time {
	t.Helper()
	if s == "" {
		return time.Time{}
	}
	d, err := time.Parse("2006-01-02", s)
	require.NoError(t, err)
	return d
}

func siteMs(t *testing.T, s string) int64 {
	d := siteDate(t, s)
	if d.IsZero() {
		return 0
	}
	return d.UnixMilli()
}

// downableStore fails every call while down is set.
type downableStore struct {
	*memory.Store
	down bool
}

var errSiteDB = errors.New("database is down")

func (s *downableStore) Apply(ctx context.Context, ev entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	if s.down {
		return nil, errSiteDB
	}
	return s.Store.Apply(ctx, ev, fn)
}

func (s *downableStore) Get(ctx context.Context, src entitlements.Source, tx string) (*entitlements.Entitlement, error) {
	if s.down {
		return nil, errSiteDB
	}
	return s.Store.Get(ctx, src, tx)
}

func (s *downableStore) ListByUser(ctx context.Context, u string) ([]entitlements.Entitlement, error) {
	if s.down {
		return nil, errSiteDB
	}
	return s.Store.ListByUser(ctx, u)
}

type siteRig struct {
	t        *testing.T
	fx       *siteFixture
	st       *downableStore
	svc      *entitlements.Service
	now      time.Time
	playDown bool
	sub      *google.SubscriptionPurchaseV2

	parseCalled bool
	parseErr    error
	procErr     error
	procCalled  bool

	apple, google http.Handler
	trusted       *testutil.AppleCA
	attacker      *testutil.AppleCA
}

type recordingProc struct{ r *siteRig }

func (p recordingProc) Process(ctx context.Context, ev entitlements.Event) (*entitlements.Entitlement, error) {
	p.r.procCalled = true
	ent, err := p.r.svc.Process(ctx, ev)
	p.r.procErr = err
	return ent, err
}

type recordingApple struct {
	r *siteRig
	p *apple.Parser
}

func (a recordingApple) Parse(body []byte) (entitlements.Event, error) {
	a.r.parseCalled = true
	ev, err := a.p.Parse(body)
	a.r.parseErr = err
	return ev, err
}

type recordingGoogle struct {
	r *siteRig
	p *google.Parser
}

func (g recordingGoogle) ParseResult(ctx context.Context, body []byte) (google.Result, error) {
	g.r.parseCalled = true
	res, err := g.p.ParseResult(ctx, body)
	g.r.parseErr = err
	return res, err
}

type siteFetcher struct{ r *siteRig }

func (f siteFetcher) GetV2(context.Context, string, string) (*google.SubscriptionPurchaseV2, error) {
	if f.r.playDown {
		return nil, fmt.Errorf("%w: play api timeout", google.ErrFetchTransient)
	}
	return f.r.sub, nil
}

const sitePackage = "com.example.app"

func newSiteRig(t *testing.T, fx *siteFixture, allowSandbox bool, trusted, attacker *testutil.AppleCA) *siteRig {
	r := &siteRig{t: t, fx: fx, st: &downableStore{Store: memory.New()}, trusted: trusted, attacker: attacker}
	opts := []entitlements.Option{entitlements.WithTimeFn(func() time.Time { return r.now })}
	if allowSandbox {
		opts = append(opts, entitlements.WithAllowSandbox())
	}
	r.svc = entitlements.NewService(r.st, opts...)
	v, err := apple.NewVerifier(sitePackage, apple.WithRootPool(trusted.Pool))
	require.NoError(t, err)
	r.apple = httpx.NewAppleHandler(recordingApple{r, apple.NewParser(v)}, recordingProc{r})
	gp, err := google.NewParser(siteFetcher{r}, sitePackage, google.WithTimeFn(func() time.Time { return siteDate(t, fx.WallClock) }))
	require.NoError(t, err)
	r.google, err = httpx.NewGoogleHandler(recordingGoogle{r, gp}, recordingProc{r},
		httpx.WithPushAuthenticator(func(req *http.Request) error {
			if req.Header.Get("X-Test-Push-Token") == "" {
				return errors.New("no push token")
			}
			return nil
		}))
	require.NoError(t, err)
	return r
}

func (r *siteRig) appleBody(c siteCard) []byte {
	t, a := r.t, c.Apple
	env := a.Env
	if env == "" {
		env = "Production"
	}
	data := map[string]any{"bundleId": sitePackage, "environment": env}
	if a.Status != 0 {
		data["status"] = a.Status
	}
	n := map[string]any{
		"notificationType": a.Type, "notificationUUID": a.UUID, "version": "2.0",
		"signedDate": siteMs(t, a.Signed), "data": data,
	}
	if a.Subtype != "" {
		n["subtype"] = a.Subtype
	}
	f := testutil.AppleFixture{Notification: n}
	if a.Type != "TEST" {
		txn := a.Txn
		if txn == "" {
			txn = "t-" + a.UUID
		}
		tx := map[string]any{
			"originalTransactionId": a.Tx, "transactionId": txn, "productId": a.Product,
			"bundleId": sitePackage, "environment": env, "appAccountToken": a.UserID,
		}
		if ms := siteMs(t, a.Expires); ms != 0 {
			tx["expiresDate"] = ms
		}
		if ms := siteMs(t, a.RevocationDate); ms != 0 {
			tx["revocationDate"] = ms
		}
		if a.TxType != "" {
			tx["type"] = a.TxType
		}
		f.Transaction = tx
		if ms := siteMs(t, a.GraceEnd); ms != 0 {
			f.Renewal = map[string]any{"originalTransactionId": a.Tx, "gracePeriodExpiresDate": ms, "environment": env}
		}
	}
	ca := r.trusted
	if c.Forged {
		ca = r.attacker
	}
	return f.SignBody(t, ca)
}

func (r *siteRig) googleBody(c siteCard) []byte {
	t, g := r.t, c.Google
	dn := google.DeveloperNotification{
		Version: "1.0", PackageName: sitePackage,
		EventTimeMillis: google.Millis(siteMs(t, g.EventTime)),
	}
	switch g.Kind {
	case "test":
		dn.TestNotification = &google.TestNotification{Version: "1.0"}
	case "voided":
		dn.VoidedPurchaseNotification = &google.VoidedPurchaseNotification{PurchaseToken: g.Token, OrderID: "order-1"}
	default:
		dn.SubscriptionNotification = &google.SubscriptionNotification{Version: "1.0", NotificationType: 4, PurchaseToken: g.Token, SubscriptionID: g.Product}
	}
	if g.Kind == "otherPackage" {
		dn.PackageName = "com.other.app"
	}
	inner, err := json.Marshal(dn)
	require.NoError(t, err)
	body, err := json.Marshal(google.PushEnvelope{Message: google.PushMessage{Data: inner, MessageID: g.MessageID}})
	require.NoError(t, err)

	li := google.LineItem{ProductID: g.Product, ExpiryTime: siteDate(t, g.Expiry), LatestSuccessfulOrderID: "order-other"}
	if g.OrderMatches {
		li.LatestSuccessfulOrderID = "order-1"
	}
	state := g.SubState
	if state == "" {
		state = "ACTIVE"
	}
	r.sub = &google.SubscriptionPurchaseV2{
		SubscriptionState:          google.State("SUBSCRIPTION_STATE_" + state),
		LinkedPurchaseToken:        g.Linked,
		LineItems:                  []google.LineItem{li},
		ExternalAccountIdentifiers: &google.ExternalAccountIdentifiers{ObfuscatedExternalAccountID: g.UserID},
		TestPurchase:               g.TestPurchase,
	}
	return body
}

func (r *siteRig) stamp(c siteCard, code int) string {
	if c.Src == "google" && !r.parseCalled && code == http.StatusUnauthorized {
		return "REJECTED"
	}
	if r.parseErr != nil {
		switch {
		case errors.Is(r.parseErr, entitlements.ErrInvalidSignature):
			return "FORGED"
		case errors.Is(r.parseErr, google.ErrMalformed):
			return "REJECTED"
		case errors.Is(r.parseErr, google.ErrFetchTransient):
			return "RETRY"
		case errors.Is(r.parseErr, entitlements.ErrIgnored), errors.Is(r.parseErr, google.ErrPackageMismatch):
			return "IGNORED"
		case errors.Is(r.parseErr, entitlements.ErrUnsupported):
			return "SKIPPED"
		}
		return "?" + r.parseErr.Error()
	}
	switch {
	case r.procErr == nil:
		return "APPLIED"
	case errors.Is(r.procErr, entitlements.ErrDuplicate):
		return "DUPLICATE"
	case errors.Is(r.procErr, entitlements.ErrStale):
		return "STALE"
	case errors.Is(r.procErr, entitlements.ErrIgnored):
		return "IGNORED"
	}
	return "RETRY"
}

func (r *siteRig) send(c siteCard) siteExpect {
	r.parseCalled, r.parseErr, r.procErr, r.procCalled = false, nil, nil, false
	var h http.Handler
	var body []byte
	if c.Src == "apple" {
		h, body = r.apple, r.appleBody(c)
	} else {
		h, body = r.google, r.googleBody(c)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if !c.NoAuth {
		req.Header.Set("X-Test-Push-Token", "ok")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return siteExpect{Code: rec.Code, Stamp: r.stamp(c, rec.Code), Chains: r.chains(), Access: r.access()}
}

func (r *siteRig) chains() map[string]siteChain {
	out := map[string]siteChain{}
	seen := map[string]bool{}
	for _, c := range r.fx.Cards {
		var src entitlements.Source
		var tx string
		if c.Src == "apple" {
			src, tx = entitlements.SourceApple, c.Apple.Tx
		} else {
			src, tx = entitlements.SourceGoogle, c.Google.Token
		}
		key := string(src) + "|" + tx
		if tx == "" || seen[key] {
			continue
		}
		seen[key] = true
		e, err := r.st.Store.Get(context.Background(), src, tx)
		if errors.Is(err, entitlements.ErrNotFound) {
			continue
		}
		require.NoError(r.t, err)
		date := func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format("2006-01-02")
		}
		refunded := append([]string{}, e.RefundedTransactions...)
		out[key] = siteChain{State: string(e.State), Version: e.Version, Expires: date(e.ExpiresAt), Updated: date(e.UpdatedAt), User: e.UserID, Refunded: refunded}
	}
	return out
}

func (r *siteRig) access() map[string]bool {
	out := map[string]bool{}
	was := r.st.down
	r.st.down = false
	defer func() { r.st.down = was }()
	for _, d := range r.fx.CheckDates {
		r.now = siteDate(r.t, d)
		ok, err := r.svc.IsActive(context.Background(), r.fx.User, r.fx.Product)
		require.NoError(r.t, err)
		out[d] = ok
	}
	return out
}

func TestSiteScenarios(t *testing.T) {
	path := filepath.Join("site", "scenarios.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var fx siteFixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	cards := map[string]siteCard{}
	for _, c := range fx.Cards {
		cards[c.ID] = c
	}
	trusted, attacker := testutil.NewAppleCA(t), testutil.NewAppleCA(t)

	// Generic copy of the file, so an update keeps every other field.
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&doc))
	docScenarios := doc["scenarios"].([]any)

	for si, sc := range fx.Scenarios {
		t.Run(strings.ReplaceAll(sc.Name, " ", "_"), func(t *testing.T) {
			rig := newSiteRig(t, &fx, sc.AllowSandbox, trusted, attacker)
			docSteps := docScenarios[si].(map[string]any)["steps"].([]any)
			for i, st := range sc.Steps {
				switch {
				case st.DB != "":
					rig.st.down = st.DB == "down"
				case st.PlayDown != nil:
					rig.playDown = *st.PlayDown
				default:
					c, ok := cards[st.Send]
					require.True(t, ok, "unknown card %q", st.Send)
					got := rig.send(c)
					require.False(t, strings.HasPrefix(got.Stamp, "?"), "step %d %s: unclassified parse error %s", i, st.Send, got.Stamp)
					if *updateSite {
						var m map[string]any
						b, _ := json.Marshal(got)
						require.NoError(t, json.Unmarshal(b, &m))
						docSteps[i].(map[string]any)["expect"] = m
						continue
					}
					var want siteExpect
					require.NotEmpty(t, st.Expect, "step %d %s has no expectation; run with -update-site", i, st.Send)
					require.NoError(t, json.Unmarshal(st.Expect, &want))
					require.Equal(t, want, got, "scenario %q step %d (card %s)", sc.Name, i, st.Send)
				}
			}
		})
	}

	if *updateSite {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		require.NoError(t, enc.Encode(doc))
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	}
}
