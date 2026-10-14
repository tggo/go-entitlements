// Package pubsubauth verifies the OIDC token Pub/Sub attaches to push
// requests. It is a separate package so that httpx does not pull in the Google
// API client.
package pubsubauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"
)

// ErrMissingConfig is returned by New when audience or service account email
// is empty.
var ErrMissingConfig = errors.New("pubsubauth: audience and service account email are both required")

// New returns a push authenticator for httpx.WithPushAuthenticator. It
// verifies the token Pub/Sub sends as Authorization: Bearer <JWT> when the
// subscription has authentication enabled: issued by Google for audience, with
// a verified email equal to serviceAccountEmail (the push service account on
// the subscription). Both arguments are required: idtoken skips the audience
// check when it is empty, so an empty value would silently accept any
// Google-signed token.
func New(audience, serviceAccountEmail string) (func(*http.Request) error, error) {
	if audience == "" || serviceAccountEmail == "" {
		return nil, ErrMissingConfig
	}
	return newAuthenticator(audience, serviceAccountEmail, idtoken.Validate), nil
}

// idTokenValidator is the part of idtoken.Validate we depend on.
type idTokenValidator func(ctx context.Context, token, audience string) (*idtoken.Payload, error)

func newAuthenticator(audience, email string, validate idTokenValidator) func(*http.Request) error {
	return func(r *http.Request) error {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			return errors.New("missing bearer token")
		}
		p, err := validate(r.Context(), token, audience)
		if err != nil {
			return fmt.Errorf("validate id token: %w", err)
		}
		gotEmail, _ := p.Claims["email"].(string)
		verified, _ := p.Claims["email_verified"].(bool)
		if !verified || gotEmail != email {
			return fmt.Errorf("token email %q (verified=%t), want %q", gotEmail, verified, email)
		}
		return nil
	}
}
