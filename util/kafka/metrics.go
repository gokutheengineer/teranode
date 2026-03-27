// Package kafka provides Kafka consumer and producer implementations for message handling.
package kafka

import (
	"sync"

	"github.com/bsv-blockchain/teranode/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for monitoring Kafka consumer performance and health.
//
// These metrics provide comprehensive observability into the Kafka consumer,
// enabling monitoring of watchdog recovery operations, stuck consumer detection,
// and overall consumer health. The metrics are automatically registered with Prometheus
// and can be scraped by monitoring systems.
//
// Metric Categories:
//   - Watchdog metrics: Recovery attempts and stuck consumer detection
//   - Duration metrics: Time spent in various consumer states
var (
	// prometheusKafkaWatchdogRecoveryAttempts counts the number of times the watchdog
	// triggered a force recovery due to a stuck Consume() call.
	// Labels: topic, consumer_group
	// This counter helps identify how often consumers get stuck and need recovery,
	// which may indicate issues with Kafka brokers or network connectivity.
	prometheusKafkaWatchdogRecoveryAttempts *prometheus.CounterVec

	// prometheusKafkaWatchdogStuckDuration tracks how long the consumer was stuck
	// before the watchdog triggered recovery.
	// Labels: topic
	// This histogram measures the duration between when Consume() was called and
	// when the watchdog detected it as stuck, helping diagnose consumer hangs.
	prometheusKafkaWatchdogStuckDuration *prometheus.HistogramVec

	// prometheusKafkaProducerSendDuration tracks end-to-end produce latency (from
	// Produce() call to broker acknowledgment).
	// Labels: topic
	prometheusKafkaProducerSendDuration *prometheus.HistogramVec

	// prometheusKafkaProducerAdaptiveState reports whether the adaptive batch
	// controller is in constrained mode (1) or normal mode (0).
	// Labels: topic
	prometheusKafkaProducerAdaptiveState *prometheus.GaugeVec

	// prometheusKafkaProducerBackpressureTotal counts the number of times
	// backpressure was activated due to excessive pending produces.
	// Labels: topic
	prometheusKafkaProducerBackpressureTotal *prometheus.CounterVec
)

var (
	prometheusMetricsInitOnce sync.Once
)

// InitPrometheusMetrics initializes Prometheus metrics for Kafka consumers.
// This function is idempotent and can be called multiple times safely.
func InitPrometheusMetrics() {
	prometheusMetricsInitOnce.Do(_initPrometheusMetrics)
}

func _initPrometheusMetrics() {
	prometheusKafkaWatchdogRecoveryAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "teranode",
			Subsystem: "kafka",
			Name:      "watchdog_recovery_attempts_total",
			Help:      "Number of times the watchdog triggered force recovery for stuck consumer",
		},
		[]string{"topic", "consumer_group"},
	)

	prometheusKafkaWatchdogStuckDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "kafka",
			Name:      "watchdog_stuck_duration_seconds",
			Help:      "Duration (in seconds) that consumer was stuck before watchdog recovery",
			Buckets:   util.MetricsBucketsSeconds,
		},
		[]string{"topic"},
	)

	prometheusKafkaProducerSendDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "teranode",
			Subsystem: "kafka",
			Name:      "producer_send_duration_seconds",
			Help:      "End-to-end produce latency from Produce() call to broker acknowledgment",
			Buckets:   util.MetricsBucketsSeconds,
		},
		[]string{"topic"},
	)

	prometheusKafkaProducerAdaptiveState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "teranode",
			Subsystem: "kafka",
			Name:      "producer_adaptive_batch_constrained",
			Help:      "Whether the adaptive batch controller detects bandwidth constraint (1=constrained, 0=normal)",
		},
		[]string{"topic"},
	)

	prometheusKafkaProducerBackpressureTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "teranode",
			Subsystem: "kafka",
			Name:      "producer_backpressure_activations_total",
			Help:      "Number of times backpressure was activated due to excessive pending produces",
		},
		[]string{"topic"},
	)
}
