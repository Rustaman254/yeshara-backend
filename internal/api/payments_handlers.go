package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"yeshara.network/marketplace/internal/investments"
	"yeshara.network/marketplace/internal/offerings"
	"yeshara.network/marketplace/internal/payments"
)

// mobileMoneyCallback handles POST /api/v1/payments/mobile-money/callback —
// Mamlaka's PSP posting the final outcome of an STK push here, identified
// by the externalId Yeshara supplied at initiation (an investment's
// PaymentRef). Public: Mamlaka can't authenticate as one of our investors,
// so this is guarded by an HMAC signature instead (when enabled for the
// account) rather than a session/admin token.
func (s *Server) mobileMoneyCallback(c *gin.Context) {
	// Deliberately not c.Request.Context(): the on-chain transfer below can
	// take longer than Mamlaka (or the ngrok tunnel in front of it during
	// local testing) is willing to hold the connection open. If the caller
	// hangs up early, c.Request.Context() cancels mid-transfer — the STK
	// payment already succeeded at that point, so aborting the mint here
	// would leave a paid-but-unfulfilled order. A background context with
	// its own generous timeout keeps this call independent of the caller's
	// patience.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	if !payments.VerifySignature(s.cfg.PaymentsCallbackSecret, raw, c.GetHeader("X-Mamlaka-Signature")) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	var payload payments.CallbackPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	order, err := investments.GetByPaymentRef(ctx, payload.ExternalID)
	if err != nil {
		log.Printf("[mobile-money-callback] unknown externalId=%s", payload.ExternalID)
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown order"})
		return
	}
	if order.Status != investments.StatusPendingPayment {
		// Already settled (or a duplicate delivery) — ack without redoing anything.
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	switch payload.TransactionStatus {
	case "COMPLETE":
		o, err := offerings.Get(ctx, order.OfferingID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "offering not found for this order"})
			return
		}
		investorExternalID := "investor-" + order.InvestorID.Hex()
		if _, err := investments.ConfirmPayment(ctx, s.ce, order.ID, o.Symbol, o.Chain, s.cfg.TreasuryUserID, investorExternalID); err != nil {
			log.Printf("[mobile-money-callback] confirm failed order=%s: %v", order.ID.Hex(), err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if payload.Reference != "" {
			_ = investments.SetProviderReference(ctx, order.ID, payload.Reference)
		}
	case "FAILED":
		reason := payload.TransactionReport
		if reason == "" {
			reason = "mobile money payment failed"
		}
		if err := investments.Fail(ctx, order.ID, reason); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	default:
		// PENDING or anything else — nothing to do yet, just ack.
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
