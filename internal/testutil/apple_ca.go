// Package testutil holds test helpers shared across packages.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

var (
	oidAppleLeaf         = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidAppleIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}
)

// AppleCAOptions tweaks the generated chain to produce invalid variants.
type AppleCAOptions struct {
	OmitLeafOID         bool
	OmitIntermediateOID bool
	ExpiredLeaf         bool
	// ExtraIntermediate puts one more CA between the root and the Apple-style
	// intermediate, so x5c has four certificates: leaf, intermediate, upper, root.
	ExtraIntermediate bool
	// LeafP384 gives the leaf certificate a P-384 key. Sign cannot use it (it
	// produces ES256 signatures); sign with SignWithKey and another key.
	LeafP384 bool
}

// AppleCA is a throwaway root -> intermediate -> leaf chain shaped like
// Apple's receipt-signing chain.
type AppleCA struct {
	Root         *x509.Certificate
	Intermediate *x509.Certificate
	Leaf         *x509.Certificate
	Upper        *x509.Certificate // CA between root and Intermediate, nil unless ExtraIntermediate
	LeafKey      *ecdsa.PrivateKey
	Pool         *x509.CertPool // contains Root only
}

func newKey(tb testing.TB) *ecdsa.PrivateKey {
	tb.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

func sign(tb testing.TB, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
	tb.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		tb.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

var nextSerial int64 = 1000

func serial() *big.Int { nextSerial++; return big.NewInt(nextSerial) }

// NewAppleCA generates a fresh chain valid around time.Now().
func NewAppleCA(tb testing.TB, opts ...AppleCAOptions) *AppleCA {
	tb.Helper()
	var o AppleCAOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	now := time.Now()
	nullExt := []byte{0x05, 0x00}

	rootKey, interKey, leafKey := newKey(tb), newKey(tb), newKey(tb)
	if o.LeafP384 {
		var err error
		if leafKey, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader); err != nil {
			tb.Fatal(err)
		}
	}
	rootTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Test Apple Root CA"},
		NotBefore: now.Add(-72 * time.Hour), NotAfter: now.Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	root := sign(tb, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)

	interTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Test Apple WWDR Intermediate"},
		NotBefore: now.Add(-72 * time.Hour), NotAfter: now.Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	if !o.OmitIntermediateOID {
		interTmpl.ExtraExtensions = []pkix.Extension{{Id: oidAppleIntermediate, Value: nullExt}}
	}
	var upper *x509.Certificate
	issuer, issuerKey := root, rootKey
	if o.ExtraIntermediate {
		upperKey := newKey(tb)
		upperTmpl := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: "Test Extra Intermediate"},
			NotBefore: now.Add(-72 * time.Hour), NotAfter: now.Add(24 * time.Hour * 365),
			IsCA: true, BasicConstraintsValid: true, MaxPathLen: 1, KeyUsage: x509.KeyUsageCertSign,
		}
		upper = sign(tb, upperTmpl, root, &upperKey.PublicKey, rootKey)
		issuer, issuerKey = upper, upperKey
	}
	inter := sign(tb, interTmpl, issuer, &interKey.PublicKey, issuerKey)

	leafTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Test Prod ECC Mac App Store Receipt Signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if o.ExpiredLeaf {
		leafTmpl.NotBefore, leafTmpl.NotAfter = now.Add(-48*time.Hour), now.Add(-24*time.Hour)
	}
	if !o.OmitLeafOID {
		leafTmpl.ExtraExtensions = []pkix.Extension{{Id: oidAppleLeaf, Value: nullExt}}
	}
	leaf := sign(tb, leafTmpl, inter, &leafKey.PublicKey, interKey)

	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &AppleCA{Root: root, Intermediate: inter, Upper: upper, Leaf: leaf, LeafKey: leafKey, Pool: pool}
}

// RootPEM returns the root certificate in PEM form.
func (ca *AppleCA) RootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw})
}

// Sign produces a real ES256 JWS over claims with the x5c chain
// [leaf, intermediate, root] in the header.
func (ca *AppleCA) Sign(tb testing.TB, claims any) string {
	tb.Helper()
	return ca.SignWithKey(tb, ca.LeafKey, claims)
}

// SignWithKey is like Sign but signs with key instead of the leaf key
// (to simulate a forged signature).
func (ca *AppleCA) SignWithKey(tb testing.TB, key *ecdsa.PrivateKey, claims any) string {
	tb.Helper()
	return SignChain(tb, key, ca.Chain(), claims)
}

// Chain returns the x5c certificates in header order: leaf, intermediate,
// the extra intermediate if any, root.
func (ca *AppleCA) Chain() [][]byte {
	chain := [][]byte{ca.Leaf.Raw, ca.Intermediate.Raw}
	if ca.Upper != nil {
		chain = append(chain, ca.Upper.Raw)
	}
	return append(chain, ca.Root.Raw)
}

// SignChain produces an ES256 JWS signed with key and the given DER
// certificates as the x5c header.
func SignChain(tb testing.TB, key *ecdsa.PrivateKey, x5c [][]byte, claims any) string {
	tb.Helper()
	cb, err := json.Marshal(claims)
	if err != nil {
		tb.Fatal(err)
	}
	return signRaw(tb, key, x5c, cb)
}

func signRaw(tb testing.TB, key *ecdsa.PrivateKey, x5c [][]byte, payload []byte) string {
	tb.Helper()
	certs := make([]string, len(x5c))
	for i, der := range x5c {
		certs[i] = base64.StdEncoding.EncodeToString(der)
	}
	return signRawHeader(tb, key, map[string]any{"alg": "ES256", "x5c": certs}, payload)
}

// SignRawHeader signs payload with key under an arbitrary JWS header, for
// tests that tamper with header fields such as alg.
func SignRawHeader(tb testing.TB, key *ecdsa.PrivateKey, hdr map[string]any, payload []byte) string {
	tb.Helper()
	return signRawHeader(tb, key, hdr, payload)
}

func signRawHeader(tb testing.TB, key *ecdsa.PrivateKey, hdr map[string]any, cb []byte) string {
	tb.Helper()
	hb, err := json.Marshal(hdr)
	if err != nil {
		tb.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		tb.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// NewAppleKey returns a fresh P-256 key (for forged-signature tests).
func NewAppleKey(tb testing.TB) *ecdsa.PrivateKey {
	tb.Helper()
	return newKey(tb)
}

// SignRaw is like Sign but takes the payload bytes verbatim, valid JSON or
// not. For fuzzing and malformed-payload tests.
func (ca *AppleCA) SignRaw(tb testing.TB, payload []byte) string {
	tb.Helper()
	return signRaw(tb, ca.LeafKey, ca.Chain(), payload)
}
