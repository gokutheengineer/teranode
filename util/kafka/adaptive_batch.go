package kafka

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/teranode/ulogger"
)

// AdaptiveBatchConfig holds configuration for the adaptive batching controller.
// When enabled, the producer monitors send latencies and dynamically adjusts
// batching parameters to optimize throughput under bandwidth constraints.
type AdaptiveBatchConfig struct {
	Enabled               bool          // Master switch for adaptive batching
	BaselineWindow        time.Duration // Duration to collect baseline latency samples
	ConstraintThreshold   float64       // Latency ratio (current/baseline) to trigger constraint mode
	RecoveryThreshold     float64       // Latency ratio to revert to normal mode
	MinLingerDelay        time.Duration // Linger delay when unconstrained
	MaxLingerDelay        time.Duration // Maximum linger delay under heavy constraint
	MinBatchTarget        int           // Batch accumulation target when unconstrained
	MaxBatchTarget        int           // Maximum batch accumulation target under constraint
	BackpressureThreshold int64         // Pending produce count that triggers backpressure
	EMAAlpha              float64       // Smoothing factor for exponential moving average (0,1]
	MinBaselineSamples    int           // Minimum samples before baseline is considered valid
}

// DefaultAdaptiveBatchConfig returns sensible defaults for adaptive batching.
func DefaultAdaptiveBatchConfig() AdaptiveBatchConfig {
	return AdaptiveBatchConfig{
		Enabled:               false,
		BaselineWindow:        10 * time.Second,
		ConstraintThreshold:   3.0,
		RecoveryThreshold:     1.5,
		MinLingerDelay:        0,
		MaxLingerDelay:        500 * time.Millisecond,
		MinBatchTarget:        1,
		MaxBatchTarget:        100,
		BackpressureThreshold: 10000,
		EMAAlpha:              0.3,
		MinBaselineSamples:    5,
	}
}

// BandwidthState represents the detected bandwidth condition.
type BandwidthState int

const (
	BandwidthNormal      BandwidthState = iota // No constraint detected
	BandwidthConstrained                        // Bandwidth degradation detected
)

// String returns a human-readable representation of the bandwidth state.
func (bs BandwidthState) String() string {
	switch bs {
	case BandwidthNormal:
		return "normal"
	case BandwidthConstrained:
		return "constrained"
	default:
		return "unknown"
	}
}

// AdaptiveBatchStats holds current statistics for monitoring and testing.
type AdaptiveBatchStats struct {
	BaselineLatencyMs  float64
	CurrentLatencyMs   float64
	LatencyRatio       float64
	State              BandwidthState
	LingerDelay        time.Duration
	BatchTarget        int
	TotalSamples       int64
	PendingCount       int64
	BackpressureActive bool
	BaselineReady      bool
}

// AdaptiveBatchController monitors send latencies and adjusts batching parameters
// to optimize throughput under network bandwidth constraints.
//
// It works by tracking an exponential moving average (EMA) of produce latencies.
// During startup, it establishes a baseline. When the current EMA exceeds the
// baseline by ConstraintThreshold, it increases linger delay and batch targets
// to amortize network overhead. When latency recovers, it reverts to normal.
type AdaptiveBatchController struct {
	config AdaptiveBatchConfig
	logger ulogger.Logger

	mu sync.RWMutex

	// Baseline tracking
	baselineLatencyNs float64
	baselineSamples   int
	baselineReady     bool
	baselineStart     time.Time

	// Current latency tracking (EMA)
	currentLatencyNs float64
	totalSamples     atomic.Int64

	// State
	state            BandwidthState
	constrainedSince time.Time

	// Adaptive parameters (read under mu)
	lingerDelay time.Duration
	batchTarget int

	// Backpressure (lock-free)
	pendingCount atomic.Int64
	backpressure atomic.Bool
}

