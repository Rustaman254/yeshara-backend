package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"yeshara.network/marketplace/internal/offerings"
	"yeshara.network/marketplace/internal/staff"
)

// ── auth ─────────────────────────────────────────────────────────────────

func (s *Server) staffLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	st, token, err := staff.Login(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)), req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "staff": st})
}

func (s *Server) staffLogout(c *gin.Context) {
	token := bearerToken(c)
	_ = staff.Logout(c.Request.Context(), token)
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) staffMe(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"staff": currentStaff(c)})
}

func bearerToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	return ""
}

// ── admin: provisioning the four seats ──────────────────────────────────

// createStaffAccount handles POST /api/v1/admin/staff — admin-only, since
// these are named operational roles a real institution assigns, not
// something anyone self-registers into (see internal/staff's doc comment).
func (s *Server) createStaffAccount(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
		FullName string `json:"fullName" binding:"required"`
		Role     string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	st, err := staff.Create(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)), req.Password, req.FullName, staff.Role(req.Role))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, staff.ErrEmailTaken) || errors.Is(err, staff.ErrInvalidRole) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"staff": st})
}

func (s *Server) listStaffAccounts(c *gin.Context) {
	list, err := staff.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"staff": list, "count": len(list)})
}

// deleteStaffAccount handles DELETE /api/v1/admin/staff/:id — offboarding.
func (s *Server) deleteStaffAccount(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid staff id"})
		return
	}
	if err := staff.Delete(c.Request.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, staff.ErrNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// ── the approval chain itself ───────────────────────────────────────────

// roleQueueStatus maps a seat to the offering status it should see in its
// queue — the mirror image of the Status -> next-stage transitions in
// internal/offerings.
var roleQueueStatus = map[staff.Role]offerings.Status{
	staff.RoleManagerA: offerings.StatusPendingManagerA,
	staff.RoleManagerB: offerings.StatusPendingManagerB,
	staff.RoleTrusteeA: offerings.StatusPendingTrusteeA,
	staff.RoleTrusteeB: offerings.StatusPendingTrusteeB,
}

// staffQueue handles GET /api/v1/staff/offerings — whatever's sitting on
// the authenticated seat's own desk, so a client never has to know the
// status vocabulary itself.
func (s *Server) staffQueue(c *gin.Context) {
	st := currentStaff(c)
	status, ok := roleQueueStatus[st.Role]
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unrecognized staff role"})
		return
	}
	list, err := offerings.List(c.Request.Context(), string(status))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offerings": list, "count": len(list)})
}

// staffReview handles POST /api/v1/staff/offerings/:offeringId/review — the
// one action every seat takes to clear its stage and forward the offering.
// It dispatches on the AUTHENTICATED session's role, never on anything the
// client sends — this is the segregation-of-duties enforcement point: a
// manager_b session can only ever reach offerings.ManagerBReview here, full
// stop, regardless of the offering's actual current stage (which
// ManagerBReview then separately, atomically re-checks).
func (s *Server) staffReview(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	st := currentStaff(c)
	actor := st.Email

	var updated *offerings.Offering
	var txHash, txURL string
	var reviewErr error

	switch st.Role {
	case staff.RoleManagerA:
		updated, reviewErr = offerings.ManagerAReview(c.Request.Context(), o.ID, actor)
	case staff.RoleManagerB:
		updated, reviewErr = offerings.ManagerBReview(c.Request.Context(), o.ID, actor)
	case staff.RoleTrusteeA:
		updated, reviewErr = offerings.TrusteeAApprove(c.Request.Context(), o.ID, actor)
	case staff.RoleTrusteeB:
		updated, txHash, reviewErr = offerings.TrusteeBTokenize(c.Request.Context(), s.ce, o.ID, actor)
		if updated != nil {
			txURL = updated.MintTxURL
		}
	default:
		c.JSON(http.StatusForbidden, gin.H{"error": "unrecognized staff role"})
		return
	}
	if reviewErr != nil {
		status := http.StatusInternalServerError
		if errors.Is(reviewErr, offerings.ErrWrongStage) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": reviewErr.Error()})
		return
	}
	resp := gin.H{"offering": updated}
	if txHash != "" {
		resp["mintTxHash"] = txHash
		resp["mintTxUrl"] = txURL
	}
	c.JSON(http.StatusOK, resp)
}

// staffReject handles POST /api/v1/staff/offerings/:offeringId/reject —
// available to any seat, from whatever stage the offering is currently
// sitting at (offerings.Reject itself checks that).
func (s *Server) staffReject(c *gin.Context) {
	o, err := offerings.GetByOfferingID(c.Request.Context(), c.Param("offeringId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	st := currentStaff(c)
	if err := offerings.Reject(c.Request.Context(), s.ce, o.ID, st.Email, string(st.Role), req.Reason); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, offerings.ErrWrongStage) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}
