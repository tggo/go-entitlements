package serverapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// ConsumptionDeadline is how long the developer has to answer a
// CONSUMPTION_REQUEST notification ("Respond within 12 hours of receiving
// the CONSUMPTION_REQUEST notification").
const ConsumptionDeadline = 12 * time.Hour

// autoRenewable is the transaction type of an auto-renewable subscription.
const autoRenewable = "Auto-Renewable Subscription"

// DeliveryStatus says whether the app delivered the purchase and it works.
// https://developer.apple.com/documentation/appstoreserverapi/deliverystatus
type DeliveryStatus string

// Delivery statuses.
const (
	DeliveryDelivered        DeliveryStatus = "DELIVERED"
	DeliveryQualityIssue     DeliveryStatus = "UNDELIVERED_QUALITY_ISSUE"
	DeliveryWrongItem        DeliveryStatus = "UNDELIVERED_WRONG_ITEM"
	DeliveryServerOutage     DeliveryStatus = "UNDELIVERED_SERVER_OUTAGE"
	DeliveryUndeliveredOther DeliveryStatus = "UNDELIVERED_OTHER"
)

func (d DeliveryStatus) valid() bool {
	switch d {
	case DeliveryDelivered, DeliveryQualityIssue, DeliveryWrongItem, DeliveryServerOutage, DeliveryUndeliveredOther:
		return true
	}
	return false
}

// RefundPreference is the developer's wish for the refund decision.
// https://developer.apple.com/documentation/appstoreserverapi/refundpreference
type RefundPreference string

// Refund preferences.
const (
	RefundDecline       RefundPreference = "DECLINE"
	RefundGrantFull     RefundPreference = "GRANT_FULL"
	RefundGrantProrated RefundPreference = "GRANT_PRORATED"
)

// ConsumptionRequestReason is why the customer asked for a refund
// (data.consumptionRequestReason of a CONSUMPTION_REQUEST notification).
// https://developer.apple.com/documentation/appstoreservernotifications/consumptionrequestreason
type ConsumptionRequestReason string

// Refund request reasons.
const (
	ReasonUnintendedPurchase      ConsumptionRequestReason = "UNINTENDED_PURCHASE"
	ReasonFulfillmentIssue        ConsumptionRequestReason = "FULFILLMENT_ISSUE"
	ReasonUnsatisfiedWithPurchase ConsumptionRequestReason = "UNSATISFIED_WITH_PURCHASE"
	ReasonLegal                   ConsumptionRequestReason = "LEGAL"
	ReasonOther                   ConsumptionRequestReason = "OTHER"
)

// MaxConsumptionPercentage is 100% in milliunits.
const MaxConsumptionPercentage = 100000

// Errors of consumption validation.
var (
	// ErrInvalidConsumptionInfo wraps every ConsumptionInfo validation failure.
	ErrInvalidConsumptionInfo = errors.New("serverapi: invalid consumption info")
	// ErrConsentRequired means CustomerConsented is false. Apple: "If not,
	// don't respond to the CONSUMPTION_REQUEST notification." Nothing is sent.
	ErrConsentRequired = fmt.Errorf("%w: the customer has not consented to share consumption data", ErrInvalidConsumptionInfo)
)

// ConsumptionInfo is the body of Send Consumption Information (Apple's
// ConsumptionRequest, the V2 shape). The App Store uses it to decide a refund.
// https://developer.apple.com/documentation/appstoreserverapi/consumptionrequest
//
// You are solely responsible for obtaining the customer's consent before
// sharing their data; the zero value has CustomerConsented false and is never
// sent.
type ConsumptionInfo struct {
	// CustomerConsented must be true: the customer agreed to share this data.
	CustomerConsented bool
	// DeliveryStatus is required.
	DeliveryStatus DeliveryStatus
	// SampleContentProvided: you offered a free sample, trial or description
	// of the content before the purchase. Required by Apple; false is a
	// statement too.
	SampleContentProvided bool
	// ConsumptionPercentage is the consumed share in milliunits (50000 is
	// 50%), for consumables, non-consumables and non-renewing subscriptions.
	// nil omits it. It must be 0 when DeliveryStatus is not DELIVERED, and
	// must not be set for an auto-renewable subscription (Apple computes it
	// from elapsed time). Use Percent to build it.
	ConsumptionPercentage *int
	// RefundPreference is optional; empty omits it. GRANT_PRORATED needs a
	// percentage above 0 and below 100000, except for auto-renewable
	// subscriptions, where it must be omitted.
	RefundPreference RefundPreference
}

// Percent converts a percentage (0 to 100) to the milliunits ConsumptionInfo
// wants.
func Percent(p float64) *int {
	v := int(p*1000 + 0.5)
	return &v
}

