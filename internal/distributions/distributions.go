// Package distributions is Yeshara's yield-payout mechanism: an admin
// declares a pool of income (rental, sale-proceeds split, whatever a given
// period earned) for an offering, this package splits it pro-rata across
// every investor currently holding completed units of that offering (per
// Yeshara's own investments collection — the authoritative cap table, not
// on-chain holder enumeration), and records one DistributionEntry per
// investor share. How each entry actually gets paid is a separate concern
// (see internal/api/distribution_handlers.go) — this package only computes
// and records the split.
package distributions

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"yeshara.network/marketplace/internal/database"
	"yeshara.network/marketplace/internal/investments"
)

// PayoutMethod is how a distribution's (or redemption's) money actually
// moves. All three are recorded identically; only settlement differs.
type PayoutMethod string

const (
	PayoutManual   PayoutMethod = "manual"        // admin pays off-platform, marks paid with a reference
	PayoutMpesaB2C PayoutMethod = "mpesa_b2c"     // comet-engine B2C transfer — requires a funded comet-engine treasury
	PayoutOnchain  PayoutMethod = "onchain_token" // comet-engine /distribute — mints a registered payout asset to the wallet
)

type DistributionStatus string

const (
	DistributionPending         DistributionStatus = "pending"
	DistributionProcessing      DistributionStatus = "processing"
	DistributionCompleted       DistributionStatus = "completed"
	DistributionPartialFailure  DistributionStatus = "partial_failure"
	DistributionFundingRequired DistributionStatus = "funding_required" // mpesa_b2c: comet-engine treasury balance was short
)

type EntryStatus string

const (
	EntryPending    EntryStatus = "pending"
	EntryProcessing EntryStatus = "processing"
	EntryPaid       EntryStatus = "paid"
	EntryFailed     EntryStatus = "failed"
)

type Distribution struct {
	ID                primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	OfferingID        primitive.ObjectID `bson:"offering_id" json:"offeringId"`
	PeriodLabel       string             `bson:"period_label" json:"periodLabel"`
	TotalAmount       float64            `bson:"total_amount" json:"totalAmount"`
	Currency          string             `bson:"currency" json:"currency"`
	PayoutMethod      PayoutMethod       `bson:"payout_method" json:"payoutMethod"`
	PayoutAssetSymbol string             `bson:"payout_asset_symbol,omitempty" json:"payoutAssetSymbol,omitempty"`
	Status            DistributionStatus `bson:"status" json:"status"`
	CreatedBy         string             `bson:"created_by" json:"createdBy"`
	CreatedAt         time.Time          `bson:"created_at" json:"createdAt"`
}

type DistributionEntry struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	DistributionID primitive.ObjectID `bson:"distribution_id" json:"distributionId"`
	OfferingID     primitive.ObjectID `bson:"offering_id" json:"offeringId"`
	InvestorID     primitive.ObjectID `bson:"investor_id" json:"investorId"`
	Units          float64            `bson:"units" json:"units"` // units held at computation time — the pro-rata basis
	Amount         float64            `bson:"amount" json:"amount"`
	Currency       string             `bson:"currency" json:"currency"`
	PayoutMethod   PayoutMethod       `bson:"payout_method" json:"payoutMethod"`
	Status         EntryStatus        `bson:"status" json:"status"`
	PaymentRef     string             `bson:"payment_ref,omitempty" json:"paymentRef,omitempty"` // manual: admin-entered; mpesa: PSP transactionId; onchain: txHash
	FailureReason  string             `bson:"failure_reason,omitempty" json:"failureReason,omitempty"`
	CreatedAt      time.Time          `bson:"created_at" json:"createdAt"`
	PaidAt         *time.Time         `bson:"paid_at,omitempty" json:"paidAt,omitempty"`
}

var (
	ErrNotFound   = errors.New("distributions: not found")
	ErrNoHolders  = errors.New("distributions: offering has no completed investors to distribute to")
	ErrNotPending = errors.New("distributions: entry not in a payable state")
)

func distCollection() *mongo.Collection  { return database.GetDB().Collection("distributions") }
func entryCollection() *mongo.Collection { return database.GetDB().Collection("distribution_entries") }

func EnsureIndexes(ctx context.Context) error {
	_, err := entryCollection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "investor_id", Value: 1}, {Key: "created_at", Value: -1}},
	})
	return err
}

