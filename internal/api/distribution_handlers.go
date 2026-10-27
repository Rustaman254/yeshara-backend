package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"yeshara.network/marketplace/internal/cometengine"
	"yeshara.network/marketplace/internal/distributions"
	"yeshara.network/marketplace/internal/investors"
)

// triggerDistribution handles POST /api/v1/admin/offerings/:offeringId/distributions
// — an admin declares a pool of income for a period, this computes the
// pro-rata split across current holders, and (depending on payoutMethod)
// either pays it out immediately (onchain_token, mpesa_b2c) or leaves it
// for manual settlement.
func (s *Server) triggerDistribution(c *gin.Context) {
	offeringOID, o, err := s.offeringObjectID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	var req struct {
		PeriodLabel       string  `json:"periodLabel" binding:"required"`
		TotalAmount       float64 `json:"totalAmount" binding:"required,gt=0"`
		PayoutMethod      string  `json:"payoutMethod" binding:"required"`
		PayoutAssetSymbol string  `json:"payoutAssetSymbol"`
		CreatedBy         string  `json:"createdBy" binding:"required"`
		// DistributionType is what the payout is; Yeshara distributes
		// property income, so it defaults to rental. A Shariah offering
		// accepts only rental, profit_share, sale_proceeds or capital_return.
		DistributionType string `json:"distributionType"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.DistributionType == "" {
		req.DistributionType = "rental"
	}
	if err := o.CheckDistributionType(req.DistributionType); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	method := distributions.PayoutMethod(req.PayoutMethod)
	if method != distributions.PayoutManual && method != distributions.PayoutMpesaB2C && method != distributions.PayoutOnchain {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payoutMethod must be \"manual\", \"mpesa_b2c\", or \"onchain_token\""})
		return
	}
	if method == distributions.PayoutOnchain && req.PayoutAssetSymbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payoutAssetSymbol is required for the onchain_token rail"})
		return
	}

	dist, entries, err := distributions.Compute(c.Request.Context(), offeringOID, req.PeriodLabel, req.TotalAmount, o.Currency, method, req.PayoutAssetSymbol, req.CreatedBy)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, distributions.ErrNoHolders) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	switch method {
	case distributions.PayoutOnchain:
		payouts := make([]cometengine.Payout, 0, len(entries))
		byExternalID := map[string]primitive.ObjectID{}
		for _, e := range entries {
			inv, err := investors.GetByID(ctx, e.InvestorID)
			if err != nil {
				continue
			}
			payouts = append(payouts, cometengine.Payout{ExternalUserID: inv.ExternalUserID(), Amount: e.Amount})
			byExternalID[inv.ExternalUserID()] = e.ID
		}
		result, err := s.ce.Distribute(ctx, o.ReceiptID, req.PayoutAssetSymbol, dist.ID.Hex(), req.DistributionType, payouts)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "comet-engine distribute failed", "details": err.Error(), "distribution": dist})
			return
		}
		for _, r := range result.Results {
			entryID, ok := byExternalID[r.ExternalUserID]
			if !ok {
				continue
			}
			if r.Error != "" {
				_ = distributions.SetEntryResult(ctx, entryID, distributions.EntryFailed, "", r.Error)
			} else {
				_ = distributions.SetEntryResult(ctx, entryID, distributions.EntryPaid, r.TxHash, "")
			}
		}

	case distributions.PayoutMpesaB2C:
		available, err := s.ce.TreasuryBalance(ctx)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to check comet-engine treasury balance", "details": err.Error(), "distribution": dist})
			return
		}
		requiredKES, err := amountInKES(req.TotalAmount, o.Currency, s.cfg.USDKESRate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "distribution": dist})
			return
		}
		if available < requiredKES {
			_ = distributions.SetDistributionStatus(ctx, dist.ID, distributions.DistributionFundingRequired)
			c.JSON(http.StatusOK, gin.H{
				"distribution": dist, "entries": entries,
				"status": "funding_required", "treasuryAvailable": available, "required": requiredKES,
				"message": "comet-engine's KES treasury doesn't hold enough to pay this out automatically — fund it (e.g. an admin STK-push-to-self) or switch entries to the manual rail.",
			})
			return
		}
		for _, e := range entries {
			inv, err := investors.GetByID(ctx, e.InvestorID)
			if err != nil || inv.PayoutPhone == "" {
				_ = distributions.SetEntryResult(ctx, e.ID, distributions.EntryFailed, "", "no payout phone on file for this investor")
				continue
			}
			kesAmount, err := amountInKES(e.Amount, e.Currency, s.cfg.USDKESRate)
			if err != nil {
				_ = distributions.SetEntryResult(ctx, e.ID, distributions.EntryFailed, "", err.Error())
				continue
			}
			result, err := s.ce.B2CTransfer(ctx, inv.ExternalUserID(), inv.PayoutPhone, "M-Pesa", fmt.Sprintf("yield-%s-%s", dist.ID.Hex(), e.ID.Hex()), kesAmount)
			if err != nil {
				_ = distributions.SetEntryResult(ctx, e.ID, distributions.EntryFailed, "", err.Error())
				continue
			}
			// comet-engine's B2C transfer is async (a PSP callback resolves the
			// final outcome on comet-engine's side, not ours) — "processing" is
			// as far as this call can honestly confirm.
			_ = distributions.SetEntryResult(ctx, e.ID, distributions.EntryProcessing, result.TransactionID, "")
		}

	case distributions.PayoutManual:
		// Entries stay pending — an admin settles each with mark-paid.
	}

	_ = distributions.RecomputeStatus(ctx, dist.ID)
	updated, _ := distributions.Get(ctx, dist.ID)
	updatedEntries, _ := distributions.ListEntriesByDistribution(ctx, dist.ID)
	c.JSON(http.StatusOK, gin.H{"distribution": updated, "entries": updatedEntries})
}

// markDistributionEntryPaid handles
// POST /api/v1/admin/distributions/entries/:entryId/mark-paid — the
// manual rail's settlement confirmation.
func (s *Server) markDistributionEntryPaid(c *gin.Context) {
	entryID, err := primitive.ObjectIDFromHex(c.Param("entryId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid entry id"})
		return
	}
	var req struct {
		PaymentRef string `json:"paymentRef" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	entry, err := distributions.GetEntry(c.Request.Context(), entryID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err := distributions.SetEntryResult(c.Request.Context(), entryID, distributions.EntryPaid, req.PaymentRef, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = distributions.RecomputeStatus(c.Request.Context(), entry.DistributionID)
	updated, _ := distributions.GetEntry(c.Request.Context(), entryID)
	c.JSON(http.StatusOK, gin.H{"entry": updated})
}

// listOfferingDistributions handles GET /api/v1/admin/offerings/:offeringId/distributions
func (s *Server) listOfferingDistributions(c *gin.Context) {
	offeringOID, _, err := s.offeringObjectID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	list, err := distributions.ListByOffering(c.Request.Context(), offeringOID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"distributions": list, "count": len(list)})
}

// listDistributionEntries handles GET /api/v1/admin/distributions/:id/entries
func (s *Server) listDistributionEntries(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid distribution id"})
		return
	}
	entries, err := distributions.ListEntriesByDistribution(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries, "count": len(entries)})
}

// myYield handles GET /api/v1/me/yield — every distribution entry the
// investor has ever been owed, plus running totals by currency, the number
// the portfolio page surfaces as "value from holding."
func (s *Server) myYield(c *gin.Context) {
	inv := currentInvestor(c)
	entries, err := distributions.ListByInvestor(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	earned := map[string]float64{}
	pending := map[string]float64{}
	for _, e := range entries {
		switch e.Status {
		case distributions.EntryPaid:
			earned[e.Currency] += e.Amount
		case distributions.EntryPending, distributions.EntryProcessing:
			pending[e.Currency] += e.Amount
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"entries":           entries,
		"earnedByCurrency":  earned,
		"pendingByCurrency": pending,
	})
}
