package serverapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// Base URLs of the App Store Server API.
const (
	ProductionURL = "https://api.storekit.apple.com"
	SandboxURL    = "https://api.storekit-sandbox.apple.com"
)

const (
	defaultMaxRetries   = 3
	defaultBackoff      = 500 * time.Millisecond
	defaultMaxRetryWait = 30 * time.Second
	defaultHTTPTimeout  = 30 * time.Second
	maxResponseBytes    = 16 << 20
)

// Config identifies the app and the App Store Connect API key used to call
// the App Store Server API (Users and Access > Integrations > In-App
// Purchase in App Store Connect).
type Config struct {
	// IssuerID is the issuer ID shown on the keys page.
	IssuerID string
	// KeyID is the ID of the in-app purchase key (JWT "kid").
	KeyID string
	// PrivateKeyPEM is the content of the downloaded AuthKey_<KeyID>.p8 file.
	PrivateKeyPEM []byte
	// BundleID is the app's bundle ID (JWT "bid"). Every signed value the
	// API returns must belong to it.
	BundleID string
	// Environment selects the host: EnvironmentProduction (the zero value
	// also means production) or EnvironmentSandbox.
	Environment entitlements.Environment
}

// String hides the private key, so a Config that ends up in a log line or a
// panic message does not leak it.
func (Config) String() string { return "serverapi.Config{PrivateKeyPEM: [redacted]}" }

// GoString keeps %#v from printing the key.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from printing the key.
func (Config) LogValue() slog.Value { return slog.StringValue("serverapi.Config{[redacted]}") }

// Client calls the App Store Server API. It is safe for concurrent use.
type Client struct {
	signer      *signer
	env         entitlements.Environment
	bundleID    string
	base        string
	http        *http.Client
	verifier    *apple.Verifier
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	log         *slog.Logger
	maxRetries  int
	backoff     time.Duration
	maxWait     time.Duration
	tokenTTL    time.Duration
	verifierSet bool
}

// Option configures a Client. Nil options are ignored.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client (default: a client with a 30 s
// timeout). A nil client keeps the default.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithClock injects the clock used for the JWT iat/exp claims, Retry-After
// arithmetic and the default Verifier's certificate checks. A nil fn keeps
// time.Now.
func WithClock(fn func() time.Time) Option {
	return func(c *Client) {
		if fn != nil {
			c.now = fn
		}
	}
}

// WithBaseURL overrides the API host (for a proxy or a test server). The
// Environment in Config still decides what the responses must claim.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.base = strings.TrimRight(u, "/")
		}
	}
}

// WithVerifier replaces the Verifier used for every signed value the API
// returns (default: apple.NewVerifier(BundleID) pinned to Config.Environment,
// trusting Apple Root CA - G3). Use it to trust a test root, or to pin the
// certificate clock when decoding old notification history. The Client
// checks the bundle ID and environment of every decoded value itself, so a
// Verifier built WithInsecureAnyBundle does not weaken that.
func WithVerifier(v *apple.Verifier) Option {
	return func(c *Client) {
		if v != nil {
			c.verifier = v
			c.verifierSet = true
		}
	}
}

// WithLogger sets a logger for request outcomes (method, path, status,
// attempt). Tokens, keys and response bodies are never logged. The default
// discards.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithMaxRetries sets how many times a retryable failure (429, 5xx, a
// transport error, or one of Apple's "try again" error codes) is repeated
// (default 3). Zero disables retries.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = max(n, 0) }
}

// WithBackoff sets the first retry delay (default 500 ms); it doubles on each
// retry, with jitter, and is capped by WithMaxRetryWait.
func WithBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.backoff = d
		}
	}
}

// WithMaxRetryWait caps how long the client sleeps before a retry (default
// 30 s). A Retry-After beyond the cap is not waited out: the call fails with
// ErrRateLimited and APIError.RetryAfter tells the caller when to come back.
func WithMaxRetryWait(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.maxWait = d
		}
	}
}

// WithTokenTTL sets how long each JWT is valid (default 5 minutes, at most
// 60: Apple rejects tokens that expire more than an hour after iat). A new
// token is signed for every request.
func WithTokenTTL(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.tokenTTL = d
		}
	}
}

