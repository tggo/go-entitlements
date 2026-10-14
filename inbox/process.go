package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
)

// AppleParser verifies and decodes an App Store Server Notification body.
// *apple.Parser implements it.
type AppleParser interface {
	Parse(body []byte) (entitlements.Event, error)
}

// GoogleParser decodes a Pub/Sub push body and fetches the subscription state.
// *google.Parser implements it.
type GoogleParser interface {
	ParseResult(ctx context.Context, body []byte) (google.Result, error)
}

// Processor applies an event. *entitlements.Service implements it.
type Processor interface {
	Process(ctx context.Context, ev entitlements.Event) (*entitlements.Entitlement, error)
}

// Processors are the collaborators ProcessStored needs. Apple and Google are
// optional: an item whose source has no parser fails with ErrNoProcessor.
// Acknowledger is optional; without it Google purchases are not acknowledged.
type Processors struct {
	Apple        AppleParser
	Google       GoogleParser
	Service      Processor
	Acknowledger google.Acknowledger
}

// ErrNoProcessor means Processors has no parser for the item's source or no Service.
var ErrNoProcessor = errors.New("inbox: no processor configured for this item")

// ProcessStored processes one saved item and records the outcome. It is the
// one function both an asynchronous worker (a goroutine, a Temporal activity)
// and a replay tool call.
//
// It loads the item and, unless it is already final, verifies and parses it
// (Apple: the signed payload; Google: the push, plus the Play API fetch),
// applies every resulting event through Service.Process, acknowledges a
// pending Google purchase once its events are stored, and marks the item with
// the outcome and attempts+1.
//
// The returned error is non-nil only when trying again can help: a store or
// Play API outage, a missing parser (configure it and retry), or a failure to
// record the outcome. The item is then StatusFailed (retryable) and the
// worker should retry with backoff. Any other outcome, including an invalid
// signature, is final: the status is returned with a nil error.
//
// It is safe to run again: a final item is returned as it is without
// processing, and a retryable one is reprocessed, where events applied by an
// earlier run come back as duplicates. If every event of a re-run is a
// duplicate the status is StatusDuplicate, even though the first run applied
// them.
//
// The aggregate status of an item with several events (a Google subscription
// with add-ons yields one per line item) is, in order of precedence:
// processed, stale, duplicate, unsupported, ignored. A failing event does not
// stop the others.
func ProcessStored(ctx context.Context, in Inbox, id string, p Processors) (Status, error) {
	item, err := in.Get(ctx, id)
	if err != nil {
		return "", fmt.Errorf("inbox: get %s: %w", id, err)
	}
	if item.Status.Final() {
		return item.Status, nil
	}
	out := process(ctx, item, p)
	// Record the outcome even if the caller's context ended meanwhile.
	if err = in.Mark(context.WithoutCancel(ctx), id, out.status, out.text, item.Attempts+1); err != nil {
		return out.status, fmt.Errorf("inbox: mark %s %s: %w", id, out.status, err)
	}
	return out.status, out.retry
}

type outcome struct {
	status Status
	text   string // reason or error, empty on a plain success
	retry  error  // non-nil only when a retry can help
}

func final(st Status, err error) outcome {
	o := outcome{status: st}
	if err != nil {
		o.text = err.Error()
	}
	return o
}

func failed(err error) outcome { return outcome{status: StatusFailed, text: err.Error(), retry: err} }

func process(ctx context.Context, item *Stored, p Processors) outcome {
	if p.Service == nil {
		return failed(fmt.Errorf("%w: no Service", ErrNoProcessor))
	}
	var (
		res google.Result
		evs []entitlements.Event
	)
	switch item.Source {
	case entitlements.SourceApple:
		if p.Apple == nil {
			return failed(fmt.Errorf("%w: no Apple parser", ErrNoProcessor))
		}
		ev, err := p.Apple.Parse(item.Body)
		if err != nil {
			return final(classifyApple(err), err)
		}
		evs = []entitlements.Event{ev}
	case entitlements.SourceGoogle:
		if p.Google == nil {
			return failed(fmt.Errorf("%w: no Google parser", ErrNoProcessor))
		}
		var err error
		if res, err = p.Google.ParseResult(ctx, item.Body); err != nil {
			return classifyGoogle(err)
		}
		evs = res.Events
	default:
		return final(StatusUnsupported, fmt.Errorf("%w: source %q", ErrInvalid, item.Source))
	}
	return apply(ctx, evs, res.Ack, p)
}

