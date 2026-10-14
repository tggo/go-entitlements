package serverapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tggo/go-entitlements"
)

// Sentinels for errors.Is. An *APIError matches the one for its HTTP class
// and, when Apple's errorCode is known, a more specific one. Every 404
// matches entitlements.ErrNotFound.
var (
	// ErrBadRequest is any HTTP 400.
	ErrBadRequest = errors.New("serverapi: bad request")
	// ErrUnauthorized is HTTP 401: the JWT is invalid or expired, or the key
	// does not belong to the app (check issuer ID, key ID, bundle ID and the
	// system clock).
	ErrUnauthorized = errors.New("serverapi: unauthorized")
	// ErrNotFound is any HTTP 404; it wraps entitlements.ErrNotFound.
	ErrNotFound = fmt.Errorf("serverapi: not found: %w", entitlements.ErrNotFound)
	// ErrRateLimited is HTTP 429 (errorCode 4290000). Apple enforces rate
	// limits per hour, so the client does not wait out a long Retry-After;
	// APIError.RetryAfter says when to try again.
	ErrRateLimited = errors.New("serverapi: rate limited")
	// ErrServer is HTTP 5xx.
	ErrServer = errors.New("serverapi: App Store server error")

	// ErrTransactionNotFound is errorCode 4040010, 4040005 or 4040006: the
	// transaction ID is unknown for this app in this environment. A Sandbox
	// transaction looked up in Production (and the reverse) gives this error.
	ErrTransactionNotFound = fmt.Errorf("%w: transaction id", ErrNotFound)
	// ErrAccountNotFound is errorCode 4040001 or 4040002.
	ErrAccountNotFound = fmt.Errorf("%w: account", ErrNotFound)
	// ErrAppNotFound is errorCode 4040003 or 4040004.
	ErrAppNotFound = fmt.Errorf("%w: app", ErrNotFound)
	// ErrNotificationURLNotFound is errorCode 4040007: no App Store Server
	// Notifications URL is configured for this environment.
	ErrNotificationURLNotFound = fmt.Errorf("%w: server notification URL", ErrNotFound)
	// ErrTestNotificationNotReady is errorCode 4040008: the test notification
	// token is expired or its status is not available yet. Poll again.
	ErrTestNotificationNotReady = fmt.Errorf("%w: test notification status", ErrNotFound)
	// ErrInvalidTransactionID is errorCode 4000006.
	ErrInvalidTransactionID = fmt.Errorf("%w: invalid transaction id", ErrBadRequest)
	// ErrPaginationTokenExpired is errorCode 4000017: start the listing again.
	ErrPaginationTokenExpired = fmt.Errorf("%w: pagination token expired", ErrBadRequest)
)

// Apple errorCode values this package treats specially.
// https://developer.apple.com/documentation/appstoreserverapi/error-codes
const (
	codeAccountNotFound         = 4040001
	codeAccountNotFoundRetry    = 4040002
	codeAppNotFound             = 4040003
	codeAppNotFoundRetry        = 4040004
	codeOrigTxNotFound          = 4040005
	codeOrigTxNotFoundRetry     = 4040006
	codeNotificationURLNotFound = 4040007
	codeTestNotificationMissing = 4040008
	codeTxNotFound              = 4040010
	codeInvalidTransactionID    = 4000006
	codePaginationTokenExpired  = 4000017
	codeGeneralInternalRetry    = 5000001
)

// APIError is an error response from the App Store Server API.
type APIError struct {
	// Method and Path identify the request (the path never carries the JWT).
	Method string
	Path   string
	// StatusCode is the HTTP status.
	StatusCode int
	// Code and Message are Apple's errorCode and errorMessage; Code is 0 when
	// the body was not Apple's error JSON.
	Code    int64
	Message string
	// RetryAfter is the wait Apple asked for with Retry-After (0 if absent).
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("serverapi: %s %s: HTTP %d", e.Method, e.Path, e.StatusCode)
	if e.Code != 0 {
		msg += fmt.Sprintf(" errorCode %d", e.Code)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	return msg
}

// Unwrap returns the sentinels this error matches under errors.Is.
func (e *APIError) Unwrap() []error {
	var out []error
	switch {
	case e.StatusCode == http.StatusBadRequest:
		out = append(out, ErrBadRequest)
	case e.StatusCode == http.StatusUnauthorized:
		out = append(out, ErrUnauthorized)
	case e.StatusCode == http.StatusNotFound:
		out = append(out, ErrNotFound)
	case e.StatusCode == http.StatusTooManyRequests:
		out = append(out, ErrRateLimited)
	case e.StatusCode >= 500:
		out = append(out, ErrServer)
	}
	switch e.Code {
	case codeTxNotFound, codeOrigTxNotFound, codeOrigTxNotFoundRetry:
		out = append(out, ErrTransactionNotFound)
	case codeAccountNotFound, codeAccountNotFoundRetry:
		out = append(out, ErrAccountNotFound)
	case codeAppNotFound, codeAppNotFoundRetry:
		out = append(out, ErrAppNotFound)
	case codeNotificationURLNotFound:
		out = append(out, ErrNotificationURLNotFound)
	case codeTestNotificationMissing:
		out = append(out, ErrTestNotificationNotReady)
	case codeInvalidTransactionID:
		out = append(out, ErrInvalidTransactionID)
	case codePaginationTokenExpired:
		out = append(out, ErrPaginationTokenExpired)
	}
	return out
}

// Retryable reports whether repeating the request can succeed: HTTP 429 and
// 5xx, and the 404/500 codes Apple labels "please try again".
func (e *APIError) Retryable() bool {
	if e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500 {
		return true
	}
	switch e.Code {
	case codeAccountNotFoundRetry, codeAppNotFoundRetry, codeOrigTxNotFoundRetry, codeGeneralInternalRetry:
		return true
	}
	return false
}
