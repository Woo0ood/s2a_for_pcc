package routes

import (
	"context"
	"fmt"
	"time"

	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// [s2a fork] The fork disables the upstream admin compliance acknowledgement
// gate (service.GetAdminComplianceStatus reports Required=false), so admin MCP
// calls that upstream's tests expect to stop at 423 instead reach the real
// handler. Route tests use nil handler receivers, so give the dashboard a
// cache-backed service that answers without any repositories.

type mcpForkDashboardCache struct{}

func (mcpForkDashboardCache) GetDashboardStats(context.Context) (string, error) {
	return fmt.Sprintf(`{"stats":{},"updated_at":%d}`, time.Now().Unix()), nil
}

func (mcpForkDashboardCache) SetDashboardStats(context.Context, string, time.Duration) error {
	return nil
}

func (mcpForkDashboardCache) DeleteDashboardStats(context.Context) error { return nil }

func mcpForkDashboardHandler() *adminhandler.DashboardHandler {
	return adminhandler.NewDashboardHandler(service.NewDashboardService(nil, nil, mcpForkDashboardCache{}, nil), nil)
}
