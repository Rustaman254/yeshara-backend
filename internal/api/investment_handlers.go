package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"yeshara.network/marketplace/internal/investments"
	"yeshara.network/marketplace/internal/investors"
	"yeshara.network/marketplace/internal/kyc"
	"yeshara.network/marketplace/internal/offerings"
	"yeshara.network/marketplace/internal/payments"
)

// invest handles POST /api/v1/offerings/:offeringId/invest — places an
// order. KYC-gated. Does not move any tokens yet: see confirmInvestment.
func (s *Server) invest(c *gin.Context) {
	inv := currentInvestor(c)

	status, err := kyc.GetStatus(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if status != kyc.StatusVerified {
		c.JSON(http.StatusForbidden, gin.H{"error": "KYC verification required before investing", "kycStatus": status})
		return
	}

	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if o.Status != offerings.StatusLive {
		c.JSON(http.StatusConflict, gin.H{"error": "offering is not open for investment"})
		return
	}

	var req struct {
		Units float64 `json:"units" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	available, err := s.ce.AssetBalance(c.Request.Context(), o.Symbol, o.Chain, s.cfg.TreasuryUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check available units", "details": err.Error()})
		return
	}
	if req.Units > available {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not enough units available", "available": available})
		return
	}

	order, err := investments.PlaceOrder(c.Request.Context(), o.ID, inv.ID, req.Units, o.PricePerUnit, o.MinInvestmentUnits, o.Currency)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, investments.ErrBelowMinimum) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"investment": order,
		"message":    "Order placed. Wire " + order.Currency + " to the reference below; units transfer once payment is confirmed.",
	})
}

// investMobileMoney handles POST /api/v1/offerings/:offeringId/invest/mobile-money
// — places an order exactly like invest, then immediately requests an STK
// push for it. The order is minted the moment Mamlaka's callback reports
// the charge COMPLETE (see mobileMoneyCallback), not here — this call only
// confirms the prompt reached the investor's phone.
func (s *Server) investMobileMoney(c *gin.Context) {
	inv := currentInvestor(c)

	status, err := kyc.GetStatus(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if status != kyc.StatusVerified {
		c.JSON(http.StatusForbidden, gin.H{"error": "KYC verification required before investing", "kycStatus": status})
		return
	}

	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if o.Status != offerings.StatusLive {
		c.JSON(http.StatusConflict, gin.H{"error": "offering is not open for investment"})
		return
	}

	var req struct {
		Units    float64 `json:"units" binding:"required"`
		Provider string  `json:"provider" binding:"required"` // "mpesa" | "airtel"
		Phone    string  `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	provider := payments.Provider(req.Provider)
	if provider != payments.ProviderMPesa && provider != payments.ProviderAirtel {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be \"mpesa\" or \"airtel\""})
		return
	}

	available, err := s.ce.AssetBalance(c.Request.Context(), o.Symbol, o.Chain, s.cfg.TreasuryUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check available units", "details": err.Error()})
		return
	}
	if req.Units > available {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not enough units available", "available": available})
		return
	}

	kesAmount, err := amountInKES(o.PricePerUnit*req.Units, o.Currency, s.cfg.USDKESRate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Mobile money doesn't do fractional shillings, and the USD->KES
	// conversion above is prone to float noise (e.g. 64500.00000000001) that
	// Mamlaka's API rejects outright — round to a whole KES amount, which is
	// also the number actually shown to the investor before they pay.
	kesAmount = math.Round(kesAmount)

	order, err := investments.PlaceOrder(c.Request.Context(), o.ID, inv.ID, req.Units, o.PricePerUnit, o.MinInvestmentUnits, o.Currency)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, investments.ErrBelowMinimum) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	pc := payments.New(s.cfg.PaymentsBaseURL, s.cfg.PaymentsMerchantID)
	result, err := pc.InitiateSTK(provider, "Yeshara", req.Phone, kesAmount, order.PaymentRef, s.cfg.PaymentsCallbackURL)
	if err != nil {
		_ = investments.Fail(c.Request.Context(), order.ID, err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to initiate mobile money payment", "details": err.Error()})
		return
	}
	if err := investments.AttachPayment(c.Request.Context(), order.ID, req.Provider, req.Phone, result.SecureID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	order.PaymentProvider, order.Phone, order.SecureID = req.Provider, req.Phone, result.SecureID

	c.JSON(http.StatusCreated, gin.H{
		"investment": order,
		"kesAmount":  kesAmount,
		"message":    "Payment request sent — approve it on your phone to complete the purchase.",
	})
}

// amountInKES converts an offering's amount due into KES for mobile-money
// checkout. Yeshara has no live FX feed, so non-KES offerings use a static,
// manually-configured rate (config.USDKESRate) — currently USD-only.
func amountInKES(amount float64, currency string, usdKesRate float64) (float64, error) {
	switch currency {
	case "KES":
		return amount, nil
	case "USD":
		// Rounded to cents — plain multiplication leaves float noise (e.g.
		// 64500.00000000001) that's harmless for display but gets rejected
		// outright by Mamlaka's API when used as a literal charge amount.
		return math.Round(amount*usdKesRate*100) / 100, nil
	default:
		return 0, fmt.Errorf("mobile money checkout isn't available for offerings priced in %s", currency)
	}
}

// myInvestmentStatus handles GET /api/v1/me/investments/:id — lets the
// investor poll a single order's status after initiating mobile-money
// checkout, without waiting on a page refresh for the callback to land.
func (s *Server) myInvestmentStatus(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid investment id"})
		return
	}
	order, err := investments.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if order.InvestorID != currentInvestor(c).ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"investment": order})
}

