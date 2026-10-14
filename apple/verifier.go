package apple

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/tggo/go-entitlements"
)

//go:embed AppleRootCA-G3.cer
var appleRootCAG3DER []byte

// maxX5C bounds how many certificates of an untrusted header are parsed.
// Apple sends three (leaf, intermediate, root).
const maxX5C = 5

var (
	// Apple's receipt-signing marker extensions.
	oidLeaf         = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}

	defaultPoolOnce sync.Once
	defaultPool     *x509.CertPool
)

// RootCAG3Pool returns a new pool containing only the embedded
// "Apple Root CA - G3" certificate. Each call returns an independent copy, so
// callers may add certificates to it. It panics if the embedded certificate is
// corrupt, which the tests rule out.
func RootCAG3Pool() *x509.CertPool {
	defaultPoolOnce.Do(func() {
		cert, err := x509.ParseCertificate(appleRootCAG3DER)
		if err != nil {
			panic("apple: embedded root CA is invalid: " + err.Error())
		}
		defaultPool = x509.NewCertPool()
		defaultPool.AddCert(cert)
	})
	return defaultPool.Clone()
}

// Verifier verifies Apple-signed JWS values (ES256 with an x5c chain) and
// pins them to one app (bundle ID) and, optionally, one environment.
type Verifier struct {
	roots       *x509.CertPool
	timeFn      func() time.Time
	bundleID    string
	environment entitlements.Environment
	anyBundle   bool
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithRootPool replaces the trusted roots (default: embedded Apple Root CA - G3).
// The Verifier keeps p without copying it: do not modify p afterwards
// (x509.CertPool is not safe for concurrent mutation). A nil pool keeps the default.
func WithRootPool(p *x509.CertPool) Option {
	return func(v *Verifier) {
		if p != nil {
			v.roots = p
		}
	}
}

// WithTimeFn sets the clock used to check certificate validity (default
// time.Now). Apple's leaf certificates are short-lived, so verifying an old
// notification later (e.g. replaying stored payloads) may need a clock pinned
// to the notification's signedDate; the library does not do that implicitly
// because signedDate is attacker-controlled until the signature is checked.
// A nil fn keeps time.Now.
func WithTimeFn(fn func() time.Time) Option {
	return func(v *Verifier) {
		if fn != nil {
			v.timeFn = fn
		}
	}
}

// WithEnvironment rejects notifications (outer and nested) whose environment
// differs: EnvironmentProduction or EnvironmentSandbox (NewVerifier rejects
// any other value). Without it both are accepted and nested payloads must
// still match the outer notification's environment. A production service
// should pin EnvironmentProduction: sandbox purchases are free, and
// entitlements.Service ignores sandbox events unless built WithAllowSandbox.
func WithEnvironment(env entitlements.Environment) Option {
	return func(v *Verifier) { v.environment = env }
}

// WithInsecureAnyBundle lets NewVerifier accept an empty bundle ID and skip
// the bundle check. Any Apple-signed notification from any app then passes
// signature verification; only use it in tools, never in a webhook.
func WithInsecureAnyBundle() Option { return func(v *Verifier) { v.anyBundle = true } }

// NewVerifier builds a Verifier for one app. bundleID is required: it is
// compared with the bundle ID of the notification and of the nested
// transaction and renewal info. Without WithRootPool it trusts only
// Apple Root CA - G3.
func NewVerifier(bundleID string, opts ...Option) (*Verifier, error) {
	v := &Verifier{bundleID: bundleID}
	for _, o := range opts {
		if o != nil {
			o(v)
		}
	}
	switch v.environment {
	case "", entitlements.EnvironmentProduction, entitlements.EnvironmentSandbox:
	default:
		return nil, fmt.Errorf("apple: unsupported environment %q: use entitlements.EnvironmentProduction or EnvironmentSandbox", v.environment)
	}
	if v.bundleID == "" && !v.anyBundle {
		return nil, errors.New("apple: bundle ID is required (use WithInsecureAnyBundle to skip the check)")
	}
	if v.roots == nil {
		v.roots = RootCAG3Pool()
	}
	if v.timeFn == nil {
		v.timeFn = time.Now
	}
	return v, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", entitlements.ErrInvalidSignature, fmt.Sprintf(format, args...))
}

type jwsHeader struct {
	Alg string   `json:"alg"`
	X5C []string `json:"x5c"`
}

// Verify checks the JWS signature and certificate chain of token and
// unmarshals its payload into out. The x5c header is never trusted on its
// own: it must chain to the configured root pool and carry Apple's OIDs.
func (v *Verifier) Verify(token string, out any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return invalid("malformed JWS: want 3 segments, got %d", len(parts))
	}
	hdrRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return invalid("header encoding: %v", err)
	}
	var hdr jwsHeader
	if err = json.Unmarshal(hdrRaw, &hdr); err != nil {
		return invalid("header json: %v", err)
	}
	if hdr.Alg != "ES256" {
		return invalid("unsupported alg %q", hdr.Alg)
	}
	if len(hdr.X5C) < 2 {
		return invalid("x5c must contain leaf and intermediate, got %d certs", len(hdr.X5C))
	}
	if len(hdr.X5C) > maxX5C {
		return invalid("x5c has %d certs, at most %d allowed", len(hdr.X5C), maxX5C)
	}
	certs := make([]*x509.Certificate, len(hdr.X5C))
	for i, s := range hdr.X5C {
		der, derr := base64.StdEncoding.DecodeString(s)
		if derr != nil {
			return invalid("x5c[%d] encoding: %v", i, derr)
		}
		if certs[i], err = x509.ParseCertificate(der); err != nil {
			return invalid("x5c[%d] parse: %v", i, err)
		}
	}
	leaf, inter := certs[0], certs[1]
	if !hasExtension(leaf, oidLeaf) {
		return invalid("leaf certificate lacks Apple OID %s", oidLeaf)
	}
	if !hasExtension(inter, oidIntermediate) {
		return invalid("intermediate certificate lacks Apple OID %s", oidIntermediate)
	}
	inters := x509.NewCertPool()
	for _, c := range certs[1:] {
		inters.AddCert(c)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{
		Roots:         v.roots,
		Intermediates: inters,
		CurrentTime:   v.timeFn(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return invalid("certificate chain: %v", err)
	}

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return invalid("leaf key is not ECDSA P-256")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return invalid("signature encoding: %v", err)
	}
	if len(sig) != 64 {
		return invalid("signature length %d, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return invalid("signature mismatch")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return invalid("payload encoding: %v", err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("apple: decode payload: %w", err)
	}
	return nil
}

func hasExtension(c *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	for _, e := range c.Extensions {
		if e.Id.Equal(oid) {
			return true
		}
	}
	return false
}

// VerifyNotification verifies body (a ResponseBodyV2 JSON document) and the
// nested transaction/renewal JWS values, and applies the bundleID and
// environment checks.
func (v *Verifier) VerifyNotification(body []byte) (*Notification, error) {
	var rb ResponseBodyV2
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&rb); err != nil {
		return nil, fmt.Errorf("apple: decode body: %w", err)
	}
	if rb.SignedPayload == "" {
		return nil, errors.New("apple: empty signedPayload")
	}
	n := &Notification{}
	if err := v.Verify(rb.SignedPayload, &n.Payload); err != nil {
		return nil, err
	}
	outerEnv, err := v.checkOuterIdentity(&n.Payload)
	if err != nil {
		return nil, err
	}
	d := n.Payload.Data
	if d == nil {
		return n, nil
	}
	if d.SignedTransactionInfo != "" {
		n.Transaction = &JWSTransactionDecodedPayload{}
		if err := v.Verify(d.SignedTransactionInfo, n.Transaction); err != nil {
			return nil, fmt.Errorf("signedTransactionInfo: %w", err)
		}
		if err := v.checkNested("signedTransactionInfo", n.Transaction.BundleID, n.Transaction.Environment, outerEnv); err != nil {
			return nil, err
		}
	}
	if d.SignedRenewalInfo != "" {
		n.Renewal = &JWSRenewalInfoDecodedPayload{}
		if err := v.Verify(d.SignedRenewalInfo, n.Renewal); err != nil {
			return nil, fmt.Errorf("signedRenewalInfo: %w", err)
		}
		// Renewal info has no bundleId field; only its environment is checked.
		if err := v.checkNested("signedRenewalInfo", v.bundleID, n.Renewal.Environment, outerEnv); err != nil {
			return nil, err
		}
	}
	return n, nil
}

