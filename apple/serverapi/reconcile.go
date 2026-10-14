package serverapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// ErrUnknownStatus means Apple reported a subscription status this package
// does not know (not 1 to 5). It wraps entitlements.ErrUnsupported.
var ErrUnknownStatus = fmt.Errorf("serverapi: unknown subscription status: %w", entitlements.ErrUnsupported)

// ErrNoSignedDate means the signed transaction carries no signedDate, so a
// snapshot built from it could not be ordered against webhooks. It wraps
// entitlements.ErrUnsupported.
var ErrNoSignedDate = fmt.Errorf("serverapi: signed transaction has no signedDate: %w", entitlements.ErrUnsupported)

// eventIDPrefix marks reconcile event IDs. Notification IDs are UUIDs, so the
// two never collide in the store's per-source ID set.
const eventIDPrefix = "reconcile-"

// StatusSource is what Reconcile needs from a Client.
type StatusSource interface {
	GetAllSubscriptionStatuses(ctx context.Context, anyTransactionID string, statuses ...Status) (*SubscriptionStatuses, error)
}

// Applier is what Reconcile and ReplayNotificationHistory need from an
// entitlements.Service.
type Applier interface {
	Process(ctx context.Context, ev entitlements.Event) (*entitlements.Entitlement, error)
	Get(ctx context.Context, source entitlements.Source, originalTxID string) (*entitlements.Entitlement, error)
}

// Outcome says what happened to one event fed to the Service.
type Outcome string

// Outcomes of feeding an event to the Service.
const (
	// OutcomeApplied: the event changed the stored entitlement.
	OutcomeApplied Outcome = "applied"
	// OutcomeUnchanged: the stored entitlement already matches; nothing was
	// sent (Reconcile only).
	OutcomeUnchanged Outcome = "unchanged"
	// OutcomeDuplicate: this exact event was applied before.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeStale: a newer event had been applied, so this one was dropped.
	OutcomeStale Outcome = "stale"
	// OutcomeIgnored: the Service ignores the event (for example a Sandbox
	// event on a Service built without WithAllowSandbox).
	OutcomeIgnored Outcome = "ignored"
	// OutcomeFailed: Err says why.
	OutcomeFailed Outcome = "failed"
)

// classify turns the error of Service.Process into an Outcome.
func classify(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeApplied
	case errors.Is(err, entitlements.ErrDuplicate):
		return OutcomeDuplicate
	case errors.Is(err, entitlements.ErrStale):
		return OutcomeStale
	case errors.Is(err, entitlements.ErrIgnored), errors.Is(err, entitlements.ErrUnsupported):
		return OutcomeIgnored
	}
	return OutcomeFailed
}

// Reconciled is the result for one subscription chain.
type Reconciled struct {
	Event       entitlements.Event
	Outcome     Outcome
	Entitlement *entitlements.Entitlement
	Err         error
}

// Reconcile repairs a lost or missed webhook: it asks Apple for the current
// status of the customer's subscriptions (Get All Subscription Statuses for
// originalTransactionID, which may be any transaction ID of the customer),
// builds one entitlements.Event per subscription chain returned, and feeds
// them to svc.
//
// The state comes from Apple's status field (see EventsFromStatuses). A chain
// whose stored entitlement already has the same state, expiry and product is
// skipped (OutcomeUnchanged), so running Reconcile repeatedly writes nothing.
//
// Ordering. The event's OccurredAt is the signedDate Apple put in the signed
// transaction, that is Apple's clock at the moment it answered, the same clock
// that stamps webhooks. The Service drops any event older than the stored one
// (ErrStale), so:
//   - a webhook applied after Apple answered but before Reconcile applies its
//     result is newer, and the snapshot is dropped as stale instead of
//     overwriting it;
//   - a webhook delayed past the snapshot is newer than the snapshot and
//     applies on top of it when it arrives;
//   - a webhook older than the snapshot is stale once the snapshot is in, so
//     a late retry from before the repair cannot undo it.
//
// Using the local clock instead would break the first case whenever the
// server's clock runs ahead of Apple's. IDs are deterministic (a hash of the
// chain, its latest transaction, the status, the dates and the signedDate), so
// delivering one snapshot twice is a duplicate. Retrying after a partial
// failure is safe for the same reason.
//
// A returned error joins every per-chain failure; the successful chains are
// still in the result.
func Reconcile(ctx context.Context, src StatusSource, svc Applier, originalTransactionID string) ([]Reconciled, error) {
	statuses, err := src.GetAllSubscriptionStatuses(ctx, originalTransactionID)
	if err != nil {
		return nil, fmt.Errorf("serverapi: reconcile %s: %w", originalTransactionID, err)
	}
	var (
		out  []Reconciled
		errs []error
	)
	for _, g := range statuses.Groups {
		for _, s := range g.Subscriptions {
			ev, berr := EventFromStatus(s)
			if berr != nil {
				out = append(out, Reconciled{Outcome: OutcomeFailed, Err: berr})
				errs = append(errs, fmt.Errorf("chain %s: %w", s.OriginalTransactionID, berr))
				continue
			}
			r := reconcileOne(ctx, svc, ev)
			out = append(out, r)
			if r.Outcome == OutcomeFailed {
				errs = append(errs, fmt.Errorf("chain %s: %w", ev.OriginalTxID, r.Err))
			}
		}
	}
	return out, errors.Join(errs...)
}

func reconcileOne(ctx context.Context, svc Applier, ev entitlements.Event) Reconciled {
	cur, err := svc.Get(ctx, ev.Source, ev.OriginalTxID)
	switch {
	case err == nil:
		if sameState(cur, ev) {
			return Reconciled{Event: ev, Outcome: OutcomeUnchanged, Entitlement: cur}
		}
	case errors.Is(err, entitlements.ErrNotFound):
	default:
		return Reconciled{Event: ev, Outcome: OutcomeFailed, Err: err}
	}
	ent, perr := svc.Process(ctx, ev)
	r := Reconciled{Event: ev, Outcome: classify(perr), Entitlement: ent}
	if r.Outcome == OutcomeFailed {
		r.Err = perr
	}
	return r
}