// NewAdaptiveBatchController creates a new controller with the given config.
// Returns nil if config.Enabled is false.
func NewAdaptiveBatchController(config AdaptiveBatchConfig, logger ulogger.Logger) *AdaptiveBatchController {
	if !config.Enabled {
		return nil
	}

	if config.EMAAlpha <= 0 || config.EMAAlpha > 1 {
		config.EMAAlpha = 0.3
	}
	if config.ConstraintThreshold <= 1 {
		config.ConstraintThreshold = 3.0
	}
	if config.RecoveryThreshold <= 1 || config.RecoveryThreshold >= config.ConstraintThreshold {
		config.RecoveryThreshold = 1.5
	}
	if config.MaxBatchTarget <= 0 {
		config.MaxBatchTarget = 100
	}
	if config.MinBatchTarget <= 0 {
		config.MinBatchTarget = 1
	}
	if config.MinBaselineSamples <= 0 {
		config.MinBaselineSamples = 5
	}
	if config.BackpressureThreshold <= 0 {
		config.BackpressureThreshold = 10000
	}

	return &AdaptiveBatchController{
		config:        config,
		logger:        logger,
		batchTarget:   config.MinBatchTarget,
		lingerDelay:   config.MinLingerDelay,
		baselineStart: time.Now(),
	}
}

// RecordSendDuration records the duration of a produce operation's round-trip
// (from Produce() call to acknowledgment callback). This is the primary signal
// used for bandwidth constraint detection.
func (abc *AdaptiveBatchController) RecordSendDuration(d time.Duration) {
	abc.mu.Lock()
	defer abc.mu.Unlock()

	ns := float64(d.Nanoseconds())
	abc.totalSamples.Add(1)

	if !abc.baselineReady {
		abc.recordBaselineSample(ns)
		return
	}

	abc.currentLatencyNs = abc.config.EMAAlpha*ns + (1-abc.config.EMAAlpha)*abc.currentLatencyNs
	abc.evaluate()
}

// recordBaselineSample adds a sample during the baseline collection phase.
// Caller must hold abc.mu.
func (abc *AdaptiveBatchController) recordBaselineSample(ns float64) {
	abc.baselineSamples++
	if abc.baselineSamples == 1 {
		abc.baselineLatencyNs = ns
		abc.currentLatencyNs = ns
	} else {
		// Running average for baseline (more stable than EMA during warmup)
		abc.baselineLatencyNs += (ns - abc.baselineLatencyNs) / float64(abc.baselineSamples)
		abc.currentLatencyNs = abc.config.EMAAlpha*ns + (1-abc.config.EMAAlpha)*abc.currentLatencyNs
	}

	if time.Since(abc.baselineStart) >= abc.config.BaselineWindow && abc.baselineSamples >= abc.config.MinBaselineSamples {
		abc.baselineReady = true
		abc.logger.Infof("[adaptive-batch] Baseline established: %.2fms (%d samples)",
			abc.baselineLatencyNs/1e6, abc.baselineSamples)
	}
}

// evaluate checks current metrics and adjusts batching parameters.
// Caller must hold abc.mu.
func (abc *AdaptiveBatchController) evaluate() {
	if abc.baselineLatencyNs <= 0 {
		return
	}

	ratio := abc.currentLatencyNs / abc.baselineLatencyNs

	switch abc.state {
	case BandwidthNormal:
		if ratio >= abc.config.ConstraintThreshold {
			abc.state = BandwidthConstrained
			abc.constrainedSince = time.Now()
			abc.scaleUp(ratio)
			abc.logger.Warnf("[adaptive-batch] Bandwidth constraint detected (%.2fx baseline), topic=%s",
				ratio, abc.topicForLog())
		}
	case BandwidthConstrained:
		if ratio <= abc.config.RecoveryThreshold {
			abc.state = BandwidthNormal
			abc.scaleDown()
			constrainedFor := time.Since(abc.constrainedSince)
			abc.logger.Infof("[adaptive-batch] Bandwidth recovered (%.2fx baseline) after %v, topic=%s",
				ratio, constrainedFor, abc.topicForLog())
		} else {
			abc.scaleUp(ratio)
		}
	}
}

func (abc *AdaptiveBatchController) topicForLog() string {
	return "producer"
}

