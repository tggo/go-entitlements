package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/tggo/go-entitlements"
)

// AppleParser verifies and maps an App Store Server Notifications V2 body.
// *apple.Parser implements it.
type AppleParser interface {
	Parse(body []byte) (entitlements.Event, error)
}

type appleHandler struct {
	parser AppleParser
	svc    Processor
	log    *slog.Logger
	cfg    config
}

// NewAppleHandler returns the handler for the App Store Server Notifications V2
// endpoint. Parsing Apple notifications involves no I/O, so any parse error
// other than TEST/unsupported types is permanent: 401 for a failed signature,
// 400 for anything else.
func NewAppleHandler(parser AppleParser, svc Processor, opts ...Option) http.Handler {
	cfg := newConfig(opts)
	return &appleHandler{parser: parser, svc: svc, log: cfg.log.With("handler", "apple"), cfg: cfg}
}

func (h *appleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer recoverPanic(w, h.log)
	body, ok := readBody(w, r, h.cfg.maxBody, h.log)
	if !ok {
		return
	}
	ev, err := h.parser.Parse(body)
	switch {
	case err == nil:
		status, _ := process(r, h.svc, ev, h.log)
		respond(w, status)
	case errors.Is(err, entitlements.ErrIgnored):
		h.log.Info("notification ignored", "reason", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, entitlements.ErrUnsupported):
		h.log.Warn("notification not applicable", "error", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, entitlements.ErrInvalidSignature):
		h.log.Error("rejected notification: invalid signature", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
	default:
		h.log.Error("rejected notification: malformed", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}
