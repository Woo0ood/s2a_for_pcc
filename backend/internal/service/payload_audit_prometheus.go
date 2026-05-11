package service

import (
	"github.com/prometheus/client_golang/prometheus"
)

const payloadAuditMetricsNamespace = "payload_audit"

// PayloadAuditMetrics holds Prometheus metric descriptors for the payload audit
// subsystem. Polling-based metrics are collected via a custom Collector that
// reads SinkStats on every scrape. Event-driven metrics (histograms, per-call
// counters) are exposed as regular Prometheus objects for call-site Observe/Inc.
type PayloadAuditMetrics struct {
	// --- event-driven (call-site Observe / Inc) ---

	InsertDuration   prometheus.Histogram // payload_audit_insert_duration_seconds
	InputBytesHist   prometheus.Histogram // payload_audit_input_bytes
	OutputBytesHist  prometheus.Histogram // payload_audit_output_bytes
	TruncatedInput   prometheus.Counter   // payload_audit_truncated_total{which="input"}
	TruncatedOutput  prometheus.Counter   // payload_audit_truncated_total{which="output"}
	RedisDrainOK     prometheus.Counter   // payload_audit_redis_drain_total{result="ok"}
	RedisDrainFail   prometheus.Counter   // payload_audit_redis_drain_total{result="fail"}
	RedisRecoverOK   prometheus.Counter   // payload_audit_redis_recover_total{result="ok"}
	RedisRecoverFail prometheus.Counter   // payload_audit_redis_recover_total{result="fail"}
	CleanupDropped   prometheus.Counter   // payload_audit_cleanup_partitions_dropped_total
	CleanupDuration  prometheus.Histogram // payload_audit_cleanup_run_duration_seconds

	ExportRequests *prometheus.CounterVec // payload_audit_export_request_total{key_name,status}
	ExportRows     prometheus.Histogram   // payload_audit_export_rows_returned

	// collectors keeps references so we can unregister in tests if needed.
	collectors []prometheus.Collector
}

// RegisterPayloadAuditMetrics creates and registers all payload audit Prometheus
// metrics with the given registerer (typically prometheus.DefaultRegisterer).
//
// 6 polling-based metrics read from sink.Stats() on every scrape via a custom
// collector. 8 event-driven metrics (histograms + counters) are returned in
// the struct for call-site Observe/Inc at the appropriate code points.
func RegisterPayloadAuditMetrics(reg prometheus.Registerer, sink *PayloadAuditSink) *PayloadAuditMetrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &PayloadAuditMetrics{}

	// ---------------------------------------------------------------
	// 1. Polling-based metrics via custom collector
	// ---------------------------------------------------------------

	sinkCollector := newSinkStatsCollector(sink)

	// ---------------------------------------------------------------
	// 2. Event-driven metrics (call-site Observe / Inc)
	// ---------------------------------------------------------------

	m.InsertDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "insert_duration_seconds",
		Help:      "Time spent on batch INSERT operations.",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	m.InputBytesHist = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "input_bytes",
		Help:      "Input body size in bytes per event.",
		Buckets:   prometheus.ExponentialBuckets(256, 4, 8), // 256, 1K, 4K, 16K, 64K, 256K, 1M, 4M
	})

	m.OutputBytesHist = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "output_bytes",
		Help:      "Output body size in bytes per event.",
		Buckets:   prometheus.ExponentialBuckets(256, 4, 8),
	})

	truncatedVec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "truncated_total",
		Help:      "Number of events with truncated input or output.",
	}, []string{"which"})
	m.TruncatedInput = truncatedVec.WithLabelValues("input")
	m.TruncatedOutput = truncatedVec.WithLabelValues("output")

	redisDrainVec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "redis_drain_total",
		Help:      "Redis buffer drain attempts.",
	}, []string{"result"})
	m.RedisDrainOK = redisDrainVec.WithLabelValues("ok")
	m.RedisDrainFail = redisDrainVec.WithLabelValues("fail")

	redisRecoverVec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "redis_recover_total",
		Help:      "Redis buffer recovery attempts.",
	}, []string{"result"})
	m.RedisRecoverOK = redisRecoverVec.WithLabelValues("ok")
	m.RedisRecoverFail = redisRecoverVec.WithLabelValues("fail")

	m.CleanupDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "cleanup_partitions_dropped_total",
		Help:      "Number of partitions dropped by cleanup.",
	})

	m.CleanupDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "cleanup_run_duration_seconds",
		Help:      "Duration of cleanup runs.",
		Buckets:   []float64{0.1, 0.5, 1, 5, 10, 30, 60},
	})

	m.ExportRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "export_request_total",
		Help:      "Export API requests by key name and status.",
	}, []string{"key_name", "status"})

	m.ExportRows = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: payloadAuditMetricsNamespace,
		Name:      "export_rows_returned",
		Help:      "Number of rows returned per export request.",
		Buckets:   prometheus.ExponentialBuckets(1, 4, 8), // 1, 4, 16, 64, 256, 1024, 4096, 16384
	})

	// ---------------------------------------------------------------
	// 3. Register all
	// ---------------------------------------------------------------

	eventCollectors := []prometheus.Collector{
		m.InsertDuration,
		m.InputBytesHist,
		m.OutputBytesHist,
		truncatedVec,
		redisDrainVec,
		redisRecoverVec,
		m.CleanupDropped,
		m.CleanupDuration,
		m.ExportRequests,
		m.ExportRows,
	}

	all := make([]prometheus.Collector, 0, 1+len(eventCollectors))
	all = append(all, sinkCollector)
	all = append(all, eventCollectors...)

	for _, c := range all {
		reg.MustRegister(c)
	}
	m.collectors = all

	return m
}