// sameState reports whether the stored entitlement already says what ev says.
func sameState(cur *entitlements.Entitlement, ev entitlements.Event) bool {
	return cur.State == ev.State &&
		cur.ExpiresAt.Equal(ev.ExpiresAt) &&
		(ev.ProductID == "" || cur.ProductID == ev.ProductID) &&
		cur.Environment == ev.Environment
}

// EventsFromStatuses builds one event per subscription chain in s, skipping
// (and reporting in the joined error) chains Apple's data cannot be turned
// into a safe event for.
func EventsFromStatuses(s *SubscriptionStatuses) ([]entitlements.Event, error) {
	var (
		evs  []entitlements.Event
		errs []error
	)
	for _, g := range s.Groups {
		for _, sub := range g.Subscriptions {
			ev, err := EventFromStatus(sub)
			if err != nil {
				errs = append(errs, fmt.Errorf("chain %s: %w", sub.OriginalTransactionID, err))
				continue
			}
			evs = append(evs, ev)
		}
	}
	return evs, errors.Join(errs...)
}

// EventFromStatus maps one verified subscription status to an event.
//
//   - status 1 → active until expiresDate; 2 → expired; 3 → billing_retry;
//     4 → grace until the later of expiresDate and gracePeriodExpiresDate;
//     5 → revoked for Family Sharing, otherwise refunded. Anything else is
//     ErrUnknownStatus.
//   - an active or grace event without its expiry date is apple.ErrNoExpiry,
//     never open-ended access.
//   - OccurredAt is the later signedDate of the transaction and renewal info
//     (Apple's clock). With neither the snapshot cannot be ordered against
//     webhooks, so it is refused (ErrNoSignedDate); the local clock is never
//     substituted.
//   - A snapshot in a non-terminal state sets Restores: Apple's answer is
//     authoritative, so it may lift a refund the Service still holds (a
//     missed REFUND_REVERSED). The Service still requires it to be strictly
//     newer than that refund.
func EventFromStatus(s SubscriptionStatus) (entitlements.Event, error) {
	tx := s.Transaction
	expires := msToTime(tx.ExpiresDate)

	var state entitlements.State
	switch s.Status {
	case StatusActive:
		state = entitlements.StateActive
		if expires.IsZero() {
			return entitlements.Event{}, fmt.Errorf("%w: status active", apple.ErrNoExpiry)
		}
	case StatusExpired:
		state = entitlements.StateExpired
	case StatusBillingRetry:
		state = entitlements.StateBillingRetry
	case StatusGracePeriod:
		state = entitlements.StateGrace
		var graceEnd time.Time
		if s.Renewal != nil {
			graceEnd = msToTime(s.Renewal.GracePeriodExpiresDate)
		}
		if graceEnd.IsZero() {
			return entitlements.Event{}, fmt.Errorf("%w: status grace period without gracePeriodExpiresDate", apple.ErrNoExpiry)
		}
		if graceEnd.After(expires) {
			expires = graceEnd
		}
	case StatusRevoked:
		state = entitlements.StateRefunded
		if tx.InAppOwnershipType == "FAMILY_SHARED" {
			state = entitlements.StateRevoked
		}
	default:
		return entitlements.Event{}, fmt.Errorf("%w: %d", ErrUnknownStatus, int(s.Status))
	}

	signed := msToTime(tx.SignedDate)
	if s.Renewal != nil {
		if rs := msToTime(s.Renewal.SignedDate); rs.After(signed) {
			signed = rs
		}
	}
	if signed.IsZero() {
		return entitlements.Event{}, ErrNoSignedDate
	}

	origTx := s.OriginalTransactionID
	if origTx == "" {
		origTx = tx.OriginalTransactionID
	}
	terminal := state == entitlements.StateRefunded || state == entitlements.StateRevoked
	raw, _ := json.Marshal(struct {
		SignedTransactionInfo string `json:"signedTransactionInfo"`
		SignedRenewalInfo     string `json:"signedRenewalInfo,omitempty"`
	}{s.SignedTransactionInfo, s.SignedRenewalInfo})

	return entitlements.Event{
		ID:            reconcileID(origTx, tx.TransactionID, s.Status, tx.ExpiresDate, tx.RevocationDate, signed, s.Renewal),
		Source:        entitlements.SourceApple,
		UserID:        tx.AppAccountToken,
		OriginalTxID:  origTx,
		TransactionID: tx.TransactionID,
		ProductID:     tx.ProductID,
		State:         state,
		ExpiresAt:     expires,
		OccurredAt:    signed,
		Restores:      !terminal,
		Environment:   entitlements.Environment(tx.Environment),
		Raw:           raw,
	}, nil
}

// reconcileID is a stable name for one snapshot of one chain.
func reconcileID(origTx, tx string, status Status, expires, revoked int64, signed time.Time, ri *apple.JWSRenewalInfoDecodedPayload) string {
	var grace int64
	if ri != nil {
		grace = ri.GracePeriodExpiresDate
	}
	h := sha256.New()
	for _, f := range []string{
		origTx, tx, strconv.Itoa(int(status)), strconv.FormatInt(expires, 10),
		strconv.FormatInt(revoked, 10), strconv.FormatInt(grace, 10), strconv.FormatInt(signed.UnixMilli(), 10),
	} {
		h.Write([]byte(strings.ReplaceAll(f, "|", "_") + "|"))
	}
	return eventIDPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
