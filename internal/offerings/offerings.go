// Package offerings is Yeshara's primary-market listings — the securitize.io
// "primary market" concept. Each Offering is a thin, cached projection of a
// comet-engine receipt: comet-engine remains the source of truth for
// tokenization state (symbol, status, on-chain quantity), while this local
// record exists for fast listing pages and Yeshara-specific display fields.
package offerings

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"yeshara.network/marketplace/internal/cometengine"
	"yeshara.network/marketplace/internal/database"
)

type Status string

// The approval chain is four sequential, separately-authenticated seats —
// see internal/staff.Role. Each status names whose desk the offering is
// currently on; the transition functions below (ManagerAReview,
// ManagerBReview, TrusteeAApprove, TrusteeBTokenize) are the only way to
// move it forward, and each checks the offering is still on its own status
// before doing anything — same atomic guard-then-update shape as
// comet-engine's receipts.MarkVerified, so two concurrent actions on the
// same offering can't both succeed.
const (
	StatusPendingManagerA Status = "pending_manager_a" // submitted, awaiting Manager A's (maker) review
	StatusPendingManagerB Status = "pending_manager_b" // Manager A cleared it, awaiting Manager B's (checker) countersign
	StatusPendingTrusteeA Status = "pending_trustee_a" // Manager B cleared it, awaiting Trustee A's issuance approval
	StatusPendingTrusteeB Status = "pending_trustee_b" // Trustee A approved, awaiting Trustee B to actually tokenize
	StatusLive            Status = "live"              // Trustee B minted — units exist on-chain, open for investment
	StatusRejected        Status = "rejected"
	StatusClosed          Status = "closed" // manually closed or fully sold
	// StatusRemoved: retired from the platform. Every unit was burned on the
	// engine (only possible once no investor holds any); kept for audit.
	StatusRemoved Status = "removed"
)