// Unregister removes all registered metrics from the given registerer.
// Useful in tests to avoid duplicate registration panics.
func (m *PayloadAuditMetrics) Unregister(reg prometheus.Registerer) {
	if m == nil || reg == nil {
		return
	}
	for _, c := range m.collectors {
		reg.Unregister(c)
	}
}

// ---------------------------------------------------------------------------
// sinkStatsCollector — custom prometheus.Collector that reads SinkStats
// ---------------------------------------------------------------------------

type sinkStatsCollector struct {
	sink *PayloadAuditSink

	descEnqueued      *prometheus.Desc
	descQueueDepth    *prometheus.Desc
	descBatchInserted *prometheus.Desc
	descBatchFailed   *prometheus.Desc
	descWorkersActive *prometheus.Desc
}

func newSinkStatsCollector(sink *PayloadAuditSink) *sinkStatsCollector {
	return &sinkStatsCollector{
		sink: sink,
		descEnqueued: prometheus.NewDesc(
			prometheus.BuildFQName(payloadAuditMetricsNamespace, "", "enqueued_total"),
			"Total events enqueued or dropped.",
			[]string{"result"}, nil,
		),
		descQueueDepth: prometheus.NewDesc(
			prometheus.BuildFQName(payloadAuditMetricsNamespace, "", "queue_depth"),
			"Current number of events in the queue.",
			nil, nil,
		),
		descBatchInserted: prometheus.NewDesc(
			prometheus.BuildFQName(payloadAuditMetricsNamespace, "", "batch_inserted_total"),
			"Total events successfully batch-inserted.",
			nil, nil,
		),
		descBatchFailed: prometheus.NewDesc(
			prometheus.BuildFQName(payloadAuditMetricsNamespace, "", "batch_failed_total"),
			"Total events in failed batches.",
			nil, nil,
		),
		descWorkersActive: prometheus.NewDesc(
			prometheus.BuildFQName(payloadAuditMetricsNamespace, "", "workers_active"),
			"Number of configured sink worker goroutines.",
			nil, nil,
		),
	}
}

func (c *sinkStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descEnqueued
	ch <- c.descQueueDepth
	ch <- c.descBatchInserted
	ch <- c.descBatchFailed
	ch <- c.descWorkersActive
}

func (c *sinkStatsCollector) Collect(ch chan<- prometheus.Metric) {
	if c.sink == nil {
		return
	}
	s := c.sink.Stats()

	ch <- prometheus.MustNewConstMetric(c.descEnqueued, prometheus.CounterValue, float64(s.Accepted), "accepted")
	ch <- prometheus.MustNewConstMetric(c.descEnqueued, prometheus.CounterValue, float64(s.DropQueueFull+s.DropByteBudget), "dropped")
	ch <- prometheus.MustNewConstMetric(c.descQueueDepth, prometheus.GaugeValue, float64(s.QueueDepth))
	ch <- prometheus.MustNewConstMetric(c.descBatchInserted, prometheus.CounterValue, float64(s.BatchInserted))
	ch <- prometheus.MustNewConstMetric(c.descBatchFailed, prometheus.CounterValue, float64(s.BatchFailed))
	ch <- prometheus.MustNewConstMetric(c.descWorkersActive, prometheus.GaugeValue, float64(c.sink.cfg.WorkerCount))
}
