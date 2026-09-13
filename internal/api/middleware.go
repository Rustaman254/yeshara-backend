package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"yeshara.network/marketplace/internal/investors"
	"yeshara.network/marketplace/internal/owners"
)

const investorContextKey = "investor"
const ownerContextKey = "owner"

// requireInvestor validates an Authorization: Bearer <session> header and
// puts the investor in gin's context for handlers to read via
// currentInvestor(c).
func (s *Server) requireInvestor(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return
	}
	inv, err := investors.ValidateSession(c.Request.Context(), parts[1])
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}
	c.Set(investorContextKey, inv)
	c.Next()
}

func currentInvestor(c *gin.Context) *investors.Investor {
	v, _ := c.Get(investorContextKey)
	inv, _ := v.(*investors.Investor)
	return inv
}

// requireOwner validates an Authorization: Bearer <session> header against
// the owners package and puts the owner in gin's context for handlers to
// read via currentOwner(c) — same shape as requireInvestor, separate
// account namespace.
func (s *Server) requireOwner(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return
	}
	o, err := owners.ValidateSession(c.Request.Context(), parts[1])
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}
	c.Set(ownerContextKey, o)
	c.Next()
}

func currentOwner(c *gin.Context) *owners.Owner {
	v, _ := c.Get(ownerContextKey)
	o, _ := v.(*owners.Owner)
	return o
}

// requireAdmin is a deliberately simple shared-secret gate for the
// issuer/admin side — this is an MVP stand-in, not a real admin role
// system with per-user permissions. Fine for one issuer operating its own
// marketplace; a multi-issuer Yeshara would need real admin accounts.
func (s *Server) requireAdmin(c *gin.Context) {
	token := c.GetHeader("X-Admin-Token")
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin token required"})
		return
	}
	c.Next()
}
