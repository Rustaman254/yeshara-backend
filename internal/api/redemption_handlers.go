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
	"yeshara.network/marketplace/internal/offerings"
	"yeshara.network/marketplace/internal/redemptions"
)

// setPayoutPhone handles PATCH /api/v1/me/payout-phone — where yield and
// redemption payouts go on the mpesa_b2c rail. Separate from an
// investment's per-order Phone: a distribution run pays many investors in
// one batch with no chance to prompt each of them individually.
func (s *Server) setPayoutPhone(c *gin.Context) {
	inv := currentInvestor(c)
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := investors.SetPayoutPhone(c.Request.Context(), inv.ID, req.Phone); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// requestRedemption handles POST /api/v1/offerings/:offeringId/redemptions
// — an investor asks to cash out `units` of a completed holding. No tokens
// move yet; that happens on admin approval (approveRedemption).
func (s *Server) requestRedemption(c *gin.Context) {
	inv := currentInvestor(c)
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	var req struct {
		Units             float64 `json:"units" binding:"required,gt=0"`
		PayoutMethod      string  `json:"payoutMethod" binding:"required"`
		Phone             string  `json:"phone"`
		PayoutAssetSymbol string  `json:"payoutAssetSymbol"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	method := distributions.PayoutMethod(req.PayoutMethod)
	if method != distributions.PayoutManual && method != distributions.PayoutMpesaB2C && method != distributions.PayoutOnchain {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payoutMethod must be \"manual\", \"mpesa_b2c\", or \"onchain_token\""})
		return
	}
	phone := req.Phone
	if method == distributions.PayoutMpesaB2C && phone == "" {
		phone = inv.PayoutPhone
	}
	if method == distributions.PayoutMpesaB2C && phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a phone number is required for the mpesa_b2c rail — set one on your profile or pass it with this request"})
		return
	}
	if method == distributions.PayoutOnchain && req.PayoutAssetSymbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payoutAssetSymbol is required for the onchain_token rail"})
		return
	}

	held, err := s.ce.AssetBalance(c.Request.Context(), o.Symbol, o.Chain, inv.ExternalUserID())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check current holding", "details": err.Error()})
		return
	}
	if req.Units > held {
		c.JSON(http.StatusBadRequest, gin.H{"error": "units exceed your current holding", "held": held})
		return
	}

	r, err := redemptions.Request(c.Request.Context(), o.ID, inv.ID, req.Units, o.PricePerUnit, o.Currency, method, phone, req.PayoutAssetSymbol)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"redemption": r})
}

// myRedemptions handles GET /api/v1/me/redemptions
func (s *Server) myRedemptions(c *gin.Context) {
	inv := currentInvestor(c)
	list, err := redemptions.ListByInvestor(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"redemptions": list, "count": len(list)})
}

// ── admin ────────────────────────────────────────────────────────────────

// listRedemptions handles GET /api/v1/admin/redemptions?status=pending
func (s *Server) listRedemptions(c *gin.Context) {
	list, err := redemptions.List(c.Request.Context(), c.Query("status"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"redemptions": list, "count": len(list)})
}

// approveRedemption handles POST /api/v1/admin/redemptions/:id/approve —
// burns the tokens on-chain via comet-engine (the real "claim the
// underlying asset back" step), then attempts settlement for whichever
// rail the request specified; the manual rail is left for a follow-up
// mark-paid call.
func (s *Server) approveRedemption(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid redemption id"})
		return
	}
	r, err := redemptions.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if r.Status != redemptions.StatusPending {
		c.JSON(http.StatusConflict, gin.H{"error": "redemption is not pending review"})
		return
	}
	o, err := offerings.Get(c.Request.Context(), r.OfferingID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "offering not found for this redemption"})
		return
	}
	inv, err := investors.GetByID(c.Request.Context(), r.InvestorID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "investor not found for this redemption"})
		return
	}

	ctx := c.Request.Context()

	// The burn is signed by the investor's own custodial wallet, so it can
	// only ever succeed up to what that wallet actually holds on-chain —
	// re-check now rather than let comet-engine/Stellar reject it with an
	// opaque error. This can legitimately go stale between request and
	// approval (a second redemption already spent the balance, a transfer
	// happened, or — in a reset dev/test environment — the holding was
	// never really minted on-chain even though this record says so).
	held, err := s.ce.AssetBalance(ctx, o.Symbol, o.Chain, inv.ExternalUserID())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to check investor's on-chain balance", "details": err.Error()})
		return
	}
	if held < r.Units {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf(
				"investor only holds %g units of %s on-chain, but this redemption is for %g — reject this request or have them re-request for the amount they actually hold",
				held, o.Symbol, r.Units,
			),
			"heldUnits": held, "requestedUnits": r.Units,
		})
		return
	}

	result, err := s.ce.Redeem(ctx, o.ReceiptID, inv.ExternalUserID(), r.ID.Hex(), r.Units)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "comet-engine redeem failed", "details": err.Error()})
		return
	}
	if err := redemptions.MarkBurned(ctx, id, result.TxHash); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, redemptions.ErrNotPending) {
			status = http.StatusConflict
		}
		// Tokens are already burned on-chain at this point — same
		// needs-manual-reconciliation gap the investment-confirmation flow is
		// already honest about.
		c.JSON(status, gin.H{"error": fmt.Sprintf("burned on-chain (tx %s) but failed to update the redemption record: %v", result.TxHash, err)})
		return
	}

	switch r.PayoutMethod {
	case distributions.PayoutOnchain:
		payouts := []cometengine.Payout{{ExternalUserID: inv.ExternalUserID(), Amount: r.PayoutAmount}}
		payoutResult, err := s.ce.Distribute(ctx, o.ReceiptID, r.PayoutAssetSymbol, r.ID.Hex(), "capital_return", payouts) // paying an investor back for units they redeemed
		if err != nil {
			_ = redemptions.MarkFailed(ctx, id, err.Error())
		} else if len(payoutResult.Results) > 0 && payoutResult.Results[0].Error == "" {
			_ = redemptions.MarkPaid(ctx, id, payoutResult.Results[0].TxHash)
		} else if len(payoutResult.Results) > 0 {
			_ = redemptions.MarkFailed(ctx, id, payoutResult.Results[0].Error)
		}
	case distributions.PayoutMpesaB2C:
		kesAmount, kesErr := amountInKES(r.PayoutAmount, r.Currency, s.cfg.USDKESRate)
		available, err := s.ce.TreasuryBalance(ctx)
		if kesErr != nil {
			_ = redemptions.MarkFailed(ctx, id, kesErr.Error())
		} else if err != nil || available < kesAmount {
			_ = redemptions.MarkFailed(ctx, id, "comet-engine treasury balance too low to pay this out automatically — settle manually or fund the treasury")
		} else {
			b2c, err := s.ce.B2CTransfer(ctx, inv.ExternalUserID(), r.Phone, "M-Pesa", "redeem-"+r.ID.Hex(), kesAmount)
			if err != nil {
				_ = redemptions.MarkFailed(ctx, id, err.Error())
			} else {
				// Async — comet-engine resolves the final PSP outcome on its own
				// side; record the initiation reference, reconcile manually.
				_ = redemptions.MarkPaid(ctx, id, b2c.TransactionID)
			}
		}
	case distributions.PayoutManual:
		// Left approved_burned — an admin settles with mark-paid.
	}

	updated, _ := redemptions.Get(ctx, id)
	c.JSON(http.StatusOK, gin.H{"redemption": updated})
}

func (s *Server) rejectRedemption(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid redemption id"})
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := redemptions.Reject(c.Request.Context(), id, req.Reason); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, redemptions.ErrNotPending) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// markRedemptionPaid handles POST /api/v1/admin/redemptions/:id/mark-paid
// — the manual rail's settlement confirmation (also usable to close out a
// failed mpesa_b2c attempt once paid by another means).
func (s *Server) markRedemptionPaid(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid redemption id"})
		return
	}
	var req struct {
		PaymentRef string `json:"paymentRef" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := redemptions.MarkPaid(c.Request.Context(), id, req.PaymentRef); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, redemptions.ErrNotApprovedYet) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	updated, _ := redemptions.Get(c.Request.Context(), id)
	c.JSON(http.StatusOK, gin.H{"redemption": updated})
}