// PendingStatuses is every status a staff review action can still move —
// used both to build the "any pending stage" filter Reject needs and by
// the API layer to map a staff.Role to the queue it should see.
var PendingStatuses = []Status{StatusPendingManagerA, StatusPendingManagerB, StatusPendingTrusteeA, StatusPendingTrusteeB}

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
	// MintTxHash/MintTxURL are the comet-engine tx (and its ready-to-use
	// block-explorer link) that minted this offering's units on-chain —
	// AssetURL is the asset/token's own explorer page (a different page
	// than one specific transaction). All set once, by TrusteeBTokenize.
	// Empty until then.
	MintTxHash   string    `bson:"mint_tx_hash,omitempty" json:"mintTxHash,omitempty"`
	MintTxURL    string    `bson:"mint_tx_url,omitempty" json:"mintTxUrl,omitempty"`
	AssetURL     string    `bson:"asset_url,omitempty" json:"assetUrl,omitempty"`
	IssuerUserID string    `bson:"issuer_user_id" json:"issuerUserId"`
	Status       Status    `bson:"status" json:"status"`
	CreatedAt    time.Time `bson:"created_at" json:"createdAt"`
	UpdatedAt    time.Time `bson:"updated_at" json:"updatedAt"`

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

	// Approval-chain audit trail — who (by staff email) cleared each stage
	// and when. Populated one field at a time as the offering moves through
	// ManagerAReview -> ManagerBReview -> TrusteeAApprove -> TrusteeBTokenize.
	ManagerAReviewedBy  string     `bson:"manager_a_reviewed_by,omitempty" json:"managerAReviewedBy,omitempty"`
	ManagerAReviewedAt  *time.Time `bson:"manager_a_reviewed_at,omitempty" json:"managerAReviewedAt,omitempty"`
	ManagerBReviewedBy  string     `bson:"manager_b_reviewed_by,omitempty" json:"managerBReviewedBy,omitempty"`
	ManagerBReviewedAt  *time.Time `bson:"manager_b_reviewed_at,omitempty" json:"managerBReviewedAt,omitempty"`
	TrusteeAApprovedBy  string     `bson:"trustee_a_approved_by,omitempty" json:"trusteeAApprovedBy,omitempty"`
	TrusteeAApprovedAt  *time.Time `bson:"trustee_a_approved_at,omitempty" json:"trusteeAApprovedAt,omitempty"`
	TrusteeBTokenizedBy string     `bson:"trustee_b_tokenized_by,omitempty" json:"trusteeBTokenizedBy,omitempty"`
	TrusteeBTokenizedAt *time.Time `bson:"trustee_b_tokenized_at,omitempty" json:"trusteeBTokenizedAt,omitempty"`

	// RejectedBy/RejectedStage/RejectedReason record who killed the
	// submission and at which desk it was sitting when they did — any of
	// the four stages can reject, not just the last one.
	RejectedBy     string `bson:"rejected_by,omitempty" json:"rejectedBy,omitempty"`
	RejectedStage  string `bson:"rejected_stage,omitempty" json:"rejectedStage,omitempty"`
	RejectedReason string `bson:"rejected_reason,omitempty" json:"rejectedReason,omitempty"`

	// DelistedBy/At/Reason record a Delist call — the offering (and the
	// on-chain asset, and every investor's existing holding) is untouched;
	// this only stops it from being publicly listed/discoverable and
	// accepting new investment. See Delist/Relist.
	DelistedBy     string     `bson:"delisted_by,omitempty" json:"delistedBy,omitempty"`
	DelistedAt     *time.Time `bson:"delisted_at,omitempty" json:"delistedAt,omitempty"`
	DelistedReason string     `bson:"delisted_reason,omitempty" json:"delistedReason,omitempty"`

	// Shariah is set for Shariah-compliant offerings, which live in the
	// engine's separate YESHARA_SHARIAH_OFFERING pool and can only be
	// tokenized once the Shariah board's certificate is attached.
	Shariah *cometengine.ShariahProfile `bson:"shariah,omitempty" json:"shariah,omitempty"`

	// Network is the engine network the offering was tokenized on.
	Network string `bson:"network,omitempty" json:"network,omitempty"`

	RemovedBy      string     `bson:"removed_by,omitempty" json:"removedBy,omitempty"`
	RemovedAt      *time.Time `bson:"removed_at,omitempty" json:"removedAt,omitempty"`
	RemovedReason  string     `bson:"removed_reason,omitempty" json:"removedReason,omitempty"`
	RemovalBurnTxs []string   `bson:"removal_burn_txs,omitempty" json:"removalBurnTxs,omitempty"`

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
	ErrNotFound   = errors.New("offerings: not found")
	ErrSlugTaken  = errors.New("offerings: offeringId already in use")
	ErrWrongStage = errors.New("offerings: not at the expected approval stage")
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
	// Shariah makes this a Shariah-compliant offering (separate pool).
	Shariah *cometengine.ShariahProfile
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
	}, in.Shariah)
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
		Status: StatusPendingManagerA, CreatedAt: now, UpdatedAt: now,
		Details: in.Details, Shariah: receipt.Shariah, Network: ce.Network(),
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

// advanceStage atomically moves an offering from `from` to `to`, merging in
// setFields (the actor/timestamp columns for this stage) — the same
// guard-then-update shape comet-engine's receipts.MarkVerified uses, so two
// concurrent review calls on the same offering (a double-click, two staff
// racing the same item) can't both succeed: the loser's UpdateOne matches
// nothing and comes back ErrWrongStage instead of silently double-applying.
func advanceStage(ctx context.Context, id primitive.ObjectID, from, to Status, setFields bson.M) (*Offering, error) {
	setFields["status"] = to
	setFields["updated_at"] = time.Now().UTC()
	res, err := collection().UpdateOne(ctx, bson.M{"_id": id, "status": from}, bson.M{"$set": setFields})
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, ErrWrongStage
	}
	return Get(ctx, id)
}

