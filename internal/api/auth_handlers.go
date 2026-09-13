package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"yeshara.network/marketplace/internal/investors"
	"yeshara.network/marketplace/internal/kyc"
)

func (s *Server) register(c *gin.Context) {
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
	inv, err := investors.Register(c.Request.Context(), email, req.Password, req.FullName)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, investors.ErrEmailTaken) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	// Ensure the investor's comet-engine wallet exists right away — the
	// same "provision on first use" pattern comet-engine's own wallet
	// endpoints use, just triggered at registration instead of first
	// investment so it's ready the moment KYC clears.
	if _, err := s.ce.EnsureWallet(c.Request.Context(), inv.ExternalUserID(), "stellar"); err != nil {
		// Non-fatal — the wallet will be created lazily on first investment
		// if this fails (EnsureWallet is idempotent either way).
	}

	_, token, err := investors.Login(c.Request.Context(), email, req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registered but failed to create session, please log in"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"investor": inv, "token": token})
}

func (s *Server) login(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	inv, token, err := investors.Login(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)), req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"investor": inv, "token": token})
}

func (s *Server) logout(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 {
		_ = investors.Logout(c.Request.Context(), parts[1])
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) me(c *gin.Context) {
	inv := currentInvestor(c)
	status, _ := kyc.GetStatus(c.Request.Context(), inv.ID)

	resp := gin.H{
		"investor":       inv,
		"kycStatus":      status,
		"externalUserId": inv.ExternalUserID(),
	}
	// Best-effort — Yeshara is Stellar-only for now (see offerings.CreateInput's
	// Chain comment); a wallet is provisioned at registration, but this stays
	// non-fatal in case that failed or comet-engine is briefly unavailable.
	if addr, err := s.ce.EnsureWallet(c.Request.Context(), inv.ExternalUserID(), "stellar"); err == nil {
		resp["stellarAddress"] = addr
	}
	c.JSON(http.StatusOK, resp)
}
