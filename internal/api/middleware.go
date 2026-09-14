package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"yeshara.network/marketplace/internal/investors"
	"yeshara.network/marketplace/internal/owners"
	"yeshara.network/marketplace/internal/staff"
)

const investorContextKey = "investor"
const ownerContextKey = "owner"
const staffContextKey = "staff"

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

// requireAnyStaff validates an Authorization: Bearer <session> header
// against the staff package — any of the four seats may authenticate this
// way. The actual segregation-of-duties enforcement isn't here: it's that
// every stage-transition handler (staffReview, staffReject in
// offering_handlers.go) switches on currentStaff(c).Role and calls the one
// offerings transition function that matches it. A Manager B session can
// authenticate to /staff/offerings/:id/review same as anyone, but that
// handler only ever calls offerings.ManagerBReview for a manager_b role —
// there is no code path from a manager_b session to
// offerings.TrusteeAApprove, regardless of what the offering's current
// status is (and ManagerBReview itself atomically refuses to fire unless
// the offering is at StatusPendingManagerB, per advanceStage).
func (s *Server) requireAnyStaff(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return
	}
	st, err := staff.ValidateSession(c.Request.Context(), parts[1])
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		return
	}
	c.Set(staffContextKey, st)
	c.Next()
}

func currentStaff(c *gin.Context) *staff.Staff {
	v, _ := c.Get(staffContextKey)
	st, _ := v.(*staff.Staff)
	return st
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

// ── Rate limiting ────────────────────────────────────────────────────────
// Every login endpoint here (investor, owner, staff, admin) was previously
// unthrottled — a bare bcrypt-compare loop reachable at whatever rate a
// client cared to send requests, on accounts including the single shared
// admin login and the four staff seats that can mint real assets. Same
// fixed-window-per-IP shape as comet-engine's loginRateLimit, which this
// mirrors.

type rateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{requests: make(map[string][]time.Time), limit: limit, window: window}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)
	timestamps := rl.requests[key]
	valid := timestamps[:0]
	for _, t := range timestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= rl.limit {
		rl.requests[key] = valid
		return false
	}
	rl.requests[key] = append(valid, now)
	return true
}

// loginRateLimit caps login attempts at 5/minute per source IP — applied
// to every login route (investor, owner, staff, admin) in server.go.
func loginRateLimit() gin.HandlerFunc {
	limiter := newRateLimiter(5, time.Minute)
	return func(c *gin.Context) {
		if !limiter.allow(c.ClientIP()) {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts, please wait and try again"})
			return
		}
		c.Next()
	}
}
