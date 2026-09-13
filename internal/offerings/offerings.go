// Package offerings is Yeshara's primary-market listings — the securitize.io
// "primary market" concept. Each Offering is a thin, cached projection of a
// comet-engine receipt: comet-engine remains the source of truth for
// tokenization state (symbol, status, on-chain quantity), while this local
// record exists for fast listing pages and Yeshara-specific display fields.
package offerings

import (
	"context"
	"errors"
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
	StatusPendingApproval Status = "pending_approval" // deposited to comet-engine, awaiting verify
	StatusLive            Status = "live"             // verified — units minted, open for investment
	StatusRejected        Status = "rejected"
	StatusClosed          Status = "closed" // manually closed or fully sold
)

type Offering struct {
	ID                 primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	OfferingID         string             `bson:"offering_id" json:"offeringId"` // slug — also the receipt's identifier
	Title              string             `bson:"title" json:"title"`
	AssetType          string             `bson:"asset_type" json:"assetType"`
	Location           string             `bson:"location" json:"location"`
	Description        string             `bson:"description" json:"description"`
	ImageURL           string             `bson:"image_url,omitempty" json:"imageUrl,omitempty"`
	TotalUnits         float64            `bson:"total_units" json:"totalUnits"`
	PricePerUnit       float64            `bson:"price_per_unit" json:"pricePerUnit"`
	Currency           string             `bson:"currency" json:"currency"`
	MinInvestmentUnits float64            `bson:"min_investment_units,omitempty" json:"minInvestmentUnits,omitempty"`
	ExpectedYieldPct   float64            `bson:"expected_yield_pct,omitempty" json:"expectedYieldPct,omitempty"`
	RiskLevel          string             `bson:"risk_level,omitempty" json:"riskLevel,omitempty"` // "low" | "medium" | "high"
	Chain              string             `bson:"chain" json:"chain"`
	Symbol             string             `bson:"symbol,omitempty" json:"symbol,omitempty"`
	ReceiptID          string             `bson:"receipt_id" json:"receiptId"`
	IssuerUserID       string             `bson:"issuer_user_id" json:"issuerUserId"`
	Status             Status             `bson:"status" json:"status"`
	CreatedAt          time.Time          `bson:"created_at" json:"createdAt"`
	UpdatedAt          time.Time          `bson:"updated_at" json:"updatedAt"`

	// OwnerUserID/ReservedUnits/ReserveTxHash record the on-approval transfer
	// that sets aside ReservedOwnerPct of TotalUnits for the real-world
	// asset's original owner — see Approve(). Set once, at approval; empty
	// before then.
	OwnerUserID   string  `bson:"owner_user_id,omitempty" json:"ownerUserId,omitempty"`
	ReservedUnits float64 `bson:"reserved_units,omitempty" json:"reservedUnits,omitempty"`
	ReserveTxHash string  `bson:"reserve_tx_hash,omitempty" json:"reserveTxHash,omitempty"`

	// OwnerAccountID links this offering to a real owners.Owner login — set
	// later by an admin, once they've verified off-platform which real
	// person owns this asset (same trust model as KYC approval). Distinct
	// from OwnerUserID: that's just the comet-engine wallet identity the
	// reserve sits in, this is who's actually allowed to view it.
	OwnerAccountID primitive.ObjectID `bson:"owner_account_id,omitempty" json:"ownerAccountId,omitempty"`

	Details Details `bson:"details,omitempty" json:"details,omitempty"`
}