// ManagerAReview is the maker's countersign: Manager A has looked at the
// submission and forwards it to Manager B. Only valid from
// StatusPendingManagerA.
func ManagerAReview(ctx context.Context, id primitive.ObjectID, reviewedBy string) (*Offering, error) {
	return advanceStage(ctx, id, StatusPendingManagerA, StatusPendingManagerB, bson.M{
		"manager_a_reviewed_by": reviewedBy, "manager_a_reviewed_at": time.Now().UTC(),
	})
}

// ManagerBReview is the checker's countersign: Manager B has independently
// verified Manager A's review and forwards it to Trustee A. Only valid
// from StatusPendingManagerB.
func ManagerBReview(ctx context.Context, id primitive.ObjectID, reviewedBy string) (*Offering, error) {
	return advanceStage(ctx, id, StatusPendingManagerB, StatusPendingTrusteeA, bson.M{
		"manager_b_reviewed_by": reviewedBy, "manager_b_reviewed_at": time.Now().UTC(),
	})
}

// TrusteeAApprove approves the issuing account for the property being
// tokenized and forwards to Trustee B, who does the actual tokenization.
// Only valid from StatusPendingTrusteeA.
func TrusteeAApprove(ctx context.Context, id primitive.ObjectID, approvedBy string) (*Offering, error) {
	return advanceStage(ctx, id, StatusPendingTrusteeA, StatusPendingTrusteeB, bson.M{
		"trustee_a_approved_by": approvedBy, "trustee_a_approved_at": time.Now().UTC(),
	})
}

