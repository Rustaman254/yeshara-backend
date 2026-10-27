package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"yeshara.network/marketplace/internal/cometengine"
	"yeshara.network/marketplace/internal/investments"
	"yeshara.network/marketplace/internal/offerings"
)

// liveOffering adds real-time funded-percent info to an offering for the
// marketplace grid — same on-chain treasury-balance technique getOffering
// uses for the detail page, just applied per card.
type liveOffering struct {
	offerings.Offering
	AvailableUnits *float64 `json:"availableUnits,omitempty"`
	UnitsSold      *float64 `json:"unitsSold,omitempty"`
	FundedPct      *float64 `json:"fundedPct,omitempty"`
	KesPerUnit     *float64 `json:"kesPerUnit,omitempty"`
}

// listLiveOfferings handles GET /api/v1/offerings — the primary market
// grid (securitize.io's /primary-market equivalent). Public, no auth.
func (s *Server) listLiveOfferings(c *gin.Context) {
	list, err := offerings.List(c.Request.Context(), string(offerings.StatusLive))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	shariahOnly := c.Query("shariah") == "true"

	out := make([]liveOffering, 0, len(list))
	for _, o := range list {
		if shariahOnly && o.Shariah == nil {
			continue
		}
		lo := liveOffering{Offering: o}
		if available, err := s.ce.AssetBalance(c.Request.Context(), o.Symbol, o.Chain, s.cfg.TreasuryUserID); err == nil {
			// publicPool excludes whatever went to the owner's reserve at
			// approval (ReservedUnits is 0 if that transfer never happened,
			// which correctly falls back to the full supply) — otherwise a
			// freshly-approved offering would show as already partly
			// "funded" just from the reserve leaving the treasury.
			publicPool := o.TotalUnits - o.ReservedUnits
			sold := publicPool - available
			lo.AvailableUnits = &available
			lo.UnitsSold = &sold
			if publicPool > 0 {
				pct := sold / publicPool * 100
				lo.FundedPct = &pct
			}
		}
		if kes, err := amountInKES(o.PricePerUnit, o.Currency, s.cfg.USDKESRate); err == nil {
			lo.KesPerUnit = &kes
		}
		out = append(out, lo)
	}
	c.JSON(http.StatusOK, gin.H{"offerings": out, "count": len(out)})
}

// getOffering handles GET /api/v1/offerings/:offeringId — public detail,
// including real-time remaining supply (the treasury identity's current
// on-chain balance of the offering's symbol — decreases as investments are
// confirmed, so this is never stale the way a cached "unitsSold" counter
// could be).
//
// Public visibility is intentionally restricted to StatusLive: an asset
// still moving through the Manager A/B -> Trustee A/B chain (or rejected,
// or delisted) isn't discoverable here even by someone who already has its
// slug — same 404 either way, so a probing request can't distinguish "no
// such offering" from "exists but isn't approved yet." listLiveOfferings
// (the marketplace grid) already filtered to live-only; this closes the
// same gap on direct-by-slug access to the detail endpoint.
func (s *Server) getOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil || o.Status != offerings.StatusLive {
		c.JSON(http.StatusNotFound, gin.H{"error": "offering not found"})
		return
	}

	resp := gin.H{"offering": o}
	available, err := s.ce.AssetBalance(c.Request.Context(), o.Symbol, o.Chain, s.cfg.TreasuryUserID)
	if err == nil {
		publicPool := o.TotalUnits - o.ReservedUnits
		sold := publicPool - available
		resp["availableUnits"] = available
		resp["unitsSold"] = sold
		if publicPool > 0 {
			resp["fundedPct"] = sold / publicPool * 100
		}
	}
	if kes, err := amountInKES(o.PricePerUnit, o.Currency, s.cfg.USDKESRate); err == nil {
		resp["kesPerUnit"] = kes
	}
	c.JSON(http.StatusOK, resp)
}

// ── admin ────────────────────────────────────────────────────────────────

