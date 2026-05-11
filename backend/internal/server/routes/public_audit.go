package routes

import (
	"github.com/Woo0ood/s2a_for_pcc/internal/handler"
	"github.com/gin-gonic/gin"
)

// RegisterPublicAuditRoutes registers the public audit export API endpoints.
// These endpoints use their own bearer-token auth middleware (not admin auth).
func RegisterPublicAuditRoutes(r *gin.Engine, h *handler.AuditExportHandler, mw gin.HandlerFunc) {
	g := r.Group("/api/v1/audit", mw)
	g.POST("/auth/verify", h.VerifyAuth)
	g.GET("/exports/payloads", h.ListPayloads)
	g.GET("/exports/payloads/:id", h.GetPayload)
	g.GET("/exports/payloads.ndjson", h.StreamNDJSON)
}
