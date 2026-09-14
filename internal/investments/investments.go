// Package investments tracks primary-market orders against an Offering.
//
// There is no payment gateway here, on purpose: real securities offerings
// (Securitize included) settle investor payment by bank wire/ACH, confirmed
// by an operations team, not by a card charge — placing an order and
// fulfilling it are two different moments in reality, and this package
// keeps them that way. PlaceOrder only reserves the order and issues a
// payment reference; ConfirmPayment is the operator action ("wire
// received") that actually triggers the on-chain unit transfer.
package investments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"yeshara.network/marketplace/internal/cometengine"
	"yeshara.network/marketplace/internal/database"
)

type Status string

const (
	StatusPendingPayment Status = "pending_payment"
	StatusCompleted      Status = "completed"
	StatusCancelled      Status = "cancelled"
	StatusFailed         Status = "failed" // mobile-money charge failed (declined, timed out, insufficient funds, ...)
)

// unitDecimals matches YESHARA_PRIMARY_OFFERING's decimals in comet-engine
// (see the standard definition) — every Yeshara offering shares one
// standard today, so this is safe to fix here rather than look up per call.
const unitDecimals = 2

type Investment struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	OfferingID  primitive.ObjectID `bson:"offering_id" json:"offeringId"`
	InvestorID  primitive.ObjectID `bson:"investor_id" json:"investorId"`
	Units       float64            `bson:"units" json:"units"`
	AmountDue   float64            `bson:"amount_due" json:"amountDue"`
	Currency    string             `bson:"currency" json:"currency"`
	Status      Status             `bson:"status" json:"status"`
	TxHash      string             `bson:"tx_hash,omitempty" json:"txHash,omitempty"`
	PaymentRef  string             `bson:"payment_ref" json:"paymentRef"`
	CreatedAt   time.Time          `bson:"created_at" json:"createdAt"`
	ConfirmedAt *time.Time         `bson:"confirmed_at,omitempty" json:"confirmedAt,omitempty"`

	// Mobile-money checkout (Mamlaka PSP) — set when this order is paid via
	// STK push rather than a manual wire. PaymentRef doubles as the
	// externalId Mamlaka echoes back on its callback.
	PaymentProvider   string `bson:"payment_provider,omitempty" json:"paymentProvider,omitempty"` // "mpesa" | "airtel"
	Phone             string `bson:"phone,omitempty" json:"phone,omitempty"`
	SecureID          string `bson:"secure_id,omitempty" json:"secureId,omitempty"`
	ProviderReference string `bson:"provider_reference,omitempty" json:"providerReference,omitempty"` // telco receipt number
	FailureReason     string `bson:"failure_reason,omitempty" json:"failureReason,omitempty"`
}

var (
	ErrNotFound     = errors.New("investments: not found")
	ErrNotPending   = errors.New("investments: not pending payment")
	ErrBelowMinimum = errors.New("investments: below the offering's minimum investment")
)

func collection() *mongo.Collection { return database.GetDB().Collection("investments") }

func PlaceOrder(ctx context.Context, offeringID, investorID primitive.ObjectID, units, pricePerUnit, minUnits float64, currency string) (*Investment, error) {
	if units <= 0 {
		return nil, errors.New("investments: units must be positive")
	}
	if minUnits > 0 && units < minUnits {
		return nil, ErrBelowMinimum
	}
	now := time.Now().UTC()
	inv := &Investment{
		OfferingID: offeringID, InvestorID: investorID, Units: units,
		AmountDue: units * pricePerUnit, Currency: currency,
		Status: StatusPendingPayment, PaymentRef: generatePaymentRef(), CreatedAt: now,
	}
	res, err := collection().InsertOne(ctx, inv)
	if err != nil {
		return nil, err
	}
	inv.ID = res.InsertedID.(primitive.ObjectID)
	return inv, nil
}