// Details is the fund/asset-fact-sheet content shown on an offering's
// detail page (Securitize-style: eligibility tags, subscription/redemption
// terms, historical performance, manager background, documents). Entirely
// admin-entered and independent of the on-chain approval workflow — none of
// it is sent to comet-engine, which only cares about identifier/quantity.
type Details struct {
	// Tags are short eligibility/structure badges shown next to the title —
	// e.g. "Qualified Purchasers", "Evergreen", "Senior Credit". Free-form
	// since offering types vary (real estate today, other asset classes
	// later) rather than a fixed set of booleans.
	Tags []string `bson:"tags,omitempty" json:"tags,omitempty"`

	TrailingOneYearReturnPct  float64 `bson:"trailing_one_year_return_pct,omitempty" json:"trailingOneYearReturnPct,omitempty"`
	ExpenseRatioPct           float64 `bson:"expense_ratio_pct,omitempty" json:"expenseRatioPct,omitempty"`
	UnderlyingFundExpensesPct float64 `bson:"underlying_fund_expenses_pct,omitempty" json:"underlyingFundExpensesPct,omitempty"`

	SubscriptionFrequency string `bson:"subscription_frequency,omitempty" json:"subscriptionFrequency,omitempty"`
	SubscriptionDeadline  string `bson:"subscription_deadline,omitempty" json:"subscriptionDeadline,omitempty"`
	Domicile              string `bson:"domicile,omitempty" json:"domicile,omitempty"`

	LiquidityPool       string `bson:"liquidity_pool,omitempty" json:"liquidityPool,omitempty"`
	RedemptionWindow    string `bson:"redemption_window,omitempty" json:"redemptionWindow,omitempty"`
	TokenTransferLockup string `bson:"token_transfer_lockup,omitempty" json:"tokenTransferLockup,omitempty"`

	// VolatilityLevel/LiquidityLevel are "low"|"medium"|"high", same
	// vocabulary as the existing RiskLevel field — shown as the fund-profile
	// risk/volatility/liquidity scale.
	VolatilityLevel string `bson:"volatility_level,omitempty" json:"volatilityLevel,omitempty"`
	LiquidityLevel  string `bson:"liquidity_level,omitempty" json:"liquidityLevel,omitempty"`

	Highlights         []Highlight         `bson:"highlights,omitempty" json:"highlights,omitempty"`
	PerformanceHistory []PerformancePeriod `bson:"performance_history,omitempty" json:"performanceHistory,omitempty"`
	Documents          []Document          `bson:"documents,omitempty" json:"documents,omitempty"`

	ManagerName  string `bson:"manager_name,omitempty" json:"managerName,omitempty"`
	ManagerBio   string `bson:"manager_bio,omitempty" json:"managerBio,omitempty"`
	ManagerStats []Stat `bson:"manager_stats,omitempty" json:"managerStats,omitempty"`

	Disclosures string `bson:"disclosures,omitempty" json:"disclosures,omitempty"`
}

type Highlight struct {
	Title string `bson:"title" json:"title"`
	Body  string `bson:"body" json:"body"`
}

type PerformancePeriod struct {
	Label      string  `bson:"label" json:"label"` // e.g. "Aug 2025", "Year-to-Date"
	StartPrice float64 `bson:"start_price" json:"startPrice"`
	EndPrice   float64 `bson:"end_price" json:"endPrice"`
	ReturnPct  float64 `bson:"return_pct" json:"returnPct"`
}

type Document struct {
	Name string `bson:"name" json:"name"`
	URL  string `bson:"url" json:"url"`
}

type Stat struct {
	Label string `bson:"label" json:"label"`
	Value string `bson:"value" json:"value"`
}

var (
	ErrNotFound      = errors.New("offerings: not found")
	ErrSlugTaken     = errors.New("offerings: offeringId already in use")
	ErrNotApprovable = errors.New("offerings: not pending approval")
)

func collection() *mongo.Collection { return database.GetDB().Collection("offerings") }

func EnsureIndexes(ctx context.Context) error {
	_, err := collection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "offering_id", Value: 1}}, Options: options.Index().SetUnique(true),
	})
	return err
}

type CreateInput struct {
	OfferingID         string
	Title              string
	AssetType          string
	Location           string
	Description        string
	ImageURL           string
	TotalUnits         float64
	PricePerUnit       float64
	Currency           string
	MinInvestmentUnits float64
	ExpectedYieldPct   float64
	RiskLevel          string
	Chain              string // Stellar-only for now — validated by the caller (HTTP handler)
	IssuerUserID       string
	Details            Details
}