// Validate checks the rules Apple documents that do not depend on the product
// type. Apple would reject a violation with HTTP 400 after a round trip; this
// fails early, with ErrInvalidConsumptionInfo.
func (c ConsumptionInfo) Validate() error {
	switch {
	case !c.CustomerConsented:
		return ErrConsentRequired
	case !c.DeliveryStatus.valid():
		return fmt.Errorf("%w: delivery status %q", ErrInvalidConsumptionInfo, c.DeliveryStatus)
	}
	switch c.RefundPreference {
	case "", RefundDecline, RefundGrantFull, RefundGrantProrated:
	default:
		return fmt.Errorf("%w: refund preference %q", ErrInvalidConsumptionInfo, c.RefundPreference)
	}
	if p := c.ConsumptionPercentage; p != nil {
		if *p < 0 || *p > MaxConsumptionPercentage {
			return fmt.Errorf("%w: consumption percentage %d is outside 0 to %d milliunits", ErrInvalidConsumptionInfo, *p, MaxConsumptionPercentage)
		}
		if c.DeliveryStatus != DeliveryDelivered && *p != 0 {
			return fmt.Errorf("%w: consumption percentage must be 0 unless the purchase was delivered", ErrInvalidConsumptionInfo)
		}
	}
	if c.RefundPreference == RefundGrantProrated && c.ConsumptionPercentage != nil &&
		(*c.ConsumptionPercentage <= 0 || *c.ConsumptionPercentage >= MaxConsumptionPercentage) {
		return fmt.Errorf("%w: GRANT_PRORATED needs a consumption percentage above 0 and below %d", ErrInvalidConsumptionInfo, MaxConsumptionPercentage)
	}
	return nil
}

// ValidateFor adds the rules that depend on the transaction type
// (JWSTransactionDecodedPayload.Type): an auto-renewable subscription takes no
// consumption percentage.
func (c ConsumptionInfo) ValidateFor(transactionType string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if transactionType == autoRenewable && c.ConsumptionPercentage != nil {
		return fmt.Errorf("%w: an auto-renewable subscription takes no consumption percentage", ErrInvalidConsumptionInfo)
	}
	if transactionType != "" && transactionType != autoRenewable &&
		c.RefundPreference == RefundGrantProrated && c.ConsumptionPercentage == nil {
		return fmt.Errorf("%w: GRANT_PRORATED needs a consumption percentage for %q", ErrInvalidConsumptionInfo, transactionType)
	}
	return nil
}

type wireConsumption struct {
	CustomerConsented     bool             `json:"customerConsented"`
	ConsumptionPercentage *int             `json:"consumptionPercentage,omitempty"`
	DeliveryStatus        DeliveryStatus   `json:"deliveryStatus"`
	RefundPreference      RefundPreference `json:"refundPreference,omitempty"`
	SampleContentProvided bool             `json:"sampleContentProvided"`
}

// SendConsumptionInfo answers a CONSUMPTION_REQUEST: it tells the App Store
// how much of the purchase the customer used, to inform the refund decision.
// transactionID is the transactionId of the notification's transaction. The
// info is validated first and nothing is sent if CustomerConsented is false.
// Apple answers 202 with an empty body. An undelivered purchase without a
// percentage is sent with an explicit 0, which Apple requires.
//
//	PUT /inApps/v2/transactions/consumption/{transactionId}
//	https://developer.apple.com/documentation/appstoreserverapi/send-consumption-information
//
// The older V1 endpoint (/inApps/v1/..., ConsumptionRequestV1 with
// accountTenure, platform, playTime and other fields) is deprecated and not
// implemented.
func (c *Client) SendConsumptionInfo(ctx context.Context, transactionID string, info ConsumptionInfo) error {
	if err := requireID("transaction ID", transactionID); err != nil {
		return err
	}
	if err := info.Validate(); err != nil {
		return err
	}
	body := wireConsumption{
		CustomerConsented: true, ConsumptionPercentage: info.ConsumptionPercentage, DeliveryStatus: info.DeliveryStatus,
		RefundPreference: info.RefundPreference, SampleContentProvided: info.SampleContentProvided,
	}
	if body.ConsumptionPercentage == nil && info.DeliveryStatus != DeliveryDelivered {
		zero := 0
		body.ConsumptionPercentage = &zero
	}
	return c.do(ctx, http.MethodPut, "/inApps/v2/transactions/consumption/"+url.PathEscape(transactionID), nil, body, nil)
}

// ConsumptionRequest describes one CONSUMPTION_REQUEST notification: a
// customer asked Apple for a refund and Apple asks what you know.
type ConsumptionRequest struct {
	NotificationUUID      string
	OriginalTransactionID string
	TransactionID         string
	// UserID is the appAccountToken, empty if the app does not set one;
	// then look the user up by OriginalTransactionID.
	UserID      string
	ProductID   string
	ProductType string
	Environment entitlements.Environment
	Reason      ConsumptionRequestReason
	// SignedAt is when Apple signed the notification; the answer is due by
	// Deadline (SignedAt + 12 hours).
	SignedAt time.Time
	Deadline time.Time
	// Transaction is the verified transaction of the refund request.
	Transaction apple.JWSTransactionDecodedPayload
}