func (s *Server) createOffering(c *gin.Context) {
	var req struct {
		OfferingID         string            `json:"offeringId" binding:"required"`
		Title              string            `json:"title" binding:"required"`
		AssetType          string            `json:"assetType" binding:"required"`
		Location           string            `json:"location"`
		Description        string            `json:"description"`
		ImageURL           string            `json:"imageUrl"`
		TotalUnits         float64           `json:"totalUnits" binding:"required"`
		PricePerUnit       float64           `json:"pricePerUnit" binding:"required"`
		Currency           string            `json:"currency" binding:"required"`
		MinInvestmentUnits float64           `json:"minInvestmentUnits"`
		ExpectedYieldPct   float64           `json:"expectedYieldPct"`
		RiskLevel          string            `json:"riskLevel"`
		Details            offerings.Details `json:"details"`
		// Shariah makes this a Shariah-compliant offering. The engine
		// screens it at once (structure, sector, debt and income ratios);
		// the board's certificate can follow before Trustee B tokenizes.
		Shariah *cometengine.ShariahProfile `json:"shariah"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	o, err := offerings.Create(c.Request.Context(), s.ce, offerings.CreateInput{
		OfferingID: strings.ToLower(strings.TrimSpace(req.OfferingID)), Title: req.Title,
		AssetType: req.AssetType, Location: req.Location, Description: req.Description,
		ImageURL: req.ImageURL, TotalUnits: req.TotalUnits, PricePerUnit: req.PricePerUnit,
		Currency: req.Currency, MinInvestmentUnits: req.MinInvestmentUnits,
		ExpectedYieldPct: req.ExpectedYieldPct, RiskLevel: req.RiskLevel,
		Chain:        "stellar", // primary market is Stellar-only for now
		IssuerUserID: s.cfg.TreasuryUserID,
		Details:      req.Details,
		Shariah:      req.Shariah,
	})
	if err != nil {
		status := http.StatusInternalServerError
		var apiErr *cometengine.APIError
		switch {
		case errors.Is(err, offerings.ErrSlugTaken):
			status = http.StatusConflict
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity:
			status = http.StatusUnprocessableEntity // failed Shariah screening
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, o)
}

func (s *Server) listAllOfferings(c *gin.Context) {
	list, err := offerings.List(c.Request.Context(), c.Query("status"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offerings": list, "count": len(list)})
}

// Offering approval is no longer a single admin action — see
// internal/api/staff_handlers.go's staffReview/staffReject, gated by the
// four-seat Manager A/B -> Trustee A/B chain in internal/offerings.

// delistOffering handles POST /api/v1/admin/offerings/:offeringId/delist —
// takes a live offering off the public marketplace (getOffering/
// listLiveOfferings both already exclude anything that isn't StatusLive).
// The on-chain asset and every investor's existing holding are untouched.
func (s *Server) delistOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		DelistedBy string `json:"delistedBy" binding:"required"`
		Reason     string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := offerings.Delist(c.Request.Context(), o.ID, req.DelistedBy, req.Reason)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrNotListed) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offering": updated})
}

// relistOffering handles POST /api/v1/admin/offerings/:offeringId/relist —
// reverses a delist.
func (s *Server) relistOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	updated, err := offerings.Relist(c.Request.Context(), o.ID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrNotDelisted) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offering": updated})
}

// revalueOffering handles POST /api/v1/admin/offerings/:offeringId/revalue —
// the manual price-appreciation/depreciation lever. Real-world causes
// (occupancy, nearby development, damage, planned demolition) aren't
// something this system observes; an admin judges the new value and this
// records it, appending one row to the offering's performance history.
func (s *Server) revalueOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		NewPricePerUnit float64 `json:"newPricePerUnit" binding:"required,gt=0"`
		Reason          string  `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := offerings.Revalue(c.Request.Context(), o.ID, req.NewPricePerUnit, req.Reason)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrInvalidPrice) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offering": updated})
}

// updateOfferingDetails handles PATCH /api/v1/admin/offerings/:offeringId/details
// — edits the fact-sheet content (tags, subscription/redemption terms,
// performance history, manager bio, documents) independent of approval
// status, since none of it is sent to comet-engine.
func (s *Server) updateOfferingDetails(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var details offerings.Details
	if err := c.ShouldBindJSON(&details); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := offerings.UpdateDetails(c.Request.Context(), o.ID, details)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offering": updated})
}

// offeringObjectID resolves the :offeringId route param (a slug) to the
// local Mongo ID investments are keyed by.
func (s *Server) offeringObjectID(c *gin.Context) (primitive.ObjectID, *offerings.Offering, error) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		return primitive.NilObjectID, nil, err
	}
	return o.ID, o, nil
}

// removeOffering handles POST /api/v1/admin/offerings/:offeringId/remove —
// retires a tokenized (live or delisted) offering. Refused with 409 while
// investors hold units or payments are pending; otherwise the remaining
// treasury and owner-reserve units are burned on the engine.
func (s *Server) removeOffering(c *gin.Context) {
	ctx := c.Request.Context()
	o, err := offerings.GetByOfferingID(ctx, c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		RemovedBy string `json:"removedBy" binding:"required"`
		Reason    string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pending, err := investments.ListByOffering(ctx, o.ID, string(investments.StatusPendingPayment))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	removed, err := offerings.Remove(ctx, s.ce, o.ID, s.cfg.TreasuryUserID, req.RemovedBy, req.Reason, len(pending))
	var holdErr *offerings.InvestorsHoldError
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"offering": removed})
	case errors.As(err, &holdErr):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "investorUnits": holdErr.Units})
	case errors.Is(err, offerings.ErrNotRemovable), errors.Is(err, offerings.ErrPendingPayments):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}

// certifyShariahOffering handles POST /api/v1/admin/offerings/:offeringId/shariah-certification
// — records the Shariah board's certificate, required before tokenizing.
func (s *Server) certifyShariahOffering(c *gin.Context) {
	ctx := c.Request.Context()
	o, err := offerings.GetByOfferingID(ctx, c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var cert cometengine.ShariahCertification
	if err := c.ShouldBindJSON(&cert); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := offerings.CertifyShariah(ctx, s.ce, o.ID, cert)
	var apiErr *cometengine.APIError
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"offering": updated})
	case errors.Is(err, offerings.ErrNotShariah), errors.Is(err, offerings.ErrWrongStage):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity:
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": apiErr.Message})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}
