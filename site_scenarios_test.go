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
	"github.com/tggo/go-entitlements/apple/serverapi"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/memory"
)

// The explainer site (site/) has a simulator that re-implements these rules in
// JavaScript. site/scenarios.json holds its cards, actions and scenarios plus
// the outcome the real library gives for every step. This test replays them
// through the real code and fails if the stored expectations differ;
// site/check-scenarios.js replays the same file through the JavaScript port in
// CI. After an intended behaviour change, regenerate with
//
//	go test -run TestSiteScenarios -update-site .
//
// read the diff of site/scenarios.json, and fix site/engine.js until
// `make test-site` passes.
//
// Path under test:
//   - Apple cards are signed with a throwaway CA and go through
//     serverapi.NewConsumptionHandler, httpx.NewAppleHandler, the real
//     apple.Parser (real verifier) and Service on the memory store. A "forged"
//     card is signed by a second CA the verifier does not trust. For
//     CONSUMPTION_REQUEST the real wrapper answers; the responder and the
//     sender are fakes.
//   - Google cards go through google.Parser, httpx.NewGoogleHandler with an
//     acknowledger, and the same Service. Fakes: the Play API (one
//     subscription per purchase token, like Google), the Pub/Sub token check
//     and the acknowledge call.
//   - Actions call Service.LinkUser, Service.ForgetUser and the real
//     serverapi.Reconcile (the App Store Server API answer is a fake).
//   - Not covered here: serverapi.ReplayNotificationHistory (needs the App
//     Store Server API over HTTP; it has its own tests).

var updateSite = flag.Bool("update-site", false, "rewrite the expect blocks in site/scenarios.json from the real library")

type siteApple struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	UUID           string `json:"uuid"`
	Tx             string `json:"tx"`
	Txn            string `json:"txn"`
	Product        string `json:"product"`
	UserID         string `json:"userId"`
	Env            string `json:"env"`
	TxType         string `json:"txType"`
	Status         int    `json:"status"`
	Signed         string `json:"signed"`
	Expires        string `json:"expires"`
	GraceEnd       string `json:"graceEnd"`
	RevocationDate string `json:"revocationDate"`
	AutoRenew      *int   `json:"autoRenew"`
	Consumption    string `json:"consumption"`
}

type siteItem struct {
	Product   string `json:"product"`
	Expiry    string `json:"expiry"`
	Order     string `json:"order"`
	AutoRenew *bool  `json:"autoRenew"`
}

type siteGoogle struct {
	Kind         string     `json:"kind"`
	MessageID    string     `json:"messageId"`
	Token        string     `json:"token"`
	SubState     string     `json:"subState"`
	Linked       string     `json:"linked"`
	Product      string     `json:"product"`
	UserID       string     `json:"userId"`
	EventTime    string     `json:"eventTime"`
	Expiry       string     `json:"expiry"`
	OrderMatches bool       `json:"orderMatches"`
	TestPurchase bool       `json:"testPurchase"`
	Ack          string     `json:"ack"`
	AutoRenew    *bool      `json:"autoRenew"`
	Items        []siteItem `json:"items"`
	NoItems      bool       `json:"noItems"`
}

type siteCard struct {
	ID     string     `json:"id"`
	Src    string     `json:"src"`
	Forged bool       `json:"forged"`
	NoAuth bool       `json:"noAuth"`
	Apple  siteApple  `json:"apple"`
	Google siteGoogle `json:"google"`
}

type siteAction struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Tx       string `json:"tx"`
	User     string `json:"user"`
	Status   int    `json:"status"`
	Family   bool   `json:"family"`
	Signed   string `json:"signed"`
	Expires  string `json:"expires"`
	GraceEnd string `json:"graceEnd"`
	Product  string `json:"product"`
	Txn      string `json:"txn"`
}

type siteChain struct {
	State   string `json:"state"`
	Version int64  `json:"version"`
	Expires string `json:"expires"`
	Updated string `json:"updated"`
	User    string `json:"user"`
	// Refunded lists the refunded transactions, oldest first (never nil).
	Refunded  []string `json:"refunded"`
	AutoRenew *bool    `json:"autoRenew"`
}

type siteExpect struct {
	Code    int                  `json:"code"`
	Stamp   string               `json:"stamp"`
	Detail  string               `json:"detail"`
	Acked   bool                 `json:"acked"`
	Changes int                  `json:"changes"`
	Chains  map[string]siteChain `json:"chains"`
	Access  map[string]bool      `json:"access"`
}