func (s *Server) myInvestments(c *gin.Context) {
	inv := currentInvestor(c)
	list, err := investments.ListByInvestor(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"investments": list, "count": len(list)})
}

// myPortfolio handles GET /api/v1/me/portfolio — current on-chain holdings
// across every offering the investor has a completed investment in, valued
// at each offering's current price per unit.
func (s *Server) myPortfolio(c *gin.Context) {
	inv := currentInvestor(c)
	list, err := investments.ListByInvestor(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	seen := map[string]bool{}
	type holding struct {
		OfferingID   string   `json:"offeringId"`
		Title        string   `json:"title"`
		Symbol       string   `json:"symbol"`
		Units        float64  `json:"units"`
		PricePerUnit float64  `json:"pricePerUnit"`
		Value        float64  `json:"value"`
		Currency     string   `json:"currency"`
		KesValue     *float64 `json:"kesValue,omitempty"` // value converted to KES — the currency M-Pesa payouts/cash-outs actually move in
	}
	holdings := []holding{}
	var totalValue, totalKesValue float64
	for _, inv := range list {
		if inv.Status != investments.StatusCompleted || seen[inv.OfferingID.Hex()] {
			continue
		}
		seen[inv.OfferingID.Hex()] = true
		o, err := offerings.Get(c.Request.Context(), inv.OfferingID)
		if err != nil {
			continue
		}
		units, err := s.ce.AssetBalance(c.Request.Context(), o.Symbol, o.Chain, investorExternalUserID(c))
		if err != nil {
			continue
		}
		value := units * o.PricePerUnit
		totalValue += value
		h := holding{
			OfferingID: o.OfferingID, Title: o.Title, Symbol: o.Symbol,
			Units: units, PricePerUnit: o.PricePerUnit, Value: value, Currency: o.Currency,
		}
		if kesValue, err := amountInKES(value, o.Currency, s.cfg.USDKESRate); err == nil {
			h.KesValue = &kesValue
			totalKesValue += kesValue
		}
		holdings = append(holdings, h)
	}
	c.JSON(http.StatusOK, gin.H{"holdings": holdings, "totalValue": totalValue, "totalKesValue": totalKesValue})
}

func investorExternalUserID(c *gin.Context) string {
	return currentInvestor(c).ExternalUserID()
}

// ── admin ────────────────────────────────────────────────────────────────

func (s *Server) listOfferingInvestments(c *gin.Context) {
	offeringOID, _, err := s.offeringObjectID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	list, err := investments.ListByOffering(c.Request.Context(), offeringOID, c.Query("status"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"investments": list, "count": len(list)})
}

// confirmInvestment handles POST /api/v1/admin/investments/:id/confirm —
// the operator action ("payment received") that actually triggers the
// on-chain unit transfer from the issuer's treasury to the investor.
func (s *Server) confirmInvestment(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid investment id"})
		return
	}
	order, err := investments.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	o, err := offerings.Get(c.Request.Context(), order.OfferingID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "offering not found for this investment"})
		return
	}
	inv, err := investors.GetByID(c.Request.Context(), order.InvestorID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "investor not found for this investment"})
		return
	}

	updated, err := investments.ConfirmPayment(c.Request.Context(), s.ce, id, o.Symbol, o.Chain, s.cfg.TreasuryUserID, inv.ExternalUserID())
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, investments.ErrNotPending) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"investment": updated})
}