// generatePaymentRef must be unguessable, not just unique: it's the
// externalId a forged POST /payments/mobile-money/callback would need to
// name to fake-confirm someone else's order (see mobileMoneyCallback — the
// HMAC signature is the primary defense, but a guessable ref made a nanosecond-
// timestamp-based one brute-forceable within the same second regardless).
// 8 random bytes is 64 bits of entropy, the same order of magnitude as the
// 32-byte session tokens elsewhere in this codebase use crypto/rand for.
func generatePaymentRef() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "YSH-" + hex.EncodeToString(b)
}

// ConfirmPayment is the operator action fulfilling an order: transfers
// `units` of the offering's symbol from the issuer's treasury identity to
// the investor's own custodial wallet — the actual on-chain moment of
// ownership transfer.
func ConfirmPayment(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, symbol, chain, treasuryUserID, investorExternalUserID string) (*Investment, error) {
	inv, err := Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if inv.Status != StatusPendingPayment {
		return nil, ErrNotPending
	}

	investorAddr, err := ce.EnsureWallet(ctx, investorExternalUserID, chain)
	if err != nil {
		return nil, fmt.Errorf("resolve investor wallet: %w", err)
	}
	amountBase := int64(math.Round(inv.Units * math.Pow10(unitDecimals)))
	txHash, err := ce.TransferUnits(ctx, symbol, chain, treasuryUserID, investorAddr, amountBase)
	if err != nil {
		return nil, fmt.Errorf("on-chain transfer failed: %w", err)
	}

	now := time.Now().UTC()
	_, err = collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusPendingPayment},
		bson.M{"$set": bson.M{"status": StatusCompleted, "tx_hash": txHash, "confirmed_at": now}},
	)
	if err != nil {
		// Units already moved on-chain — same "needs manual reconciliation"
		// gap comet-engine itself is honest about for its own mint/burn
		// pairs; surface it the same way rather than hide it.
		return nil, fmt.Errorf("transferred on-chain (tx %s) but failed to update the investment record — needs manual reconciliation: %w", txHash, err)
	}
	inv.Status, inv.TxHash, inv.ConfirmedAt = StatusCompleted, txHash, &now
	return inv, nil
}

// AttachPayment records the mobile-money charge Yeshara initiated for a
// pending order, right after Mamlaka accepts the STK push.
func AttachPayment(ctx context.Context, id primitive.ObjectID, provider, phone, secureID string) error {
	_, err := collection().UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"payment_provider": provider, "phone": phone, "secure_id": secureID}},
	)
	return err
}

// SetProviderReference records the telco's receipt number once the
// callback reports success. Best-effort — never blocks confirmation.
func SetProviderReference(ctx context.Context, id primitive.ObjectID, reference string) error {
	_, err := collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"provider_reference": reference}})
	return err
}

// GetByPaymentRef looks up an order by the reference Yeshara generated for
// it — the same value sent to Mamlaka as externalId, and what its callback
// echoes back so the two can be matched up.
func GetByPaymentRef(ctx context.Context, ref string) (*Investment, error) {
	var inv Investment
	err := collection().FindOne(ctx, bson.M{"payment_ref": ref}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &inv, err
}

// Fail marks a pending order failed — the mobile-money equivalent of
// Cancel, for a charge that was attempted and declined/timed out rather
// than abandoned by the investor.
func Fail(ctx context.Context, id primitive.ObjectID, reason string) error {
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusPendingPayment},
		bson.M{"$set": bson.M{"status": StatusFailed, "failure_reason": reason}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotPending
	}
	return nil
}

func Cancel(ctx context.Context, id primitive.ObjectID) error {
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusPendingPayment},
		bson.M{"$set": bson.M{"status": StatusCancelled}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotPending
	}
	return nil
}

func Get(ctx context.Context, id primitive.ObjectID) (*Investment, error) {
	var inv Investment
	err := collection().FindOne(ctx, bson.M{"_id": id}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &inv, err
}

func ListByInvestor(ctx context.Context, investorID primitive.ObjectID) ([]Investment, error) {
	cur, err := collection().Find(ctx, bson.M{"investor_id": investorID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Investment{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func ListByOffering(ctx context.Context, offeringID primitive.ObjectID, status string) ([]Investment, error) {
	filter := bson.M{"offering_id": offeringID}
	if status != "" {
		filter["status"] = status
	}
	cur, err := collection().Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Investment{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
