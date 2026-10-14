package serverapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

const (
	audience = "appstoreconnect-v1"

	// maxTokenTTL is the longest lifetime Apple accepts: tokens that expire
	// more than 60 minutes after iat are invalid.
	maxTokenTTL = time.Hour

	defaultTokenTTL = 5 * time.Minute
)

// signer creates the ES256 JWTs that authorize App Store Server API calls.
// The private key never leaves this type: it has no exported accessors and
// formats as a placeholder.
type signer struct {
	issuerID string
	keyID    string
	bundleID string
	key      *ecdsa.PrivateKey
}

func (s *signer) String() string { return "serverapi.signer{key: [redacted]}" }

// GoString keeps %#v from printing the key.
func (s *signer) GoString() string { return s.String() }

// ParsePrivateKey decodes an App Store Connect API key (the AuthKey_XXXX.p8
// file): a PKCS#8 PEM holding an ECDSA P-256 key.
func ParsePrivateKey(p8 []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("serverapi: private key is not PEM encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Deliberately not wrapping err: parser errors are safe, but the
		// caller only needs to know the key is unusable.
		return nil, errors.New("serverapi: private key is not PKCS#8")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("serverapi: private key is not an ECDSA P-256 key")
	}
	return key, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Aud string `json:"aud"`
	Bid string `json:"bid"`
}

// token returns a JWT issued at now and valid for ttl.
func (s *signer) token(now time.Time, ttl time.Duration) (string, error) {
	hdr, err := json.Marshal(jwtHeader{Alg: "ES256", Kid: s.keyID, Typ: "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(jwtClaims{
		Iss: s.issuerID,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
		Aud: audience,
		Bid: s.bundleID,
	})
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("serverapi: sign token: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