// Create deposits the offering as a comet-engine receipt (pending_approval
// — no units exist on-chain yet) and stores a local projection of it.
func Create(ctx context.Context, ce *cometengine.Client, in CreateInput) (*Offering, error) {
	receipt, err := ce.DepositOffering(ctx, in.IssuerUserID, in.Chain, map[string]interface{}{
		"offeringId":         in.OfferingID,
		"assetType":          in.AssetType,
		"title":              in.Title,
		"location":           in.Location,
		"description":        in.Description,
		"totalUnits":         in.TotalUnits,
		"pricePerUnit":       in.PricePerUnit,
		"currency":           in.Currency,
		"minInvestmentUnits": in.MinInvestmentUnits,
		"imageUrl":           in.ImageURL,
	})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	o := &Offering{
		OfferingID: in.OfferingID, Title: in.Title, AssetType: in.AssetType,
		Location: in.Location, Description: in.Description, ImageURL: in.ImageURL,
		TotalUnits: in.TotalUnits, PricePerUnit: in.PricePerUnit, Currency: in.Currency,
		MinInvestmentUnits: in.MinInvestmentUnits, ExpectedYieldPct: in.ExpectedYieldPct,
		RiskLevel: in.RiskLevel, Chain: in.Chain,
		Symbol: receipt.Symbol, ReceiptID: receipt.ID, IssuerUserID: in.IssuerUserID,
		Status: StatusPendingApproval, CreatedAt: now, UpdatedAt: now,
		Details: in.Details,
	}
	res, err := collection().InsertOne(ctx, o)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	o.ID = res.InsertedID.(primitive.ObjectID)
	return o, nil
}

// Approve verifies the underlying receipt — minting the full unit supply
// to the issuer's treasury wallet in one on-chain transaction — and flips
// the offering live.
// ReservedOwnerPct is the fraction of a receipt's full minted supply set
// aside for the real-world asset's original owner the moment it goes
// live — "skin in the game" kept out of the public-sale pool from the
// start, not something the owner has to separately buy back in. Every
// place that reads "how much is available to investors" (AssetBalance
// against the treasury identity) reflects this automatically, since the
// reserve transfer happens once, right after mint, before any investor can
// buy — see fundedPct in the API layer for the one place that has to
// account for it explicitly (the public pool is TotalUnits minus this).
const ReservedOwnerPct = 0.20

// unitDecimals matches YESHARA_PRIMARY_OFFERING's decimals in comet-engine
// — every Yeshara offering shares one standard today, so this is safe to
// fix here rather than look up per call. Duplicated from internal/investments
// (unexported there) rather than shared, since both are one-line constants
// tied to the same standard, not a growing piece of logic.
const unitDecimals = 2

// Approve verifies the underlying receipt — minting the full unit supply
// to the issuer's treasury wallet in one on-chain transaction — flips the
// offering live, and immediately transfers ReservedOwnerPct of that supply
// out to a per-offering owner wallet, distinct from the shared public-sale
// treasury identity. The reserve transfer is best-effort: if it fails, the
// offering still goes live off the successful mint (same
// needs-manual-reconciliation posture as ConfirmPayment) — ReservedUnits
// stays 0 and ReserveTxHash stays empty so it's visibly unresolved rather
// than silently wrong.
func Approve(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, approvedBy string) (*Offering, string, error) {
	o, err := Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if o.Status != StatusPendingApproval {
		return nil, "", ErrNotApprovable
	}
	txHash, err := ce.ApproveOffering(ctx, o.ReceiptID, approvedBy)
	if err != nil {
		return nil, "", err
	}

	ownerID := "owner-" + o.OfferingID
	reserved := o.TotalUnits * ReservedOwnerPct
	var reserveTxHash string
	if reserved > 0 {
		if addr, werr := ce.EnsureWallet(ctx, ownerID, o.Chain); werr == nil {
			amountBase := int64(math.Round(reserved * math.Pow10(unitDecimals)))
			if h, terr := ce.TransferUnits(ctx, o.Symbol, o.Chain, o.IssuerUserID, addr, amountBase); terr == nil {
				reserveTxHash = h
			}
		}
	}

	now := time.Now().UTC()
	set := bson.M{"status": StatusLive, "updated_at": now, "owner_user_id": ownerID}
	if reserveTxHash != "" {
		set["reserved_units"] = reserved
		set["reserve_tx_hash"] = reserveTxHash
	}
	_, err = collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return nil, txHash, err
	}
	o.Status = StatusLive
	o.OwnerUserID = ownerID
	if reserveTxHash != "" {
		o.ReservedUnits = reserved
		o.ReserveTxHash = reserveTxHash
	}
	return o, txHash, nil
}

