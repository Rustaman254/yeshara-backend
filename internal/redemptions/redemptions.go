// Package redemptions is Yeshara's cash-out path: an investor asks to
// redeem units of a completed investment for their real-world value. An
// admin approves it (burning the tokens on-chain via comet-engine, the
// "claim the underlying asset back" moment) and then settles the payout
// through one of three rails — see distributions.PayoutMethod, reused here
// so both flows share one settlement vocabulary.
package redemptions

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"yeshara.network/marketplace/internal/database"
	"yeshara.network/marketplace/internal/distributions"
)

type Status string

const (
	StatusPending        Status = "pending"         // requested, not yet reviewed
	StatusRejected       Status = "rejected"        // admin declined, no burn happened
	StatusApprovedBurned Status = "approved_burned" // comet-engine burn succeeded, payout not yet settled
	StatusPaid           Status = "paid"            // payout settled
	StatusFailed         Status = "failed"          // burn or payout attempt errored
)

type RedemptionRequest struct {
	ID                primitive.ObjectID         `bson:"_id,omitempty" json:"id"`
	OfferingID        primitive.ObjectID         `bson:"offering_id" json:"offeringId"`
	InvestorID        primitive.ObjectID         `bson:"investor_id" json:"investorId"`
	Units             float64                    `bson:"units" json:"units"`
	PayoutAmount      float64                    `bson:"payout_amount" json:"payoutAmount"` // units * offering.PricePerUnit at request time
	Currency          string                     `bson:"currency" json:"currency"`
	PayoutMethod      distributions.PayoutMethod `bson:"payout_method" json:"payoutMethod"`
	PayoutAssetSymbol string                     `bson:"payout_asset_symbol,omitempty" json:"payoutAssetSymbol,omitempty"` // required for onchain_token
	Phone             string                     `bson:"phone,omitempty" json:"phone,omitempty"`                           // required for mpesa_b2c
	Status            Status                     `bson:"status" json:"status"`
	BurnTxHash        string                     `bson:"burn_tx_hash,omitempty" json:"burnTxHash,omitempty"`
	PaymentRef        string                     `bson:"payment_ref,omitempty" json:"paymentRef,omitempty"`
	FailureReason     string                     `bson:"failure_reason,omitempty" json:"failureReason,omitempty"`
	RejectionReason   string                     `bson:"rejection_reason,omitempty" json:"rejectionReason,omitempty"`
	RequestedAt       time.Time                  `bson:"requested_at" json:"requestedAt"`
	ApprovedAt        *time.Time                 `bson:"approved_at,omitempty" json:"approvedAt,omitempty"`
	PaidAt            *time.Time                 `bson:"paid_at,omitempty" json:"paidAt,omitempty"`
}

var (
	ErrNotFound       = errors.New("redemptions: not found")
	ErrNotPending     = errors.New("redemptions: not pending review")
	ErrNotApprovedYet = errors.New("redemptions: not yet approved/burned")
	ErrExceedsHolding = errors.New("redemptions: units exceed current holding")
)

func collection() *mongo.Collection { return database.GetDB().Collection("redemption_requests") }

func EnsureIndexes(ctx context.Context) error {
	_, err := collection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "investor_id", Value: 1}, {Key: "requested_at", Value: -1}},
	})
	return err
}

func Request(ctx context.Context, offeringID, investorID primitive.ObjectID, units, pricePerUnit float64, currency string, method distributions.PayoutMethod, phone, payoutAssetSymbol string) (*RedemptionRequest, error) {
	if units <= 0 {
		return nil, errors.New("redemptions: units must be positive")
	}
	now := time.Now().UTC()
	r := &RedemptionRequest{
		OfferingID: offeringID, InvestorID: investorID, Units: units,
		PayoutAmount: units * pricePerUnit, Currency: currency,
		PayoutMethod: method, Phone: phone, PayoutAssetSymbol: payoutAssetSymbol,
		Status: StatusPending, RequestedAt: now,
	}
	res, err := collection().InsertOne(ctx, r)
	if err != nil {
		return nil, err
	}
	r.ID = res.InsertedID.(primitive.ObjectID)
	return r, nil
}

// MarkBurned transitions a pending request to approved_burned once
// comet-engine's redeem call has actually succeeded — guarded so a request
// can't be double-burned by a racing admin action.
func MarkBurned(ctx context.Context, id primitive.ObjectID, txHash string) error {
	now := time.Now().UTC()
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusPending},
		bson.M{"$set": bson.M{"status": StatusApprovedBurned, "burn_tx_hash": txHash, "approved_at": now}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotPending
	}
	return nil
}

func Reject(ctx context.Context, id primitive.ObjectID, reason string) error {
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusPending},
		bson.M{"$set": bson.M{"status": StatusRejected, "rejection_reason": reason}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotPending
	}
	return nil
}

// MarkPaid settles a burned request's payout — the manual rail's admin
// confirmation, or recorded automatically right after a successful
// mpesa/onchain payout call.
func MarkPaid(ctx context.Context, id primitive.ObjectID, paymentRef string) error {
	now := time.Now().UTC()
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusApprovedBurned},
		bson.M{"$set": bson.M{"status": StatusPaid, "payment_ref": paymentRef, "paid_at": now}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotApprovedYet
	}
	return nil
}

// MarkFailed records a burned-but-unpaid request's payout attempt failing
// (e.g. mpesa_b2c came back short on treasury balance) — status stays
// visible as "failed" rather than silently stuck, and an admin can retry
// the payout (mark-paid manually, or via a different rail).
func MarkFailed(ctx context.Context, id primitive.ObjectID, reason string) error {
	_, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusApprovedBurned},
		bson.M{"$set": bson.M{"status": StatusFailed, "failure_reason": reason}},
	)
	return err
}

func Get(ctx context.Context, id primitive.ObjectID) (*RedemptionRequest, error) {
	var r RedemptionRequest
	err := collection().FindOne(ctx, bson.M{"_id": id}).Decode(&r)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &r, err
}

func ListByInvestor(ctx context.Context, investorID primitive.ObjectID) ([]RedemptionRequest, error) {
	cur, err := collection().Find(ctx, bson.M{"investor_id": investorID}, options.Find().SetSort(bson.D{{Key: "requested_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []RedemptionRequest{}
	err = cur.All(ctx, &out)
	return out, err
}

// List returns redemption requests, optionally filtered by status ("" =
// all), newest first — the admin queue.
func List(ctx context.Context, status string) ([]RedemptionRequest, error) {
	filter := bson.M{}
	if status != "" {
		filter["status"] = status
	}
	cur, err := collection().Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "requested_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []RedemptionRequest{}
	err = cur.All(ctx, &out)
	return out, err
}