// TrusteeBTokenize is the actual tokenization moment: Trustee B verifies
// the underlying receipt — minting the full unit supply to the issuer's
// treasury wallet in one on-chain transaction — flips the offering live,
// and immediately transfers ReservedOwnerPct of that supply out to a
// per-offering owner wallet, distinct from the shared public-sale treasury
// identity. The reserve transfer is best-effort: if it fails, the offering
// still goes live off the successful mint (same needs-manual-reconciliation
// posture as ConfirmPayment) — ReservedUnits stays 0 and ReserveTxHash
// stays empty so it's visibly unresolved rather than silently wrong.
//
// Only valid from StatusPendingTrusteeB — checked once before minting (so
// a wrong-stage call fails fast without touching comet-engine) and again,
// atomically, on the write that records the mint, so two concurrent calls
// can't both flip the offering live even though the pre-check alone
// couldn't prevent both from reaching comet-engine.
func TrusteeBTokenize(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, tokenizedBy string) (*Offering, string, error) {
	o, err := Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if o.Status != StatusPendingTrusteeB {
		return nil, "", ErrWrongStage
	}
	// If the engine held the first attempt for maker-checker approval and
	// its signers have since approved, the receipt is already minted:
	// finish recording it instead of verifying (and failing) again.
	var txHash, txURL, assetURL string
	receipt, err := ce.GetReceipt(ctx, o.ReceiptID)
	if err != nil {
		return nil, "", err
	}
	if receipt.Status == "active" || receipt.Status == "partially_redeemed" {
		txHash, txURL, assetURL = receipt.MintTxHash, receipt.TxURL, receipt.AssetURL
	} else if txHash, txURL, assetURL, err = ce.ApproveOffering(ctx, o.ReceiptID, tokenizedBy); err != nil {
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
	set := bson.M{
		"status": StatusLive, "updated_at": now, "owner_user_id": ownerID,
		"trustee_b_tokenized_by": tokenizedBy, "trustee_b_tokenized_at": now,
	}
	if txHash != "" {
		set["mint_tx_hash"] = txHash
		set["mint_tx_url"] = txURL
		set["asset_url"] = assetURL
	}
	if reserveTxHash != "" {
		set["reserved_units"] = reserved
		set["reserve_tx_hash"] = reserveTxHash
	}
	res, err := collection().UpdateOne(ctx, bson.M{"_id": id, "status": StatusPendingTrusteeB}, bson.M{"$set": set})
	if err != nil {
		return nil, txHash, err
	}
	if res.MatchedCount == 0 {
		// Minted on-chain but the offering had already moved off
		// pending_trustee_b by the time we tried to record it (a
		// concurrent TrusteeBTokenize call, most likely) — surface this
		// loudly rather than silently losing track of it, same posture as
		// comet-engine's own mintReceipt/MarkVerified gap.
		return nil, txHash, fmt.Errorf("offerings: minted on-chain (tx %s) but offering left pending_trustee_b before it could be recorded — needs manual reconciliation", txHash)
	}
	updated, err := Get(ctx, id)
	return updated, txHash, err
}

// Reject can happen from any of the four pending stages — the desk it was
// sitting on when rejected is recorded in RejectedStage/RejectedBy so the
// audit trail shows exactly where the chain stopped.
func Reject(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, rejectedBy, stage, reason string) error {
	o, err := Get(ctx, id)
	if err != nil {
		return err
	}
	isPending := false
	for _, s := range PendingStatuses {
		if o.Status == s {
			isPending = true
			break
		}
	}
	if !isPending {
		return ErrWrongStage
	}
	if err := ce.RejectOffering(ctx, o.ReceiptID, reason); err != nil {
		return err
	}
	res, err := collection().UpdateOne(ctx, bson.M{"_id": id, "status": bson.M{"$in": PendingStatuses}}, bson.M{"$set": bson.M{
		"status": StatusRejected, "updated_at": time.Now().UTC(),
		"rejected_by": rejectedBy, "rejected_stage": stage, "rejected_reason": reason,
	}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrWrongStage
	}
	return nil
}

var ErrNotListed = errors.New("offerings: not currently live")
var ErrNotDelisted = errors.New("offerings: not currently closed")

// Delist takes a live offering off the public marketplace — sets it to
// StatusClosed, which listLiveOfferings/getOffering already exclude from
// public view. Deliberately not a deletion: the on-chain asset, its
// mint/reserve tx history, and every investor's existing holding are
// completely untouched — this only stops new discovery and new investment.
func Delist(ctx context.Context, id primitive.ObjectID, delistedBy, reason string) (*Offering, error) {
	now := time.Now().UTC()
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusLive},
		bson.M{"$set": bson.M{
			"status": StatusClosed, "updated_at": now,
			"delisted_by": delistedBy, "delisted_at": now, "delisted_reason": reason,
		}},
	)
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, ErrNotListed
	}
	return Get(ctx, id)
}

// Relist reverses a Delist — closed back to live, clearing the delisted_*
// fields (a re-listing isn't "still delisted, with a reason on file", it's
// current again). Does nothing to on-chain state, same as Delist.
func Relist(ctx context.Context, id primitive.ObjectID) (*Offering, error) {
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": StatusClosed},
		bson.M{
			"$set":   bson.M{"status": StatusLive, "updated_at": time.Now().UTC()},
			"$unset": bson.M{"delisted_by": "", "delisted_at": "", "delisted_reason": ""},
		},
	)
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, ErrNotDelisted
	}
	return Get(ctx, id)
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

// ── Shariah ─────────────────────────────────────────────────────────────

var ErrNotShariah = errors.New("offerings: not a Shariah offering")

// ShariahDistributionTypes are the payout kinds a Shariah offering may make
// (mirrors comet-engine's shariah package). Anything that is a fixed or
// guaranteed return on capital is riba.
var ShariahDistributionTypes = []string{"rental", "profit_share", "sale_proceeds", "capital_return"}

// CheckDistributionType refuses a non-permitted payout kind on a Shariah
// offering; conventional offerings accept any label.
func (o *Offering) CheckDistributionType(kind string) error {
	if o.Shariah == nil {
		return nil
	}
	if slices.Contains(ShariahDistributionTypes, kind) {
		return nil
	}
	return fmt.Errorf("%q is not permitted for a Shariah offering (use one of %v)", kind, ShariahDistributionTypes)
}

