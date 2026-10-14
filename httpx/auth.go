package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"
)

// NewPubSubAuthenticator verifies the OIDC token Pub/Sub attaches to push
// requests (Authorization: Bearer <JWT>) when the subscription has
// authentication enabled. The token must be issued by Google for audience and
// carry a verified email equal to serviceAccountEmail (the push service
// account configured on the subscription). Pass the result to
// WithPushAuthenticator.
func NewPubSubAuthenticator(audience, serviceAccountEmail string) func(*http.Request) error {
	return newPubSubAuthenticator(audience, serviceAccountEmail, idTokenValidator(idtoken.Validate))
}

// idTokenValidator is the part of idtoken.Validate we depend on.
type idTokenValidator func(ctx context.Context, token, audience string) (*idtoken.Payload, error)

func newPubSubAuthenticator(audience, email string, validate idTokenValidator) func(*http.Request) error {
	return func(r *http.Request) error {
		h := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(h, "Bearer ")
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