func classifyApple(err error) Status {
	switch {
	case errors.Is(err, entitlements.ErrIgnored):
		return StatusIgnored
	case errors.Is(err, entitlements.ErrUnsupported):
		return StatusUnsupported
	}
	return StatusInvalidSignature // bad signature, wrong bundle, malformed: retrying cannot help
}

func classifyGoogle(err error) outcome {
	switch {
	case errors.Is(err, google.ErrFetchConfig), errors.Is(err, google.ErrFetchTransient):
		return failed(err)
	case errors.Is(err, entitlements.ErrIgnored), errors.Is(err, google.ErrPackageMismatch):
		return final(StatusIgnored, err)
	case errors.Is(err, entitlements.ErrUnsupported), errors.Is(err, google.ErrPurchaseNotFound), errors.Is(err, google.ErrPurchaseInvalid):
		return final(StatusUnsupported, err)
	case errors.Is(err, google.ErrMalformed), errors.Is(err, entitlements.ErrInvalidSignature):
		return final(StatusInvalidSignature, err)
	}
	return failed(err) // unexpected: assume transient
}

// rank orders the aggregate statuses: higher wins.
var rank = map[Status]int{StatusIgnored: 1, StatusUnsupported: 2, StatusDuplicate: 3, StatusStale: 4, StatusProcessed: 5}

func apply(ctx context.Context, evs []entitlements.Event, ack *google.Ack, p Processors) outcome {
	var (
		agg       Status
		reason    error // first reason that is not a plain success
		retry     error
		allStored = len(evs) > 0
	)
	note := func(st Status, err error, stored bool) {
		if rank[st] > rank[agg] {
			agg = st
		}
		if err != nil && reason == nil {
			reason = err
		}
		allStored = allStored && stored
	}
	for _, ev := range evs {
		if err := ctx.Err(); err != nil {
			retry = err
			break
		}
		_, err := p.Service.Process(ctx, ev)
		switch {
		case err == nil:
			note(StatusProcessed, nil, true)
		case errors.Is(err, entitlements.ErrDuplicate):
			note(StatusDuplicate, err, true)
		case errors.Is(err, entitlements.ErrStale):
			note(StatusStale, err, true)
		case errors.Is(err, entitlements.ErrIgnored):
			note(StatusIgnored, err, ev.Sandbox()) // a sandbox event is stored nowhere, but there is nothing to wait for
		case errors.Is(err, entitlements.ErrUnsupported), errors.Is(err, entitlements.ErrInvalidEvent):
			note(StatusUnsupported, err, false)
		default:
			if retry == nil {
				retry = err
			}
			allStored = false
		}
	}
	if retry != nil {
		return failed(retry)
	}
	if ack != nil && p.Acknowledger != nil && allStored {
		if err := p.Acknowledger.Acknowledge(ctx, ack.PackageName, ack.SubscriptionID, ack.PurchaseToken); err != nil {
			switch {
			case errors.Is(err, google.ErrAlreadyAcknowledged), errors.Is(err, google.ErrPurchaseNotFound), errors.Is(err, google.ErrPurchaseInvalid):
			default: // transient or a configuration error: the re-run acknowledges again
				return failed(fmt.Errorf("acknowledge: %w", err))
			}
		}
	}
	if agg == "" { // no events at all
		return final(StatusIgnored, fmt.Errorf("%w: no events", entitlements.ErrIgnored))
	}
	if agg == StatusProcessed {
		return outcome{status: agg}
	}
	return final(agg, reason)
}
