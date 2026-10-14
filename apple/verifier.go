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

var (
	// Apple's receipt-signing marker extensions.
	oidLeaf         = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}

	defaultPoolOnce sync.Once
	defaultPool     *x509.CertPool
)

// AppleRootCAG3Pool returns a pool containing only the embedded
// "Apple Root CA - G3" certificate.
func AppleRootCAG3Pool() *x509.CertPool {
	defaultPoolOnce.Do(func() {
		cert, err := x509.ParseCertificate(appleRootCAG3DER)
		if err != nil {
			panic("apple: embedded root CA is invalid: " + err.Error())
		}
		defaultPool = x509.NewCertPool()
		defaultPool.AddCert(cert)
	})
	return defaultPool
}

// Verifier verifies Apple-signed JWS values (ES256 with an x5c chain).
type Verifier struct {
	roots       *x509.CertPool
	timeFn      func() time.Time
	bundleID    string
	environment string
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithRootPool replaces the trusted roots (default: embedded Apple Root CA - G3).
func WithRootPool(p *x509.CertPool) Option { return func(v *Verifier) { v.roots = p } }

// WithTimeFn sets the clock used to check certificate validity (default
// time.Now). Apple's leaf certificates are short-lived, so verifying an old
// notification later (e.g. replaying stored payloads) may need a clock pinned
// to the notification's signedDate; the library does not do that implicitly
// because signedDate is attacker-controlled until the signature is checked.
func WithTimeFn(fn func() time.Time) Option { return func(v *Verifier) { v.timeFn = fn } }

// WithBundleID rejects notifications whose data.bundleId differs.
func WithBundleID(id string) Option { return func(v *Verifier) { v.bundleID = id } }

// WithEnvironment rejects notifications whose data.environment differs
// ("Production" or "Sandbox").
func WithEnvironment(env string) Option { return func(v *Verifier) { v.environment = env } }

// NewVerifier builds a Verifier. Without options it trusts only Apple Root CA - G3.
func NewVerifier(opts ...Option) *Verifier {
	v := &Verifier{}
	for _, o := range opts {
		o(v)
	}
	if v.roots == nil {
		v.roots = AppleRootCAG3Pool()
	}
	if v.timeFn == nil {
		v.timeFn = time.Now
	}
	return v
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
	if err := json.Unmarshal(hdrRaw, &hdr); err != nil {
		return invalid("header json: %v", err)
	}
	if hdr.Alg != "ES256" {
		return invalid("unsupported alg %q", hdr.Alg)
	}
	if len(hdr.X5C) < 2 {
		return invalid("x5c must contain leaf and intermediate, got %d certs", len(hdr.X5C))
	}
	certs := make([]*x509.Certificate, len(hdr.X5C))
	for i, s := range hdr.X5C {
		der, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return invalid("x5c[%d] encoding: %v", i, err)
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
	if _, err := leaf.Verify(x509.VerifyOptions{
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
	d := n.Payload.Data
	if d == nil {
		return n, nil
	}
	if v.bundleID != "" && d.BundleID != v.bundleID {
		return nil, invalid("bundleId %q, want %q", d.BundleID, v.bundleID)
	}
	if v.environment != "" && d.Environment != v.environment {
		return nil, invalid("environment %q, want %q", d.Environment, v.environment)
	}
	if d.SignedTransactionInfo != "" {
		n.Transaction = &JWSTransactionDecodedPayload{}
		if err := v.Verify(d.SignedTransactionInfo, n.Transaction); err != nil {
			return nil, fmt.Errorf("signedTransactionInfo: %w", err)
		}
	}
	if d.SignedRenewalInfo != "" {
		n.Renewal = &JWSRenewalInfoDecodedPayload{}
		if err := v.Verify(d.SignedRenewalInfo, n.Renewal); err != nil {
			return nil, fmt.Errorf("signedRenewalInfo: %w", err)
		}
	}
	return n, nil
}