// ConsumptionResponder is implemented by the app. Return the facts you hold
// about the purchase and whether the customer consented to share them. Return
// ErrConsentRequired (or ConsumptionInfo{}) when there is no consent: nothing
// is then sent. Any other error means "try again": the webhook answers 500 so
// Apple redelivers (see NewConsumptionHandler). The method may run more than
// once for one notification and must be idempotent.
type ConsumptionResponder interface {
	Consumption(ctx context.Context, req ConsumptionRequest) (ConsumptionInfo, error)
}

// ConsumptionResponderFunc adapts a function to ConsumptionResponder.
type ConsumptionResponderFunc func(ctx context.Context, req ConsumptionRequest) (ConsumptionInfo, error)

// Consumption calls f.
func (f ConsumptionResponderFunc) Consumption(ctx context.Context, req ConsumptionRequest) (ConsumptionInfo, error) {
	return f(ctx, req)
}

// ConsumptionSender sends consumption info; *Client implements it.
type ConsumptionSender interface {
	SendConsumptionInfo(ctx context.Context, transactionID string, info ConsumptionInfo) error
}

// ConsumptionOutcome says what RespondToConsumptionRequest did.
type ConsumptionOutcome string

// Consumption outcomes.
const (
	// ConsumptionSent: Apple accepted the answer (HTTP 202).
	ConsumptionSent ConsumptionOutcome = "sent"
	// ConsumptionSkipped: no consent, so nothing was sent.
	ConsumptionSkipped ConsumptionOutcome = "skipped"
)

// ErrNotConsumptionRequest means the notification is some other type. It
// wraps entitlements.ErrIgnored.
var ErrNotConsumptionRequest = fmt.Errorf("serverapi: not a CONSUMPTION_REQUEST notification: %w", entitlements.ErrIgnored)

// consumptionPayload reads the one field apple.NotificationData lacks.
type consumptionPayload struct {
	Data struct {
		ConsumptionRequestReason string `json:"consumptionRequestReason"`
	} `json:"data"`
}

// ParseConsumptionRequest verifies a notification body with v (signature,
// bundle ID, environment, nested transaction) and returns the refund request
// in it. Other notification types return ErrNotConsumptionRequest.
func ParseConsumptionRequest(v *apple.Verifier, body []byte) (ConsumptionRequest, error) {
	n, err := v.VerifyNotification(body)
	if err != nil {
		return ConsumptionRequest{}, err
	}
	if n.Payload.NotificationType != "CONSUMPTION_REQUEST" {
		return ConsumptionRequest{}, fmt.Errorf("%w: %s", ErrNotConsumptionRequest, n.Payload.NotificationType)
	}
	if n.Transaction == nil || n.Transaction.TransactionID == "" {
		return ConsumptionRequest{}, errors.New("serverapi: CONSUMPTION_REQUEST without a transaction")
	}
	rb, err := signedPayloadOf(body)
	if err != nil {
		return ConsumptionRequest{}, err
	}
	var cp consumptionPayload
	if err := v.Verify(rb, &cp); err != nil {
		return ConsumptionRequest{}, err
	}
	signed := time.UnixMilli(n.Payload.SignedDate).UTC()
	tx := n.Transaction
	return ConsumptionRequest{
		NotificationUUID:      n.Payload.NotificationUUID,
		OriginalTransactionID: tx.OriginalTransactionID,
		TransactionID:         tx.TransactionID,
		UserID:                tx.AppAccountToken,
		ProductID:             tx.ProductID,
		ProductType:           tx.Type,
		Environment:           entitlements.Environment(tx.Environment),
		Reason:                ConsumptionRequestReason(cp.Data.ConsumptionRequestReason),
		SignedAt:              signed,
		Deadline:              signed.Add(ConsumptionDeadline),
		Transaction:           *tx,
	}, nil
}

// RespondToConsumptionRequest asks the responder about the request and sends
// the answer. Without consent (ErrConsentRequired from the responder, or
// CustomerConsented false) nothing is sent and the outcome is
// ConsumptionSkipped. The answer is validated for the product type before it
// is sent, so a mistake in the responder shows up here, not as an HTTP 400.
func RespondToConsumptionRequest(ctx context.Context, s ConsumptionSender, r ConsumptionResponder, req ConsumptionRequest) (ConsumptionOutcome, error) {
	info, err := r.Consumption(ctx, req)
	if err != nil {
		if errors.Is(err, ErrConsentRequired) {
			return ConsumptionSkipped, nil
		}
		return "", fmt.Errorf("serverapi: consumption responder: %w", err)
	}
	if !info.CustomerConsented {
		return ConsumptionSkipped, nil
	}
	if err := info.ValidateFor(req.ProductType); err != nil {
		return "", err
	}
	if err := s.SendConsumptionInfo(ctx, req.TransactionID, info); err != nil {
		return "", err
	}
	return ConsumptionSent, nil
}
