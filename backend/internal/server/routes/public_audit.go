package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// RegisterPublicAuditRoutes registers the public audit export API endpoints.
// These endpoints use their own bearer-token auth middleware (not admin auth).
func RegisterPublicAuditRoutes(r *gin.Engine, h *handler.AuditExportHandler, conv *handler.AuditConversationHandler, mw gin.HandlerFunc) {
	g := r.Group("/api/v1/audit", mw)
	g.POST("/auth/verify", h.VerifyAuth)
	g.GET("/exports/payloads", h.ListPayloads)

	// Blob proxy must be registered BEFORE :id to avoid gin's param-capture
	// matching "blobs" as an id value.
	g.GET("/exports/payloads/blobs/:sha", conv.GetBlob)

	g.GET("/exports/payloads/:id", h.GetPayload)
	g.GET("/exports/payloads.ndjson", h.StreamNDJSON)

	// Conversation export.
	g.GET("/exports/payloads/:id/conversation", conv.GetConversation)
}