type siteStep struct {
	Send     string          `json:"send"`
	Do       string          `json:"do"`
	DB       string          `json:"db"`
	Register string          `json:"register"`
	PlayDown *bool           `json:"playDown"`
	Ack      string          `json:"ack"`
	Expect   json.RawMessage `json:"expect"`
}

type siteScenario struct {
	Name         string            `json:"name"`
	AllowSandbox bool              `json:"allowSandbox"`
	Resolver     map[string]string `json:"resolver"`
	Steps        []siteStep        `json:"steps"`
}

type siteFixture struct {
	WallClock     string         `json:"wallClock"`
	Product       string         `json:"product"`
	User          string         `json:"user"`
	CheckDates    []string       `json:"checkDates"`
	CheckProducts []string       `json:"checkProducts"`
	Cards         []siteCard     `json:"cards"`
	Actions       []siteAction   `json:"actions"`
	Scenarios     []siteScenario `json:"scenarios"`
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

func (s *downableStore) DeleteUser(ctx context.Context, u string) (int, error) {
	if s.down {
		return 0, errSiteDB
	}
	return s.Store.DeleteUser(ctx, u)
}

type siteRig struct {
	t        *testing.T
	fx       *siteFixture
	st       *downableStore
	svc      *entitlements.Service
	now      time.Time
	playDown bool
	ackDown  bool
	subs     map[string]*google.SubscriptionPurchaseV2 // what Google knows, by purchase token
	changes  int
	cur      siteCard

	parseCalled bool
	parseErr    error
	procErrs    []error
	acks        int
	ackErr      error
	sent        int
	respErr     error
	answered    bool

	apple, google http.Handler
	trusted       *testutil.AppleCA
	attacker      *testutil.AppleCA
}

type recordingProc struct{ r *siteRig }

func (p recordingProc) Process(ctx context.Context, ev entitlements.Event) (*entitlements.Entitlement, error) {
	ent, err := p.r.svc.Process(ctx, ev)
	p.r.procErrs = append(p.r.procErrs, err)
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

// siteFetcher is Google's side: one subscription per purchase token. A token
// Google has not been told about is a 404.
type siteFetcher struct{ r *siteRig }

func (f siteFetcher) GetV2(_ context.Context, _, token string) (*google.SubscriptionPurchaseV2, error) {
	if f.r.playDown {
		return nil, fmt.Errorf("%w: play api timeout", google.ErrFetchTransient)
	}
	sub, ok := f.r.subs[token]
	if !ok {
		return nil, fmt.Errorf("%w: unknown token", google.ErrPurchaseNotFound)
	}
	return sub, nil
}

type siteAcker struct{ r *siteRig }

func (a siteAcker) Acknowledge(context.Context, string, string, string) error {
	if a.r.ackDown {
		a.r.ackErr = errors.New("acknowledge refused")
		return a.r.ackErr
	}
	a.r.acks++
	return nil
}

type siteResponder struct{ r *siteRig }

func (s siteResponder) Consumption(context.Context, serverapi.ConsumptionRequest) (serverapi.ConsumptionInfo, error) {
	s.r.answered = true
	switch s.r.cur.Apple.Consumption {
	case "yes":
		return serverapi.ConsumptionInfo{CustomerConsented: true, DeliveryStatus: serverapi.DeliveryDelivered, SampleContentProvided: true, RefundPreference: serverapi.RefundDecline}, nil
	case "no":
		return serverapi.ConsumptionInfo{}, serverapi.ErrConsentRequired
	}
	s.r.respErr = errors.New("lookup failed")
	return serverapi.ConsumptionInfo{}, s.r.respErr
}

type siteSender struct{ r *siteRig }

func (s siteSender) SendConsumptionInfo(context.Context, string, serverapi.ConsumptionInfo) error {
	s.r.sent++
	return nil
}

const sitePackage = "com.example.app"

func newSiteRig(t *testing.T, fx *siteFixture, sc siteScenario, trusted, attacker *testutil.AppleCA) *siteRig {
	r := &siteRig{t: t, fx: fx, st: &downableStore{Store: memory.New()}, trusted: trusted, attacker: attacker,
		subs: map[string]*google.SubscriptionPurchaseV2{}}
	opts := []entitlements.Option{
		entitlements.WithTimeFn(func() time.Time { return r.now }),
		entitlements.WithOnChange(func(context.Context, *entitlements.Entitlement, *entitlements.Entitlement) { r.changes++ }),
	}
	if sc.AllowSandbox {
		opts = append(opts, entitlements.WithAllowSandbox())
	}
	if len(sc.Resolver) > 0 {
		opts = append(opts, entitlements.WithUserResolver(func(_ context.Context, ev entitlements.Event) (string, error) {
			u := sc.Resolver[ev.OriginalTxID]
			if u == "!error" {
				return "", errors.New("directory unavailable")
			}
			return u, nil
		}))
	}
	r.svc = entitlements.NewService(r.st, opts...)
	v, err := apple.NewVerifier(sitePackage, apple.WithRootPool(trusted.Pool))
	require.NoError(t, err)
	inner, err := httpx.NewAppleHandler(recordingApple{r, apple.NewParser(v)}, recordingProc{r})
	require.NoError(t, err)
	r.apple = serverapi.NewConsumptionHandler(inner, serverapi.ConsumptionHandlerConfig{
		Verifier: v, Responder: siteResponder{r}, Sender: siteSender{r},
		Clock: func() time.Time { return siteDate(t, fx.WallClock) },
	})
	gp, err := google.NewParser(siteFetcher{r}, sitePackage, google.WithTimeFn(func() time.Time { return siteDate(t, fx.WallClock) }))
	require.NoError(t, err)
	r.google, err = httpx.NewGoogleHandler(recordingGoogle{r, gp}, recordingProc{r},
		httpx.WithAcknowledger(siteAcker{r}),
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
	if a.Type == "CONSUMPTION_REQUEST" {
		data["consumptionRequestReason"] = "UNINTENDED_PURCHASE"
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
		ren := map[string]any{"originalTransactionId": a.Tx, "environment": env}
		hasRen := false
		if ms := siteMs(t, a.GraceEnd); ms != 0 {
			ren["gracePeriodExpiresDate"] = ms
			hasRen = true
		}
		if a.AutoRenew != nil {
			ren["autoRenewStatus"] = *a.AutoRenew
			hasRen = true
		}
		if hasRen {
			f.Renewal = ren
		}
	}
	ca := r.trusted
	if c.Forged {
		ca = r.attacker
	}
	return f.SignBody(t, ca)
}

// registerGoogle tells the fake Play API about the subscription a card
// describes, and returns the push body for it.
func (r *siteRig) registerGoogle(c siteCard) []byte {
	t, g := r.t, c.Google
	items := g.Items
	if len(items) == 0 {
		items = []siteItem{{Product: g.Product, Expiry: g.Expiry}}
	}
	var lis []google.LineItem
	for _, it := range items {
		li := google.LineItem{ProductID: it.Product, ExpiryTime: siteDate(t, it.Expiry), LatestSuccessfulOrderID: "order-other"}
		switch {
		case it.Order == "none":
			li.LatestSuccessfulOrderID = ""
		case it.Order != "":
			li.LatestSuccessfulOrderID = it.Order
		case g.OrderMatches:
			li.LatestSuccessfulOrderID = "order-1"
		}
		if it.AutoRenew != nil {
			li.AutoRenewingPlan = &google.AutoRenewingPlan{AutoRenewEnabled: *it.AutoRenew}
		} else if g.AutoRenew != nil {
			li.AutoRenewingPlan = &google.AutoRenewingPlan{AutoRenewEnabled: *g.AutoRenew}
		}
		lis = append(lis, li)
	}
	if g.NoItems {
		lis = nil
	}
	state := g.SubState
	if state == "" {
		state = "ACTIVE"
	}
	sub := &google.SubscriptionPurchaseV2{
		SubscriptionState:          google.State("SUBSCRIPTION_STATE_" + state),
		LinkedPurchaseToken:        g.Linked,
		LineItems:                  lis,
		ExternalAccountIdentifiers: &google.ExternalAccountIdentifiers{ObfuscatedExternalAccountID: g.UserID},
		TestPurchase:               g.TestPurchase,
	}
	if g.Ack == "pending" {
		sub.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_PENDING"
	}
	if g.Token != "" {
		r.subs[g.Token] = sub
	}

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
	return body
}

func procKind(err error) string {
	switch {
	case err == nil:
		return "applied"
	case errors.Is(err, entitlements.ErrDuplicate):
		return "duplicate"
	case errors.Is(err, entitlements.ErrStale):
		return "stale"
	case errors.Is(err, entitlements.ErrIgnored):
		return "ignored"
	}
	return "error"
}

func (r *siteRig) stamp(c siteCard, code int) string {
	if c.Src == "apple" && c.Apple.Type == "CONSUMPTION_REQUEST" && !c.Forged {
		switch {
		case code == http.StatusInternalServerError:
			return "RETRY"
		case r.sent > 0:
			return "SENT"
		case r.respErr == nil:
			return "NOT SENT"
		}
		return "TOO LATE"
	}
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
		case errors.Is(r.parseErr, entitlements.ErrIgnored), errors.Is(r.parseErr, google.ErrPackageMismatch),
			errors.Is(r.parseErr, google.ErrPurchaseNotFound), errors.Is(r.parseErr, google.ErrPurchaseInvalid):
			return "IGNORED"
		case errors.Is(r.parseErr, entitlements.ErrUnsupported):
			return "SKIPPED"
		}
		return "?" + r.parseErr.Error()
	}
	for _, e := range r.procErrs {
		if procKind(e) == "error" {
			return "RETRY"
		}
	}
	if r.ackErr != nil {
		return "RETRY"
	}
	return map[string]string{"applied": "APPLIED", "duplicate": "DUPLICATE", "stale": "STALE", "ignored": "IGNORED"}[procKind(r.procErrs[0])]
}

func (r *siteRig) reset() {
	r.parseCalled, r.parseErr, r.procErrs = false, nil, nil
	r.ackErr, r.respErr, r.answered = nil, nil, false
	r.sent = 0
}

func (r *siteRig) send(c siteCard) siteExpect {
	r.reset()
	r.cur = c
	acksBefore, changesBefore := r.acks, r.changes
	var h http.Handler
	var body []byte
	if c.Src == "apple" {
		h, body = r.apple, r.appleBody(c)
	} else {
		h, body = r.google, r.registerGoogle(c)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if !c.NoAuth {
		req.Header.Set("X-Test-Push-Token", "ok")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := siteExpect{Code: rec.Code, Stamp: r.stamp(c, rec.Code), Acked: r.acks > acksBefore, Changes: r.changes - changesBefore}
	if len(r.procErrs) > 1 {
		kinds := make([]string, len(r.procErrs))
		for i, e := range r.procErrs {
			kinds[i] = procKind(e)
		}
		out.Detail = strings.Join(kinds, ",")
	}
	r.fill(&out)
	return out
}

func (r *siteRig) fill(out *siteExpect) {
	out.Chains, out.Access = r.chains(), r.access()
}

func (r *siteRig) chains() map[string]siteChain {
	out := map[string]siteChain{}
	seen := map[string]bool{}
	var keys []entitlements.Event
	for _, c := range r.fx.Cards {
		if c.Src == "apple" {
			keys = append(keys, entitlements.Event{Source: entitlements.SourceApple, OriginalTxID: c.Apple.Tx})
			continue
		}
		g := c.Google
		keys = append(keys, entitlements.Event{Source: entitlements.SourceGoogle, OriginalTxID: g.Token},
			entitlements.Event{Source: entitlements.SourceGoogle, OriginalTxID: g.Linked})
		for _, it := range g.Items {
			keys = append(keys,
				entitlements.Event{Source: entitlements.SourceGoogle, OriginalTxID: g.Token + "#" + it.Product},
				entitlements.Event{Source: entitlements.SourceGoogle, OriginalTxID: g.Token + "#" + it.Product + "#2"})
		}
	}
	for _, a := range r.fx.Actions {
		if a.Kind == "link" || a.Kind == "reconcile" {
			keys = append(keys, entitlements.Event{Source: entitlements.SourceApple, OriginalTxID: a.Tx})
		}
	}
	for _, k := range keys {
		key := string(k.Source) + "|" + k.OriginalTxID
		if k.OriginalTxID == "" || seen[key] {
			continue
		}
		seen[key] = true
		e, err := r.st.Store.Get(context.Background(), k.Source, k.OriginalTxID)
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
		out[key] = siteChain{State: string(e.State), Version: e.Version, Expires: date(e.ExpiresAt), Updated: date(e.UpdatedAt), User: e.UserID,
			Refunded: append([]string{}, e.RefundedTransactions...), AutoRenew: e.AutoRenew}
	}
	return out
}

func (r *siteRig) access() map[string]bool {
	out := map[string]bool{}
	was := r.st.down
	r.st.down = false
	defer func() { r.st.down = was }()
	for i, p := range r.fx.CheckProducts {
		for _, d := range r.fx.CheckDates {
			r.now = siteDate(r.t, d)
			ok, err := r.svc.IsActive(context.Background(), r.fx.User, p)
			require.NoError(r.t, err)
			key := d
			if i > 0 {
				key = d + " " + p
			}
			out[key] = ok
		}
	}
	return out
}

type siteStatusSource struct {
	s *serverapi.SubscriptionStatuses
}

func (s siteStatusSource) GetAllSubscriptionStatuses(context.Context, string, ...serverapi.Status) (*serverapi.SubscriptionStatuses, error) {
	return s.s, nil
}

func (r *siteRig) action(a siteAction) siteExpect {
	r.reset()
	ctx := context.Background()
	changesBefore := r.changes
	out := siteExpect{}
	switch a.Kind {
	case "link":
		src := entitlements.Source(a.Source)
		before, _ := r.st.Store.Get(ctx, src, a.Tx)
		ent, err := r.svc.LinkUser(ctx, src, a.Tx, a.User)
		switch {
		case errors.Is(err, entitlements.ErrUserConflict):
			out.Stamp = "CONFLICT"
		case errors.Is(err, entitlements.ErrNotFound):
			out.Stamp = "NOT FOUND"
		case err != nil:
			out.Stamp = "FAILED"
		case before != nil && before.Version == ent.Version:
			out.Stamp = "ALREADY LINKED"
		default:
			out.Stamp = "LINKED"
		}
	case "forget":
		n, err := r.svc.ForgetUser(ctx, a.User)
		if err != nil {
			out.Stamp = "FAILED"
		} else {
			out.Stamp, out.Detail = "ERASED", fmt.Sprintf("%d", n)
		}
	case "reconcile":
		t := r.t
		tx := apple.JWSTransactionDecodedPayload{
			OriginalTransactionID: a.Tx, TransactionID: a.Txn, ProductID: a.Product,
			ExpiresDate: siteMs(t, a.Expires), SignedDate: siteMs(t, a.Signed), AppAccountToken: a.User, Environment: "Production",
		}
		if a.Family {
			tx.InAppOwnershipType = "FAMILY_SHARED"
		}
		var ren *apple.JWSRenewalInfoDecodedPayload
		if a.GraceEnd != "" {
			ren = &apple.JWSRenewalInfoDecodedPayload{GracePeriodExpiresDate: siteMs(t, a.GraceEnd), SignedDate: siteMs(t, a.Signed)}
		}
		src := siteStatusSource{&serverapi.SubscriptionStatuses{Groups: []serverapi.SubscriptionGroup{{Subscriptions: []serverapi.SubscriptionStatus{
			{OriginalTransactionID: a.Tx, Status: serverapi.Status(a.Status), Transaction: tx, Renewal: ren},
		}}}}}
		res, _ := serverapi.Reconcile(ctx, src, r.svc, a.Tx)
		out.Stamp = "NONE"
		var outs []string
		for _, x := range res {
			outs = append(outs, string(x.Outcome))
		}
		if len(res) > 0 {
			out.Stamp = strings.ToUpper(string(res[0].Outcome))
			out.Detail = strings.Join(outs, ",")
		}
	default:
		r.t.Fatalf("unknown action kind %q", a.Kind)
	}
	out.Changes = r.changes - changesBefore
	r.fill(&out)
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
	actions := map[string]siteAction{}
	for _, a := range fx.Actions {
		actions[a.ID] = a
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
			rig := newSiteRig(t, &fx, sc, trusted, attacker)
			docSteps := docScenarios[si].(map[string]any)["steps"].([]any)
			for i, st := range sc.Steps {
				var got siteExpect
				switch {
				case st.DB != "":
					rig.st.down = st.DB == "down"
					continue
				case st.PlayDown != nil:
					rig.playDown = *st.PlayDown
					continue
				case st.Ack != "":
					rig.ackDown = st.Ack == "down"
					continue
				case st.Register != "":
					c, ok := cards[st.Register]
					require.True(t, ok, "unknown card %q", st.Register)
					rig.registerGoogle(c)
					continue
				case st.Do != "":
					a, ok := actions[st.Do]
					require.True(t, ok, "unknown action %q", st.Do)
					got = rig.action(a)
				default:
					c, ok := cards[st.Send]
					require.True(t, ok, "unknown card %q", st.Send)
					got = rig.send(c)
					require.False(t, strings.HasPrefix(got.Stamp, "?"), "step %d %s: unclassified parse error %s", i, st.Send, got.Stamp)
				}
				if *updateSite {
					var m map[string]any
					b, _ := json.Marshal(got)
					require.NoError(t, json.Unmarshal(b, &m))
					docSteps[i].(map[string]any)["expect"] = m
					continue
				}
				var want siteExpect
				require.NotEmpty(t, st.Expect, "step %d has no expectation; run with -update-site", i)
				require.NoError(t, json.Unmarshal(st.Expect, &want))
				require.Equal(t, want, got, "scenario %q step %d", sc.Name, i)
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
