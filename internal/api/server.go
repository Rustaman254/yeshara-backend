// Package api is Yeshara's HTTP surface: investor auth, KYC submission,
// the primary-market offering list, investing, and the issuer/admin side
// that creates and approves offerings.
package api

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"yeshara.network/marketplace/internal/cometengine"
	"yeshara.network/marketplace/internal/config"
)

type Server struct {
	cfg *config.Config
	ce  *cometengine.Client
}

func NewServer(cfg *config.Config) *Server {
	return &Server{
		cfg: cfg,
		ce:  cometengine.New(cfg.CometEngineBaseURL, cfg.CometAPIKey, cfg.CometAPISecret),
	}
}

func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), cors())

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "yeshara-marketplace"}) })

	v1 := r.Group("/api/v1")
	{
		v1.POST("/auth/register", s.register)
		v1.POST("/auth/login", loginRateLimit(), s.login)

		v1.POST("/admin/login", loginRateLimit(), s.adminLogin)

		v1.GET("/offerings", s.listLiveOfferings)
		v1.GET("/offerings/:offeringId", s.getOffering)

		// Mamlaka PSP calling back with an STK push result — can't
		// authenticate as an investor, so it's verified by HMAC signature
		// instead (see mobileMoneyCallback).
		v1.POST("/payments/mobile-money/callback", s.mobileMoneyCallback)

		authed := v1.Group("")
		authed.Use(s.requireInvestor)
		{
			authed.POST("/auth/logout", s.logout)
			authed.GET("/me", s.me)
			authed.POST("/kyc/submit", s.submitKYC)
			authed.GET("/kyc/status", s.kycStatus)
			authed.GET("/kyc/me", s.myKYC)
			authed.PATCH("/kyc/address", s.updateAddress)
			authed.POST("/offerings/:offeringId/invest", s.invest)
			authed.POST("/offerings/:offeringId/invest/mobile-money", s.investMobileMoney)
			authed.GET("/me/investments", s.myInvestments)
			authed.GET("/me/investments/:id", s.myInvestmentStatus)
			authed.GET("/me/portfolio", s.myPortfolio)
			authed.PATCH("/me/payout-phone", s.setPayoutPhone)
			authed.GET("/me/yield", s.myYield)
			authed.POST("/offerings/:offeringId/redemptions", s.requestRedemption)
			authed.GET("/me/redemptions", s.myRedemptions)
		}

		admin := v1.Group("/admin")
		admin.Use(s.requireAdmin)
		{
			admin.POST("/offerings", s.createOffering)
			admin.GET("/offerings", s.listAllOfferings)
			admin.PATCH("/offerings/:offeringId/details", s.updateOfferingDetails)
			admin.POST("/offerings/:offeringId/revalue", s.revalueOffering)
			admin.POST("/offerings/:offeringId/delist", s.delistOffering)
			admin.POST("/offerings/:offeringId/relist", s.relistOffering)
			admin.GET("/offerings/:offeringId/investments", s.listOfferingInvestments)
			admin.POST("/investments/:id/confirm", s.confirmInvestment)
			admin.GET("/kyc/pending", s.listPendingKYC)
			admin.POST("/kyc/:investorId/decide", s.decideKYC)
			admin.POST("/offerings/:offeringId/distributions", s.triggerDistribution)
			admin.GET("/offerings/:offeringId/distributions", s.listOfferingDistributions)
			admin.GET("/distributions/:id/entries", s.listDistributionEntries)
			admin.POST("/distributions/entries/:entryId/mark-paid", s.markDistributionEntryPaid)
			admin.GET("/redemptions", s.listRedemptions)
			admin.POST("/redemptions/:id/approve", s.approveRedemption)
			admin.POST("/redemptions/:id/reject", s.rejectRedemption)
			admin.POST("/redemptions/:id/mark-paid", s.markRedemptionPaid)
			admin.PATCH("/offerings/:offeringId/owner-account", s.setOfferingOwnerAccount)

			// Provisioning the four approval-chain seats — see
			// internal/staff. Not self-service; an admin assigns them.
			admin.POST("/staff", s.createStaffAccount)
			admin.GET("/staff", s.listStaffAccounts)
			admin.DELETE("/staff/:id", s.deleteStaffAccount)
		}

		// The Manager A/B -> Trustee A/B approval chain — four distinct,
		// separately-authenticated logins (internal/staff), not four
		// permission flags on the one shared admin secret above. See
		// staff_handlers.go's staffReview for how segregation of duties is
		// actually enforced (dispatch on the session's own role, not on
		// anything the client sends).
		staffLogin := v1.Group("/staff")
		{
			staffLogin.POST("/auth/login", loginRateLimit(), s.staffLogin)

			staffAuthed := staffLogin.Group("")
			staffAuthed.Use(s.requireAnyStaff)
			{
				staffAuthed.POST("/auth/logout", s.staffLogout)
				staffAuthed.GET("/me", s.staffMe)
				staffAuthed.GET("/offerings", s.staffQueue)
				staffAuthed.POST("/offerings/:offeringId/review", s.staffReview)
				staffAuthed.POST("/offerings/:offeringId/reject", s.staffReject)
			}
		}

		owners := v1.Group("/owners")
		{
			owners.POST("/auth/register", s.ownerRegister)
			owners.POST("/auth/login", loginRateLimit(), s.ownerLogin)

			ownerAuthed := owners.Group("")
			ownerAuthed.Use(s.requireOwner)
			{
				ownerAuthed.POST("/auth/logout", s.ownerLogout)
				ownerAuthed.GET("/me", s.ownerMe)
				ownerAuthed.GET("/me/offerings", s.ownerMyOfferings)
			}
		}
	}

	return r
}

