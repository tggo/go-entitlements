package apple

import (
	"errors"

	"github.com/tggo/go-entitlements"
)

// Parser verifies, decodes and maps App Store Server Notifications V2.
type Parser struct {
	verifier *Verifier
}

// NewParser returns a Parser using v. A nil v makes every Parse fail: there
// is no default verifier because the bundle ID must be chosen explicitly.
func NewParser(v *Verifier) *Parser { return &Parser{verifier: v} }

// Parse turns a raw request body into an Event. Verification failures wrap
// entitlements.ErrInvalidSignature; TEST yields ErrIgnored and unknown types
// ErrUnsupported.
func (p *Parser) Parse(body []byte) (entitlements.Event, error) {
	if p.verifier == nil {
		return entitlements.Event{}, errors.New("apple: parser has no verifier")
	}
	n, err := p.verifier.VerifyNotification(body)
	if err != nil {
		return entitlements.Event{}, err
	}
	ev, err := ToEvent(n)
	if err != nil {
		return entitlements.Event{}, err
	}
	ev.Raw = append([]byte(nil), body...)
	return ev, nil
}
