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
}

// AppleCA is a throwaway root -> intermediate -> leaf chain shaped like
// Apple's receipt-signing chain.
type AppleCA struct {
	Root         *x509.Certificate
	Intermediate *x509.Certificate
	Leaf         *x509.Certificate
	LeafKey      *ecdsa.PrivateKey
	Pool         *x509.CertPool // contains Root only
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t testing.TB, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var nextSerial int64 = 1000

func serial() *big.Int { nextSerial++; return big.NewInt(nextSerial) }

// NewAppleCA generates a fresh chain valid around time.Now().
func NewAppleCA(t testing.TB, opts ...AppleCAOptions) *AppleCA {
	t.Helper()
	var o AppleCAOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	now := time.Now()
	nullExt := []byte{0x05, 0x00}

	rootKey, interKey, leafKey := newKey(t), newKey(t), newKey(t)
	rootTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Test Apple Root CA"},
		NotBefore: now.Add(-72 * time.Hour), NotAfter: now.Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	root := sign(t, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)

	interTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Test Apple WWDR Intermediate"},
		NotBefore: now.Add(-72 * time.Hour), NotAfter: now.Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	if !o.OmitIntermediateOID {
		interTmpl.ExtraExtensions = []pkix.Extension{{Id: oidAppleIntermediate, Value: nullExt}}
	}
	inter := sign(t, interTmpl, root, &interKey.PublicKey, rootKey)

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
	leaf := sign(t, leafTmpl, inter, &leafKey.PublicKey, interKey)

	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &AppleCA{Root: root, Intermediate: inter, Leaf: leaf, LeafKey: leafKey, Pool: pool}
}

// RootPEM returns the root certificate in PEM form.
func (ca *AppleCA) RootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw})
}

// Sign produces a real ES256 JWS over claims with the x5c chain
// [leaf, intermediate, root] in the header.
func (ca *AppleCA) Sign(t testing.TB, claims any) string {
	t.Helper()
	return ca.SignWithKey(t, ca.LeafKey, claims)
}

// SignWithKey is like Sign but signs with key instead of the leaf key
// (to simulate a forged signature).
func (ca *AppleCA) SignWithKey(t testing.TB, key *ecdsa.PrivateKey, claims any) string {
	t.Helper()
	hdr := map[string]any{
		"alg": "ES256",
		"x5c": []string{
			base64.StdEncoding.EncodeToString(ca.Leaf.Raw),
			base64.StdEncoding.EncodeToString(ca.Intermediate.Raw),
			base64.StdEncoding.EncodeToString(ca.Root.Raw),
		},
	}
	hb, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// NewAppleKey returns a fresh P-256 key (for forged-signature tests).
func NewAppleKey(t testing.TB) *ecdsa.PrivateKey { return newKey(t) }
