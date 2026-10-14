package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// NotificationParser turns a notification body into an event. *apple.Parser
// implements it.
type NotificationParser interface {
	Parse(body []byte) (entitlements.Event, error)
}

// ReplayOption narrows ReplayNotificationHistory.
type ReplayOption func(*NotificationHistoryRequest)

// ReplayOnlyFailures replays only notifications that did not reach your
// server (including ones the App Store is still retrying).
func ReplayOnlyFailures() ReplayOption {
	return func(r *NotificationHistoryRequest) { r.OnlyFailures = true }
}

// ReplayTransaction replays only the notifications of one customer
// (any transaction ID of theirs).
func ReplayTransaction(id string) ReplayOption {
	return func(r *NotificationHistoryRequest) { r.TransactionID = id }
}

// ReplayType replays only one notification type (and subtype, if not empty).
// Apple does not allow it together with ReplayTransaction.
func ReplayType(notificationType, subtype string) ReplayOption {
	return func(r *NotificationHistoryRequest) {
		r.NotificationType, r.NotificationSubtype = notificationType, subtype
	}
}

// ReplayReport counts what ReplayNotificationHistory did.
type ReplayReport struct {
	// Total is the number of history records read.
	Total int
	// Counts by outcome. Notifications that carry no subscription state
	// (TEST, price changes, ...) count as Ignored.
	Applied, Duplicate, Stale, Ignored int
	// Failed counts records that could not be verified, parsed or stored;
	// Errors holds one entry per failure.
	Failed int
	Errors []error
}

// ReplayNotificationHistory re-feeds the notifications the App Store tried to
// send between start and end (at most 180 days back in production, 30 in the
// sandbox) to svc, so webhooks your server lost or rejected are applied.
//
// Each record's signedPayload goes through parser exactly as a webhook body
// would, so the parser's bundle and environment pins apply. Event IDs are
// notificationUUIDs, the same as the live webhook's: a notification that did
// arrive is a duplicate, and one older than the stored state is stale, so a
// replay can neither double-apply nor roll anything back. Records are applied
// oldest first.
//
// Apple's signing certificates are short-lived. Notifications a few months
// old may fail verification at today's date; build the parser's Verifier with
// apple.WithTimeFn if you need to replay them (see its documentation). Such a
// record is counted as Failed, not skipped silently.
//
// The returned error is non-nil if the history could not be read or any
// record failed; the report is always filled in.
func ReplayNotificationHistory(ctx context.Context, c *Client, parser NotificationParser, svc Applier, start, end time.Time, opts ...ReplayOption) (ReplayReport, error) {
	req := NotificationHistoryRequest{Start: start, End: end}
	for _, o := range opts {
		if o != nil {
			o(&req)
		}
	}
	var (
		rep    ReplayReport
		parsed []entitlements.Event
	)
	for rec, err := range c.NotificationHistory(ctx, req) {
		if err != nil {
			return rep, fmt.Errorf("serverapi: read notification history: %w", err)
		}
		rep.Total++
		body, merr := json.Marshal(apple.ResponseBodyV2{SignedPayload: rec.SignedPayload})
		if merr != nil {
			rep.fail(merr)
			continue
		}
		ev, perr := parser.Parse(body)
		switch {
		case perr == nil:
			parsed = append(parsed, ev)
		case errors.Is(perr, entitlements.ErrIgnored), errors.Is(perr, entitlements.ErrUnsupported):
			rep.Ignored++
		default:
			rep.fail(perr)
		}
	}
	slices.SortStableFunc(parsed, func(a, b entitlements.Event) int { return a.OccurredAt.Compare(b.OccurredAt) })
	for _, ev := range parsed {
		_, err := svc.Process(ctx, ev)
		switch classify(err) {
		case OutcomeApplied:
			rep.Applied++
		case OutcomeDuplicate:
			rep.Duplicate++
		case OutcomeStale:
			rep.Stale++
		case OutcomeIgnored:
			rep.Ignored++
		default:
			rep.fail(fmt.Errorf("event %s: %w", ev.ID, err))
		}
	}
	if rep.Failed > 0 {
		return rep, fmt.Errorf("serverapi: replay: %d of %d records failed: %w", rep.Failed, rep.Total, errors.Join(rep.Errors...))
	}
	return rep, nil
}

func (r *ReplayReport) fail(err error) {
	r.Failed++
	r.Errors = append(r.Errors, err)
}
