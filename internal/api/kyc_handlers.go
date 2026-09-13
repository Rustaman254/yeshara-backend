package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"yeshara.network/marketplace/internal/kyc"
)

func (s *Server) submitKYC(c *gin.Context) {
	inv := currentInvestor(c)
	var req struct {
		FullName    string `json:"fullName" binding:"required"`
		Country     string `json:"country" binding:"required"`
		Address     string `json:"address"`
		IDNumber    string `json:"idNumber" binding:"required"`
		DateOfBirth string `json:"dateOfBirth" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rec, err := kyc.Submit(c.Request.Context(), inv.ID, req.FullName, req.Country, req.Address, req.IDNumber, req.DateOfBirth)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "kyc": rec})
}

func (s *Server) kycStatus(c *gin.Context) {
	inv := currentInvestor(c)
	status, err := kyc.GetStatus(c.Request.Context(), inv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

// updateAddress handles PATCH /api/v1/kyc/address — lets an investor add or
// change their residential address independent of the verification flow, so
// it isn't stuck unset for accounts verified before this field existed (or
// simply out of date).
func (s *Server) updateAddress(c *gin.Context) {
	inv := currentInvestor(c)
	var req struct {
		Address string `json:"address" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := kyc.UpdateAddress(c.Request.Context(), inv.ID, req.Address); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// myKYC handles GET /api/v1/kyc/me — the investor's own submitted identity
// details (full name, country, address, ...), used by the profile page and
// profile dropdown. Unlike the admin-only kyc.Get, a not-yet-submitted
// record isn't an error here — it just means an empty, unverified profile.
func (s *Server) myKYC(c *gin.Context) {
	inv := currentInvestor(c)
	rec, err := kyc.Get(c.Request.Context(), inv.ID)
	if errors.Is(err, kyc.ErrNotSubmitted) {
		c.JSON(http.StatusOK, gin.H{"status": kyc.StatusUnverified})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rec)
}

// ── admin ────────────────────────────────────────────────────────────────

func (s *Server) listPendingKYC(c *gin.Context) {
	list, err := kyc.ListPending(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pending": list, "count": len(list)})
}

func (s *Server) decideKYC(c *gin.Context) {
	investorID, err := primitive.ObjectIDFromHex(c.Param("investorId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid investor id"})
		return
	}
	var req struct {
		Approved  bool   `json:"approved"`
		Reason    string `json:"reason"`
		DecidedBy string `json:"decidedBy" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := kyc.Decide(c.Request.Context(), investorID, req.Approved, req.Reason, req.DecidedBy); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, kyc.ErrNotSubmitted) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}
