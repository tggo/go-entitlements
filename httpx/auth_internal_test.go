package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/api/idtoken"
)

func TestPubSubAuthenticator(t *testing.T) {
	const aud, email = "https://example.com/webhooks/google", "push@proj.iam.gserviceaccount.com"
	validator := func(claims map[string]any, err error) idTokenValidator {
		return func(_ context.Context, token, audience string) (*idtoken.Payload, error) {
			assert.Equal(t, "tok", token)
			assert.Equal(t, aud, audience)
			return &idtoken.Payload{Claims: claims}, err
		}
	}
	tests := []struct {
		name    string
		header  string
		v       idTokenValidator
		wantErr bool
	}{
		{"ok", "Bearer tok", validator(map[string]any{"email": email, "email_verified": true}, nil), false},
		{"no header", "", validator(nil, nil), true},
		{"not bearer", "Basic tok", validator(nil, nil), true},
		{"validation fails", "Bearer tok", validator(nil, errors.New("expired")), true},
		{"wrong email", "Bearer tok", validator(map[string]any{"email": "evil@x.com", "email_verified": true}, nil), true},
		{"unverified email", "Bearer tok", validator(map[string]any{"email": email, "email_verified": false}, nil), true},
		{"no email claim", "Bearer tok", validator(map[string]any{}, nil), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			err := newPubSubAuthenticator(aud, email, tt.v)(r)
			assert.Equal(t, tt.wantErr, err != nil, "%v", err)
		})
	}
}

func TestNewPubSubAuthenticator_RealValidatorRejectsGarbage(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer not-a-jwt")
	assert.Error(t, NewPubSubAuthenticator("aud", "e@x.com")(r))
}