func Reject(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, reason string) error {
	o, err := Get(ctx, id)
	if err != nil {
		return err
	}
	if o.Status != StatusPendingApproval {
		return ErrNotApprovable
	}
	if err := ce.RejectOffering(ctx, o.ReceiptID, reason); err != nil {
		return err
	}
	_, err = collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": StatusRejected, "updated_at": time.Now().UTC()}})
	return err
}

var ErrInvalidPrice = errors.New("offerings: newPricePerUnit must be positive")

// Revalue is the manual price-appreciation lever: real-world factors that
// move an asset's value (occupancy, a new development nearby, flood
// damage, an impending demolition) aren't things this system can observe
// or model, so it doesn't try — an admin judges the new value and this
// records it as a price change plus one performance-history entry, exactly
// like the existing Details.PerformanceHistory rows already shown on the
// offering detail page. Every place that reads PricePerUnit live (portfolio
// value, marketplace price) reflects the new price immediately.
func Revalue(ctx context.Context, id primitive.ObjectID, newPricePerUnit float64, reason string) (*Offering, error) {
	if newPricePerUnit <= 0 {
		return nil, ErrInvalidPrice
	}
	o, err := Get(ctx, id)
	if err != nil {
		return nil, err
	}
	old := o.PricePerUnit
	var returnPct float64
	if old > 0 {
		returnPct = (newPricePerUnit - old) / old * 100
	}
	entry := PerformancePeriod{
		Label:      time.Now().UTC().Format("Jan 2, 2006") + " revaluation: " + reason,
		StartPrice: old,
		EndPrice:   newPricePerUnit,
		ReturnPct:  returnPct,
	}
	_, err = collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$set":  bson.M{"price_per_unit": newPricePerUnit, "updated_at": time.Now().UTC()},
		"$push": bson.M{"details.performance_history": entry},
	})
	if err != nil {
		return nil, err
	}
	return Get(ctx, id)
}

// UpdateDetails replaces an offering's fact-sheet content — independent of
// approval status, since these are display-only and don't touch comet-engine.
func UpdateDetails(ctx context.Context, id primitive.ObjectID, details Details) (*Offering, error) {
	_, err := collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"details": details, "updated_at": time.Now().UTC()}})
	if err != nil {
		return nil, err
	}
	return Get(ctx, id)
}

func Get(ctx context.Context, id primitive.ObjectID) (*Offering, error) {
	var o Offering
	err := collection().FindOne(ctx, bson.M{"_id": id}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &o, err
}

func GetByOfferingID(ctx context.Context, offeringID string) (*Offering, error) {
	var o Offering
	err := collection().FindOne(ctx, bson.M{"offering_id": offeringID}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &o, err
}

// SetOwnerAccount links an offering to a real owners.Owner login — the
// admin action that grants that person visibility into the reserved stake
// (see Offering.OwnerAccountID).
func SetOwnerAccount(ctx context.Context, offeringID primitive.ObjectID, ownerAccountID primitive.ObjectID) (*Offering, error) {
	_, err := collection().UpdateOne(ctx, bson.M{"_id": offeringID}, bson.M{"$set": bson.M{"owner_account_id": ownerAccountID, "updated_at": time.Now().UTC()}})
	if err != nil {
		return nil, err
	}
	return Get(ctx, offeringID)
}

// ListByOwnerAccount returns every offering linked to a given owner login,
// newest first — the feed behind an owner's dashboard.
func ListByOwnerAccount(ctx context.Context, ownerAccountID primitive.ObjectID) ([]Offering, error) {
	cur, err := collection().Find(ctx, bson.M{"owner_account_id": ownerAccountID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Offering{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// List returns offerings, optionally filtered by status ("" = all),
// newest first.
func List(ctx context.Context, status string) ([]Offering, error) {
	filter := bson.M{}
	if status != "" {
		filter["status"] = status
	}
	cur, err := collection().Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Offering{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