type identity struct {
	BundleID    string `json:"bundleId"`
	Environment string `json:"environment"`
}

// checkOuterIdentity finds the notification's bundle ID and environment —
// in data, summary, externalPurchaseToken or appData, depending on the
// type — and compares them with the pins. It returns the outer environment.
func (v *Verifier) checkOuterIdentity(p *NotificationPayload) (string, error) {
	var id identity
	found := false
	if p.Data != nil {
		id, found = identity{p.Data.BundleID, p.Data.Environment}, true
	}
	for _, raw := range []json.RawMessage{p.Summary, p.ExternalPurchaseToken, p.AppData} {
		if found || len(raw) == 0 {
			continue
		}
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", invalid("notification identity: %v", err)
		}
		found = true
	}
	if v.bundleID != "" && (!found || id.BundleID != v.bundleID) {
		return "", invalid("bundleId %q, want %q", id.BundleID, v.bundleID)
	}
	if v.environment != "" && entitlements.Environment(id.Environment) != v.environment {
		return "", invalid("environment %q, want %q", id.Environment, v.environment)
	}
	return id.Environment, nil
}

// checkNested applies the same pins to a nested payload and requires its
// environment to equal the outer one (a sandbox transaction cannot ride
// inside a production notification).
func (v *Verifier) checkNested(what, bundleID, env, outerEnv string) error {
	if v.bundleID != "" && bundleID != v.bundleID {
		return invalid("%s bundleId %q, want %q", what, bundleID, v.bundleID)
	}
	if v.environment != "" && entitlements.Environment(env) != v.environment {
		return invalid("%s environment %q, want %q", what, env, v.environment)
	}
	if env != "" && outerEnv != "" && env != outerEnv {
		return invalid("%s environment %q differs from notification environment %q", what, env, outerEnv)
	}
	return nil
}