// scaleUp adjusts batching parameters proportionally to the degradation level.
// scaleFactor ranges from 1.0 (at threshold) to 5.0 (severe).
// Caller must hold abc.mu.
func (abc *AdaptiveBatchController) scaleUp(ratio float64) {
	maxScale := 5.0
	scaleFactor := math.Min(ratio/abc.config.ConstraintThreshold, maxScale)
	normalized := scaleFactor / maxScale // [0.2, 1.0]

	// Linger delay: linear interpolation between min and max
	newLinger := time.Duration(float64(abc.config.MaxLingerDelay) * normalized)
	if newLinger < abc.config.MinLingerDelay {
		newLinger = abc.config.MinLingerDelay
	}
	abc.lingerDelay = newLinger

	// Batch target: linear interpolation between min and max
	batchRange := abc.config.MaxBatchTarget - abc.config.MinBatchTarget
	newTarget := abc.config.MinBatchTarget + int(float64(batchRange)*normalized)
	if newTarget > abc.config.MaxBatchTarget {
		newTarget = abc.config.MaxBatchTarget
	}
	abc.batchTarget = newTarget
}

// scaleDown reverts batching parameters to unconstrained defaults.
// Caller must hold abc.mu.
func (abc *AdaptiveBatchController) scaleDown() {
	abc.lingerDelay = abc.config.MinLingerDelay
	abc.batchTarget = abc.config.MinBatchTarget
}

// GetLingerDelay returns the current recommended linger delay.
// The producer should sleep for this duration after receiving the first message
// in a batch to allow more messages to accumulate.
func (abc *AdaptiveBatchController) GetLingerDelay() time.Duration {
	abc.mu.RLock()
	defer abc.mu.RUnlock()
	return abc.lingerDelay
}

// GetBatchTarget returns the current recommended number of messages to accumulate
// before submitting to the underlying Kafka client.
func (abc *AdaptiveBatchController) GetBatchTarget() int {
	abc.mu.RLock()
	defer abc.mu.RUnlock()
	return abc.batchTarget
}

// IsConstrained returns whether a bandwidth constraint is currently detected.
func (abc *AdaptiveBatchController) IsConstrained() bool {
	abc.mu.RLock()
	defer abc.mu.RUnlock()
	return abc.state == BandwidthConstrained
}

// IsBackpressured returns whether backpressure is active due to excessive
// pending (unacknowledged) produce operations.
func (abc *AdaptiveBatchController) IsBackpressured() bool {
	return abc.backpressure.Load()
}

// IncrementPending increments the count of pending produce operations.
// When the count exceeds BackpressureThreshold, backpressure is activated.
func (abc *AdaptiveBatchController) IncrementPending() {
	count := abc.pendingCount.Add(1)
	if count >= abc.config.BackpressureThreshold && !abc.backpressure.Load() {
		abc.backpressure.Store(true)
		abc.logger.Warnf("[adaptive-batch] Backpressure activated: %d pending exceeds threshold %d",
			count, abc.config.BackpressureThreshold)
	}
}

// DecrementPending decrements the count of pending produce operations.
// Backpressure is released when the count falls below 75% of the threshold.
func (abc *AdaptiveBatchController) DecrementPending() {
	count := abc.pendingCount.Add(-1)
	releaseAt := abc.config.BackpressureThreshold * 3 / 4
	if count < releaseAt && abc.backpressure.Load() {
		abc.backpressure.Store(false)
		abc.logger.Infof("[adaptive-batch] Backpressure released: %d pending below release threshold %d",
			count, releaseAt)
	}
}

// Stats returns a snapshot of the controller's current state for monitoring.
func (abc *AdaptiveBatchController) Stats() AdaptiveBatchStats {
	abc.mu.RLock()
	defer abc.mu.RUnlock()

	var ratio float64
	if abc.baselineLatencyNs > 0 {
		ratio = abc.currentLatencyNs / abc.baselineLatencyNs
	}

	return AdaptiveBatchStats{
		BaselineLatencyMs:  abc.baselineLatencyNs / 1e6,
		CurrentLatencyMs:   abc.currentLatencyNs / 1e6,
		LatencyRatio:       ratio,
		State:              abc.state,
		LingerDelay:        abc.lingerDelay,
		BatchTarget:        abc.batchTarget,
		TotalSamples:       abc.totalSamples.Load(),
		PendingCount:       abc.pendingCount.Load(),
		BackpressureActive: abc.backpressure.Load(),
		BaselineReady:      abc.baselineReady,
	}
}