// defaultAllowedOrigins are known-good frontend origins allowed out of the
// box, with no Render/hosting-dashboard configuration required — the
// deployed marketplace frontend, specifically. CORS_ALLOWED_ORIGINS below
// is additive on top of this list (a custom domain, a staging frontend,
// etc.), not a replacement for it.
var defaultAllowedOrigins = map[string]bool{
	"https://yeshara-marketplace.vercel.app": true,
}

// cors mirrors comet-engine's internal/api/middleware.go: an explicit
// origin allowlist (CORS_ALLOWED_ORIGINS, comma-separated) rather than a
// bare "*". Low severity on its own here since auth is bearer-token/header
// based, not cookies, but a wildcard origin is still unnecessary exposure
// with no upside — an allowlist costs nothing once the frontend's real
// origin is known. Falls back to permissive localhost matching only when
// CORS_ALLOW_DEV_ORIGINS=true, same dev/prod split as comet-engine.
func cors() gin.HandlerFunc {
	allowed := os.Getenv("CORS_ALLOWED_ORIGINS")
	allowDevOrigins := os.Getenv("CORS_ALLOW_DEV_ORIGINS") == "true"
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		switch {
		case origin == "":
			// Not a browser CORS request.
		case defaultAllowedOrigins[origin]:
			c.Header("Access-Control-Allow-Origin", origin)
		case allowed == "" && allowDevOrigins:
			if origin == "http://localhost" || origin == "https://localhost" ||
				origin == "http://127.0.0.1" || origin == "https://127.0.0.1" ||
				strings.HasPrefix(origin, "http://localhost:") ||
				strings.HasPrefix(origin, "https://localhost:") ||
				strings.HasPrefix(origin, "http://127.0.0.1:") ||
				strings.HasPrefix(origin, "https://127.0.0.1:") {
				c.Header("Access-Control-Allow-Origin", origin)
			}
		case allowed == "":
			// No allowlist configured and dev origins not opted into —
			// same as comet-engine's production default: no CORS headers
			// at all rather than a permissive fallback.
		default:
			for _, a := range strings.Split(allowed, ",") {
				if strings.TrimSpace(a) == origin {
					c.Header("Access-Control-Allow-Origin", origin)
					break
				}
			}
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Admin-Token")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
