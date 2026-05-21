package service

import (
	"context"
	"time"
)

// CHPinger abstracts the ClickHouse Ping call for health checks.
type CHPinger interface {
	Ping(ctx context.Context) error
}

// PayloadAuditSchemaEnsurer creates/adjusts the ClickHouse schema at startup.
type PayloadAuditSchemaEnsurer interface {
	EnsureSchema(ctx context.Context, retentionDays int) error
}

// StartCHHealthLoop pings ClickHouse every 10 s and updates the CHUp gauge.
func StartCHHealthLoop(ctx context.Context, p CHPinger, m *PayloadAuditMetrics) {
	if p == nil || m == nil || m.CHUp == nil {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := p.Ping(pctx)
			cancel()
			if err == nil {
				m.CHUp.Set(1)
			} else {
				m.CHUp.Set(0)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
