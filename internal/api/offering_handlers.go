package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

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

	out := make([]liveOffering, 0, len(list))
	for _, o := range list {
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
func (s *Server) getOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	resp := gin.H{"offering": o}
	if o.Status == offerings.StatusLive {
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
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrSlugTaken) {
			status = http.StatusConflict
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

func (s *Server) approveOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		ApprovedBy string `json:"approvedBy" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, txHash, err := offerings.Approve(c.Request.Context(), s.ce, o.ID, req.ApprovedBy)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrNotApprovable) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offering": updated, "mintTxHash": txHash})
}

func (s *Server) rejectOffering(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := offerings.Reject(c.Request.Context(), s.ce, o.ID, req.Reason); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrNotApprovable) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
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
