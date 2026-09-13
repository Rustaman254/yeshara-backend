// Package api is Yeshara's HTTP surface: investor auth, KYC submission,
// the primary-market offering list, investing, and the issuer/admin side
// that creates and approves offerings.
package api

import (
	"net/http"

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
		v1.POST("/auth/login", s.login)

		v1.POST("/admin/login", s.adminLogin)

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
			admin.POST("/offerings/:offeringId/approve", s.approveOffering)
			admin.POST("/offerings/:offeringId/reject", s.rejectOffering)
			admin.PATCH("/offerings/:offeringId/details", s.updateOfferingDetails)
			admin.POST("/offerings/:offeringId/revalue", s.revalueOffering)
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
		}

		owners := v1.Group("/owners")
		{
			owners.POST("/auth/register", s.ownerRegister)
			owners.POST("/auth/login", s.ownerLogin)

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

func cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Admin-Token")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