// Compute splits totalAmount pro-rata across every investor with a
// completed investment in offeringID (by units held), creates the
// Distribution header and one DistributionEntry per investor, and returns
// both. Entries start Pending — actually moving money is the caller's job.
func Compute(ctx context.Context, offeringID primitive.ObjectID, periodLabel string, totalAmount float64, currency string, method PayoutMethod, payoutAssetSymbol, createdBy string) (*Distribution, []DistributionEntry, error) {
	orders, err := investments.ListByOffering(ctx, offeringID, string(investments.StatusCompleted))
	if err != nil {
		return nil, nil, err
	}

	unitsByInvestor := map[primitive.ObjectID]float64{}
	var totalUnits float64
	for _, o := range orders {
		unitsByInvestor[o.InvestorID] += o.Units
		totalUnits += o.Units
	}
	if totalUnits <= 0 {
		return nil, nil, ErrNoHolders
	}

	now := time.Now().UTC()
	dist := &Distribution{
		OfferingID: offeringID, PeriodLabel: periodLabel, TotalAmount: totalAmount,
		Currency: currency, PayoutMethod: method, PayoutAssetSymbol: payoutAssetSymbol,
		Status: DistributionPending, CreatedBy: createdBy, CreatedAt: now,
	}
	res, err := distCollection().InsertOne(ctx, dist)
	if err != nil {
		return nil, nil, err
	}
	dist.ID = res.InsertedID.(primitive.ObjectID)

	entries := make([]DistributionEntry, 0, len(unitsByInvestor))
	docs := make([]interface{}, 0, len(unitsByInvestor))
	for investorID, units := range unitsByInvestor {
		amount := totalAmount * (units / totalUnits)
		e := DistributionEntry{
			ID: primitive.NewObjectID(), DistributionID: dist.ID, OfferingID: offeringID,
			InvestorID: investorID, Units: units, Amount: amount, Currency: currency,
			PayoutMethod: method, Status: EntryPending, CreatedAt: now,
		}
		entries = append(entries, e)
		docs = append(docs, e)
	}
	if _, err := entryCollection().InsertMany(ctx, docs); err != nil {
		return nil, nil, err
	}
	return dist, entries, nil
}

func SetDistributionStatus(ctx context.Context, id primitive.ObjectID, status DistributionStatus) error {
	_, err := distCollection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": status}})
	return err
}

// SetEntryResult records the outcome of actually trying to pay one entry —
// used right after an onchain/mpesa call returns, or by a manual mark-paid.
func SetEntryResult(ctx context.Context, id primitive.ObjectID, status EntryStatus, paymentRef, failureReason string) error {
	set := bson.M{"status": status}
	if paymentRef != "" {
		set["payment_ref"] = paymentRef
	}
	if failureReason != "" {
		set["failure_reason"] = failureReason
	}
	if status == EntryPaid {
		now := time.Now().UTC()
		set["paid_at"] = now
	}
	res, err := entryCollection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkEntryPaid is the manual-rail settlement action: an admin confirms
// they sent the money off-platform and records the reference.
func MarkEntryPaid(ctx context.Context, id primitive.ObjectID, paymentRef string) error {
	res, err := entryCollection().UpdateOne(ctx,
		bson.M{"_id": id, "status": bson.M{"$in": []EntryStatus{EntryPending, EntryProcessing}}},
		bson.M{"$set": bson.M{"status": EntryPaid, "payment_ref": paymentRef, "paid_at": time.Now().UTC()}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotPending
	}
	return nil
}

// RecomputeStatus derives a Distribution's status from its entries' current
// states — call after any entry transition (a payout attempt, a manual
// mark-paid) so the header reflects reality instead of being set once and
// going stale.
func RecomputeStatus(ctx context.Context, distributionID primitive.ObjectID) error {
	entries, err := ListEntriesByDistribution(ctx, distributionID)
	if err != nil || len(entries) == 0 {
		return err
	}
	var paid, failed, pending int
	for _, e := range entries {
		switch e.Status {
		case EntryPaid:
			paid++
		case EntryFailed:
			failed++
		default:
			pending++
		}
	}
	status := DistributionProcessing
	switch {
	case paid == len(entries):
		status = DistributionCompleted
	case pending == len(entries):
		status = DistributionPending
	case failed > 0:
		status = DistributionPartialFailure
	}
	return SetDistributionStatus(ctx, distributionID, status)
}

func Get(ctx context.Context, id primitive.ObjectID) (*Distribution, error) {
	var d Distribution
	err := distCollection().FindOne(ctx, bson.M{"_id": id}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &d, err
}

func GetEntry(ctx context.Context, id primitive.ObjectID) (*DistributionEntry, error) {
	var e DistributionEntry
	err := entryCollection().FindOne(ctx, bson.M{"_id": id}).Decode(&e)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &e, err
}

func ListByOffering(ctx context.Context, offeringID primitive.ObjectID) ([]Distribution, error) {
	cur, err := distCollection().Find(ctx, bson.M{"offering_id": offeringID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Distribution{}
	err = cur.All(ctx, &out)
	return out, err
}

func ListEntriesByDistribution(ctx context.Context, distributionID primitive.ObjectID) ([]DistributionEntry, error) {
	cur, err := entryCollection().Find(ctx, bson.M{"distribution_id": distributionID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []DistributionEntry{}
	err = cur.All(ctx, &out)
	return out, err
}

// ListByInvestor returns every distribution entry an investor has ever been
// owed, newest first — the feed behind GET /me/yield.
func ListByInvestor(ctx context.Context, investorID primitive.ObjectID) ([]DistributionEntry, error) {
	cur, err := entryCollection().Find(ctx, bson.M{"investor_id": investorID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []DistributionEntry{}
	err = cur.All(ctx, &out)
	return out, err
}
