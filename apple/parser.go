package apple

import "github.com/tggo/go-entitlements"

// Parser verifies, decodes and maps App Store Server Notifications V2.
type Parser struct {
	verifier *Verifier
}

// NewParser returns a Parser using v (a default Verifier if nil).
func NewParser(v *Verifier) *Parser {
	if v == nil {
		v = NewVerifier()
	}
	return &Parser{verifier: v}
}

// Parse turns a raw request body into an Event. Verification failures wrap
// entitlements.ErrInvalidSignature; TEST yields ErrIgnored and unknown types
// ErrUnsupported.
func (p *Parser) Parse(body []byte) (entitlements.Event, error) {
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
