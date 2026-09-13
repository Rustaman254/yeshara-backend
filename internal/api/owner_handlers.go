package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"yeshara.network/marketplace/internal/offerings"
	"yeshara.network/marketplace/internal/owners"
)

// ownerRegister handles POST /api/v1/owners/auth/register — self-service
// signup, structurally identical to investor registration. Registering
// alone grants no visibility into anything: an admin has to separately
// link the account to a specific offering (setOfferingOwnerAccount) after
// verifying off-platform that this person actually owns that asset.
func (s *Server) ownerRegister(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=8"`
		FullName string `json:"fullName" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	o, err := owners.Register(c.Request.Context(), email, req.Password, req.FullName)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, owners.ErrEmailTaken) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	_, token, err := owners.Login(c.Request.Context(), email, req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registered but failed to create session, please log in"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"owner": o, "token": token})
}

func (s *Server) ownerLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	o, token, err := owners.Login(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)), req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"owner": o, "token": token})
}

func (s *Server) ownerLogout(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 {
		_ = owners.Logout(c.Request.Context(), parts[1])
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) ownerMe(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"owner": currentOwner(c)})
}

// ownerMyOfferings handles GET /api/v1/owners/me/offerings — every
// offering linked to this owner, each annotated with the live on-chain
// balance of their reserved stake (same AssetBalance call myPortfolio uses
// for investor holdings), its current value at the offering's price, and a
// KES-equivalent for consistency with the rest of the app.
func (s *Server) ownerMyOfferings(c *gin.Context) {
	o := currentOwner(c)
	list, err := offerings.ListByOwnerAccount(c.Request.Context(), o.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type ownerOffering struct {
		OfferingID    string   `json:"offeringId"`
		Title         string   `json:"title"`
		Symbol        string   `json:"symbol"`
		ReservedUnits float64  `json:"reservedUnits"`
		PricePerUnit  float64  `json:"pricePerUnit"`
		Value         float64  `json:"value"`
		Currency      string   `json:"currency"`
		KesValue      *float64 `json:"kesValue,omitempty"`
	}
	out := make([]ownerOffering, 0, len(list))
	for _, off := range list {
		if off.OwnerUserID == "" {
			continue
		}
		units, err := s.ce.AssetBalance(c.Request.Context(), off.Symbol, off.Chain, off.OwnerUserID)
		if err != nil {
			continue
		}
		value := units * off.PricePerUnit
		oo := ownerOffering{
			OfferingID: off.OfferingID, Title: off.Title, Symbol: off.Symbol,
			ReservedUnits: units, PricePerUnit: off.PricePerUnit, Value: value, Currency: off.Currency,
		}
		if kesValue, err := amountInKES(value, off.Currency, s.cfg.USDKESRate); err == nil {
			oo.KesValue = &kesValue
		}
		out = append(out, oo)
	}
	c.JSON(http.StatusOK, gin.H{"offerings": out, "count": len(out)})
}

// ── admin ────────────────────────────────────────────────────────────────

// setOfferingOwnerAccount handles
// PATCH /api/v1/admin/offerings/:offeringId/owner-account — links an
// offering to a real owners.Owner login by email. The owner must already
// be registered (no silent auto-creation) — same verify-then-link posture
// as KYC approval; the admin is vouching that this real person owns this
// real asset, which the system can't verify itself.
func (s *Server) setOfferingOwnerAccount(c *gin.Context) {
	off, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	owner, err := owners.GetByEmail(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil {
		if errors.Is(err, owners.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "owner account not found — they need to register at /owner/sign-up first"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	updated, err := offerings.SetOwnerAccount(c.Request.Context(), off.ID, owner.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offering": updated})
}
