package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// adminLogin exchanges the configured admin email/password for the shared
// AdminToken — a friendlier login form in front of the same single-secret
// gate requireAdmin checks, not real per-user admin accounts.
func (s *Server) adminLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email != strings.ToLower(s.cfg.AdminEmail) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(s.cfg.AdminPasswordHash), []byte(req.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"token": s.cfg.AdminToken})
}
