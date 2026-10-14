package httpx

import (
	"errors"
	"net/http"

	"go.uber.org/zap"

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
	log    *zap.Logger
	cfg    config
}

// NewAppleHandler returns the handler for the App Store Server Notifications V2
// endpoint. Parsing Apple notifications involves no I/O, so any parse error
// other than TEST/unsupported types is permanent: 401 for a failed signature,
// 400 for anything else.
func NewAppleHandler(parser AppleParser, svc Processor, logger *zap.Logger, opts ...Option) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &appleHandler{parser: parser, svc: svc, log: logger.With(zap.String("handler", "apple")), cfg: newConfig(opts)}
}

func (h *appleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, h.cfg.maxBody, h.log)
	if !ok {
		return
	}
	ev, err := h.parser.Parse(body)
	switch {
	case err == nil:
		process(w, r, h.svc, ev, h.log)
	case errors.Is(err, entitlements.ErrIgnored), errors.Is(err, entitlements.ErrUnsupported):
		h.log.Warn("notification not applicable", zap.Error(err))
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, entitlements.ErrInvalidSignature):
		h.log.Error("rejected notification: invalid signature", zap.Error(err), zap.String("remote", r.RemoteAddr))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
	default:
		h.log.Error("rejected notification: malformed", zap.Error(err), zap.String("remote", r.RemoteAddr))
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}