// New returns a Client. It validates the configuration and parses the key;
// it makes no network call.
func New(cfg Config, opts ...Option) (*Client, error) {
	switch {
	case cfg.IssuerID == "":
		return nil, errors.New("serverapi: IssuerID is required")
	case cfg.KeyID == "":
		return nil, errors.New("serverapi: KeyID is required")
	case cfg.BundleID == "":
		return nil, errors.New("serverapi: BundleID is required")
	}
	key, err := ParsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	c := &Client{
		signer:     &signer{issuerID: cfg.IssuerID, keyID: cfg.KeyID, bundleID: cfg.BundleID, key: key},
		env:        cfg.Environment,
		bundleID:   cfg.BundleID,
		http:       &http.Client{Timeout: defaultHTTPTimeout},
		now:        time.Now,
		sleep:      sleepCtx,
		log:        slog.New(slog.DiscardHandler),
		maxRetries: defaultMaxRetries,
		backoff:    defaultBackoff,
		maxWait:    defaultMaxRetryWait,
		tokenTTL:   defaultTokenTTL,
	}
	switch c.env {
	case "", entitlements.EnvironmentProduction:
		c.env = entitlements.EnvironmentProduction
		c.base = ProductionURL
	case entitlements.EnvironmentSandbox:
		c.base = SandboxURL
	default:
		return nil, fmt.Errorf("serverapi: unsupported environment %q: use EnvironmentProduction or EnvironmentSandbox", cfg.Environment)
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.tokenTTL > maxTokenTTL {
		return nil, fmt.Errorf("serverapi: token TTL %s exceeds Apple's limit of %s", c.tokenTTL, maxTokenTTL)
	}
	if !c.verifierSet {
		c.verifier, err = apple.NewVerifier(cfg.BundleID, apple.WithEnvironment(c.env), apple.WithTimeFn(c.now))
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Environment is the environment the client talks to.
func (c *Client) Environment() entitlements.Environment { return c.env }

// BundleID is the app the client is bound to.
func (c *Client) BundleID() string { return c.bundleID }

// String hides the key material.
func (c *Client) String() string {
	return fmt.Sprintf("serverapi.Client{bundle: %s, env: %s, key: [redacted]}", c.bundleID, c.env)
}

// GoString keeps %#v from printing the key.
func (c *Client) GoString() string { return c.String() }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do sends one logical request, retrying retryable failures, and decodes a
// 2xx JSON body into out (nil to discard it).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("serverapi: encode request: %w", err)
		}
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, path, target, payload, out)
		if err == nil {
			return nil
		}
		wait, ok := c.retryWait(ctx, err, attempt)
		if !ok {
			return err
		}
		c.log.WarnContext(ctx, "serverapi: retrying", "method", method, "path", path, "attempt", attempt+1, "wait", wait, "error", err)
		if serr := c.sleep(ctx, wait); serr != nil {
			return fmt.Errorf("%w (giving up: %w)", err, serr)
		}
	}
}

// retryWait decides whether err is worth another attempt and for how long
// to sleep first.
func (c *Client) retryWait(ctx context.Context, err error, attempt int) (time.Duration, bool) {
	if attempt >= c.maxRetries || ctx.Err() != nil {
		return 0, false
	}
	var ae *APIError
	switch {
	case errors.As(err, &ae):
		if !ae.Retryable() {
			return 0, false
		}
		if ae.RetryAfter > 0 {
			if ae.RetryAfter > c.maxWait {
				return 0, false
			}
			return ae.RetryAfter, true
		}
	case errors.Is(err, errTransport):
	default:
		return 0, false
	}
	d := c.backoff << attempt
	if d <= 0 || d > c.maxWait {
		d = c.maxWait
	}
	// Jitter within [d/2, d] keeps parallel callers from retrying in step.
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)), true
}

var errTransport = errors.New("transport error")

func (c *Client) once(ctx context.Context, method, path, target string, payload []byte, out any) error {
	tok, err := c.signer.token(c.now(), c.tokenTTL)
	if err != nil {
		return err
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return fmt.Errorf("serverapi: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error repeats the URL (no secrets in it); the token is a header.
		return fmt.Errorf("serverapi: %s %s: %w: %w", method, path, errTransport, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("serverapi: %s %s: read body: %w: %w", method, path, errTransport, err)
	}
	c.log.DebugContext(ctx, "serverapi: response", "method", method, "path", path, "status", resp.StatusCode)
	if resp.StatusCode/100 != 2 {
		return c.apiError(method, path, resp, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("serverapi: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *Client) apiError(method, path string, resp *http.Response, raw []byte) *APIError {
	ae := &APIError{Method: method, Path: path, StatusCode: resp.StatusCode}
	var body struct {
		ErrorCode    int64  `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(raw, &body) == nil {
		ae.Code, ae.Message = body.ErrorCode, body.ErrorMessage
	}
	if h := resp.Header.Get("Retry-After"); h != "" {
		if d, ok := parseRetryAfter(h, c.now()); ok {
			ae.RetryAfter = d
		}
	}
	return ae
}

// parseRetryAfter reads the Retry-After header. Apple documents it as a UNIX
// time in milliseconds; the standard forms (delay in seconds, HTTP date) are
// accepted too. A date in the past is a zero wait.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if n, err := strconv.ParseInt(h, 10, 64); err == nil && n >= 0 {
		// A delay in seconds is small; a millisecond timestamp is ~1.7e12.
		const msThreshold = 1e11
		if n >= msThreshold {
			return max(time.UnixMilli(n).Sub(now), 0), true
		}
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}