// CertifyShariah attaches the Shariah board's certificate on the engine and
// mirrors it locally. Only before tokenization: the engine checks it when
// Trustee B mints.
func CertifyShariah(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, cert cometengine.ShariahCertification) (*Offering, error) {
	o, err := Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if o.Shariah == nil {
		return nil, ErrNotShariah
	}
	if !slices.Contains(PendingStatuses, o.Status) {
		return nil, ErrWrongStage
	}
	r, err := ce.CertifyShariah(ctx, o.ReceiptID, cert)
	if err != nil {
		return nil, err
	}
	if _, err := collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"shariah": r.Shariah, "updated_at": time.Now().UTC(),
	}}); err != nil {
		return nil, err
	}
	return Get(ctx, id)
}

// ── Removal ─────────────────────────────────────────────────────────────

var (
	ErrNotRemovable    = errors.New("offerings: only a live or closed (delisted) offering can be removed; reject a pending one instead")
	ErrPendingPayments = errors.New("offerings: investments are still awaiting payment")
)

// InvestorsHoldError means investors still own units, so the offering
// can't be removed until their redemptions are processed.
type InvestorsHoldError struct{ Units float64 }

func (e *InvestorsHoldError) Error() string {
	return fmt.Sprintf("offerings: investors still hold %.2f units; process their redemptions first", e.Units)
}

// Remove retires a tokenized offering. It refuses while any investor holds
// units or any payment is pending, then burns what is left in Yeshara's
// treasury and the owner's reserve through the engine (the receipt ends
// "redeemed") and marks the offering removed. Safe to retry: balances are
// re-read every time, so units already burned are never burned twice.
func Remove(ctx context.Context, ce *cometengine.Client, id primitive.ObjectID, treasuryUserID, removedBy, reason string, pendingPayments int) (*Offering, error) {
	o, err := Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if o.Status != StatusLive && o.Status != StatusClosed {
		return nil, ErrNotRemovable
	}
	if pendingPayments > 0 {
		return nil, fmt.Errorf("%w (%d)", ErrPendingPayments, pendingPayments)
	}

	receipt, err := ce.GetReceipt(ctx, o.ReceiptID)
	if err != nil {
		return nil, err
	}
	holders := []string{treasuryUserID}
	if o.OwnerUserID != "" && o.OwnerUserID != treasuryUserID {
		holders = append(holders, o.OwnerUserID)
	}
	balances := make([]float64, len(holders))
	var platformHeld float64
	for i, h := range holders {
		if balances[i], err = ce.AssetBalance(ctx, o.Symbol, o.Chain, h); err != nil {
			return nil, err
		}
		platformHeld += balances[i]
	}
	// Units are 2-decimal; anything under half a hundredth is rounding.
	if investorHeld := receipt.Outstanding() - platformHeld; investorHeld > 0.005 {
		return nil, &InvestorsHoldError{Units: investorHeld}
	}

	var burnTxs []string
	for i, h := range holders {
		if balances[i] <= 0 {
			continue
		}
		res, err := ce.Redeem(ctx, o.ReceiptID, h, fmt.Sprintf("remove-%s-%d", o.OfferingID, i), balances[i])
		if err != nil {
			return nil, fmt.Errorf("burn %s's %.2f units: %w", h, balances[i], err)
		}
		burnTxs = append(burnTxs, res.TxHash)
	}

	now := time.Now().UTC()
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": id, "status": bson.M{"$in": []Status{StatusLive, StatusClosed}}},
		bson.M{"$set": bson.M{
			"status": StatusRemoved, "updated_at": now,
			"removed_by": removedBy, "removed_at": now, "removed_reason": reason,
		}, "$push": bson.M{"removal_burn_txs": bson.M{"$each": burnTxs}}},
	)
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, ErrNotRemovable
	}
	return Get(ctx, id)
}
