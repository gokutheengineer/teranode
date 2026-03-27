package kafka

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAdaptiveBatchController_DisabledReturnsNil(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = false

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})
	assert.Nil(t, controller)
}

func TestNewAdaptiveBatchController_EnabledReturnsController(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})
	require.NotNil(t, controller)
	assert.Equal(t, cfg.MinBatchTarget, controller.GetBatchTarget())
	assert.Equal(t, cfg.MinLingerDelay, controller.GetLingerDelay())
	assert.False(t, controller.IsConstrained())
	assert.False(t, controller.IsBackpressured())
}

func TestNewAdaptiveBatchController_SanitizesInvalidConfig(t *testing.T) {
	cfg := AdaptiveBatchConfig{
		Enabled:               true,
		EMAAlpha:              -1,
		ConstraintThreshold:   0.5,
		RecoveryThreshold:     10.0,
		MaxBatchTarget:        -5,
		MinBatchTarget:        -1,
		MinBaselineSamples:    0,
		BackpressureThreshold: 0,
	}

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})
	require.NotNil(t, controller)

	assert.Equal(t, 0.3, controller.config.EMAAlpha)
	assert.Equal(t, 3.0, controller.config.ConstraintThreshold)
	assert.Equal(t, 1.5, controller.config.RecoveryThreshold)
	assert.Equal(t, 100, controller.config.MaxBatchTarget)
	assert.Equal(t, 1, controller.config.MinBatchTarget)
	assert.Equal(t, 5, controller.config.MinBaselineSamples)
	assert.Equal(t, int64(10000), controller.config.BackpressureThreshold)
}

func TestAdaptiveBatchController_BaselineEstablishment(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 50 * time.Millisecond
	cfg.MinBaselineSamples = 3

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})
	require.NotNil(t, controller)

	stats := controller.Stats()
	assert.False(t, stats.BaselineReady)

	// Record samples within baseline window
	controller.RecordSendDuration(10 * time.Millisecond)
	controller.RecordSendDuration(12 * time.Millisecond)

	stats = controller.Stats()
	assert.False(t, stats.BaselineReady)

	// Wait for baseline window to pass
	time.Sleep(60 * time.Millisecond)

	controller.RecordSendDuration(11 * time.Millisecond)

	stats = controller.Stats()
	assert.True(t, stats.BaselineReady)
	assert.InDelta(t, 11.0, stats.BaselineLatencyMs, 1.0)
}

func TestAdaptiveBatchController_ConstraintDetection(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 10 * time.Millisecond
	cfg.MinBaselineSamples = 3
	cfg.ConstraintThreshold = 3.0
	cfg.EMAAlpha = 0.9 // High alpha for fast response in tests

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	// Establish baseline ~10ms
	for i := 0; i < 3; i++ {
		controller.RecordSendDuration(10 * time.Millisecond)
	}
	time.Sleep(15 * time.Millisecond)
	controller.RecordSendDuration(10 * time.Millisecond)

	assert.True(t, controller.Stats().BaselineReady)
	assert.False(t, controller.IsConstrained())

	// Simulate bandwidth degradation: latency jumps to 50ms (5x baseline)
	for i := 0; i < 5; i++ {
		controller.RecordSendDuration(50 * time.Millisecond)
	}

	assert.True(t, controller.IsConstrained())
	stats := controller.Stats()
	assert.Equal(t, BandwidthConstrained, stats.State)
	assert.Greater(t, stats.LingerDelay, time.Duration(0))
	assert.Greater(t, stats.BatchTarget, cfg.MinBatchTarget)
}

func TestAdaptiveBatchController_Recovery(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 10 * time.Millisecond
	cfg.MinBaselineSamples = 3
	cfg.ConstraintThreshold = 3.0
	cfg.RecoveryThreshold = 1.5
	cfg.EMAAlpha = 0.9

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	// Establish baseline ~10ms
	for i := 0; i < 3; i++ {
		controller.RecordSendDuration(10 * time.Millisecond)
	}
	time.Sleep(15 * time.Millisecond)
	controller.RecordSendDuration(10 * time.Millisecond)

	// Enter constrained state
	for i := 0; i < 5; i++ {
		controller.RecordSendDuration(50 * time.Millisecond)
	}
	assert.True(t, controller.IsConstrained())

	// Simulate recovery: latency drops back to near-baseline
	for i := 0; i < 10; i++ {
		controller.RecordSendDuration(12 * time.Millisecond)
	}

	assert.False(t, controller.IsConstrained())
	stats := controller.Stats()
	assert.Equal(t, BandwidthNormal, stats.State)
	assert.Equal(t, cfg.MinLingerDelay, stats.LingerDelay)
	assert.Equal(t, cfg.MinBatchTarget, stats.BatchTarget)
}

func TestAdaptiveBatchController_GracefulRecoveryRevertsParams(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 10 * time.Millisecond
	cfg.MinBaselineSamples = 3
	cfg.EMAAlpha = 0.95

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	// Establish baseline
	for i := 0; i < 5; i++ {
		controller.RecordSendDuration(5 * time.Millisecond)
	}
	time.Sleep(15 * time.Millisecond)
	controller.RecordSendDuration(5 * time.Millisecond)

	// Verify initial state
	assert.Equal(t, cfg.MinBatchTarget, controller.GetBatchTarget())
	assert.Equal(t, cfg.MinLingerDelay, controller.GetLingerDelay())

	// Trigger constraint (15x degradation)
	for i := 0; i < 10; i++ {
		controller.RecordSendDuration(75 * time.Millisecond)
	}

	constrainedTarget := controller.GetBatchTarget()
	constrainedLinger := controller.GetLingerDelay()
	assert.Greater(t, constrainedTarget, cfg.MinBatchTarget)
	assert.Greater(t, constrainedLinger, cfg.MinLingerDelay)

	// Full recovery
	for i := 0; i < 20; i++ {
		controller.RecordSendDuration(5 * time.Millisecond)
	}

	assert.Equal(t, cfg.MinBatchTarget, controller.GetBatchTarget())
	assert.Equal(t, cfg.MinLingerDelay, controller.GetLingerDelay())
	assert.Less(t, controller.GetBatchTarget(), constrainedTarget)
	assert.Less(t, controller.GetLingerDelay(), constrainedLinger)
}

func TestAdaptiveBatchController_ScalingProportionalToDegradation(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 10 * time.Millisecond
	cfg.MinBaselineSamples = 3
	cfg.EMAAlpha = 0.95

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	// Establish baseline at 10ms
	for i := 0; i < 5; i++ {
		controller.RecordSendDuration(10 * time.Millisecond)
	}
	time.Sleep(15 * time.Millisecond)
	controller.RecordSendDuration(10 * time.Millisecond)

	// Moderate degradation (3x = at threshold)
	for i := 0; i < 10; i++ {
		controller.RecordSendDuration(30 * time.Millisecond)
	}
	moderateTarget := controller.GetBatchTarget()
	moderateLinger := controller.GetLingerDelay()

	// Reset by recovering
	for i := 0; i < 20; i++ {
		controller.RecordSendDuration(10 * time.Millisecond)
	}

	// Severe degradation (15x)
	for i := 0; i < 10; i++ {
		controller.RecordSendDuration(150 * time.Millisecond)
	}
	severeTarget := controller.GetBatchTarget()
	severeLinger := controller.GetLingerDelay()

	assert.Greater(t, severeTarget, moderateTarget, "severe degradation should have larger batch target")
	assert.Greater(t, severeLinger, moderateLinger, "severe degradation should have larger linger delay")
}

func TestAdaptiveBatchController_Backpressure(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BackpressureThreshold = 5

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	assert.False(t, controller.IsBackpressured())

	// Exceed threshold
	for i := 0; i < 5; i++ {
		controller.IncrementPending()
	}
	assert.True(t, controller.IsBackpressured())

	// Decrement below release threshold (75% of 5 = 3, need count < 3)
	controller.DecrementPending() // count=4, still above 3
	controller.DecrementPending() // count=3, not strictly below 3
	assert.True(t, controller.IsBackpressured(), "count=3 is not below release threshold 3")
	controller.DecrementPending() // count=2, now below 3
	assert.False(t, controller.IsBackpressured())

	stats := controller.Stats()
	assert.Equal(t, int64(2), stats.PendingCount)
}

func TestAdaptiveBatchController_BackpressureReleaseHysteresis(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BackpressureThreshold = 100

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	// Fill to threshold
	for i := 0; i < 100; i++ {
		controller.IncrementPending()
	}
	assert.True(t, controller.IsBackpressured())

	// Decrement to 76 (still above 75% = 75)
	for i := 0; i < 24; i++ {
		controller.DecrementPending()
	}
	assert.True(t, controller.IsBackpressured(), "should still be backpressured above 75%% threshold")

	// Decrement to 74 (below 75)
	controller.DecrementPending()
	controller.DecrementPending()
	assert.False(t, controller.IsBackpressured())
}

func TestAdaptiveBatchController_Stats(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 10 * time.Millisecond
	cfg.MinBaselineSamples = 2

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	controller.RecordSendDuration(10 * time.Millisecond)
	controller.RecordSendDuration(10 * time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	controller.RecordSendDuration(10 * time.Millisecond)

	stats := controller.Stats()
	assert.True(t, stats.BaselineReady)
	assert.Equal(t, int64(3), stats.TotalSamples)
	assert.Equal(t, BandwidthNormal, stats.State)
	assert.InDelta(t, 10.0, stats.BaselineLatencyMs, 1.0)
	assert.Greater(t, stats.CurrentLatencyMs, 0.0)
}

func TestBandwidthState_String(t *testing.T) {
	assert.Equal(t, "normal", BandwidthNormal.String())
	assert.Equal(t, "constrained", BandwidthConstrained.String())
	assert.Equal(t, "unknown", BandwidthState(99).String())
}

func TestDefaultAdaptiveBatchConfig(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()

	assert.False(t, cfg.Enabled)
	assert.Equal(t, 10*time.Second, cfg.BaselineWindow)
	assert.Equal(t, 3.0, cfg.ConstraintThreshold)
	assert.Equal(t, 1.5, cfg.RecoveryThreshold)
	assert.Equal(t, time.Duration(0), cfg.MinLingerDelay)
	assert.Equal(t, 500*time.Millisecond, cfg.MaxLingerDelay)
	assert.Equal(t, 1, cfg.MinBatchTarget)
	assert.Equal(t, 100, cfg.MaxBatchTarget)
	assert.Equal(t, int64(10000), cfg.BackpressureThreshold)
	assert.Equal(t, 0.3, cfg.EMAAlpha)
	assert.Equal(t, 5, cfg.MinBaselineSamples)
}

func TestCollectBatch_SingleMessage(t *testing.T) {
	producer := &KafkaAsyncProducer{}
	ch := make(chan *Message, 10)

	msg := &Message{Key: []byte("k"), Value: []byte("v")}
	ch <- msg

	batch := producer.collectBatch(ch, 10, 0)
	require.Len(t, batch, 1)
	assert.Equal(t, msg, batch[0])
}

func TestCollectBatch_MultipleMessages(t *testing.T) {
	producer := &KafkaAsyncProducer{}
	ch := make(chan *Message, 10)

	for i := 0; i < 5; i++ {
		ch <- &Message{Key: []byte{byte(i)}, Value: []byte{byte(i)}}
	}

	batch := producer.collectBatch(ch, 10, 0)
	assert.Len(t, batch, 5)
}

func TestCollectBatch_RespectsTarget(t *testing.T) {
	producer := &KafkaAsyncProducer{}
	ch := make(chan *Message, 100)

	for i := 0; i < 50; i++ {
		ch <- &Message{Key: []byte{byte(i)}, Value: []byte{byte(i)}}
	}

	batch := producer.collectBatch(ch, 10, 0)
	assert.Len(t, batch, 10)
}

func TestCollectBatch_ClosedChannelReturnsNil(t *testing.T) {
	producer := &KafkaAsyncProducer{}
	ch := make(chan *Message)
	close(ch)

	batch := producer.collectBatch(ch, 10, 0)
	assert.Nil(t, batch)
}

func TestCollectBatch_LingerAccumulatesMore(t *testing.T) {
	producer := &KafkaAsyncProducer{}
	ch := make(chan *Message, 100)

	ch <- &Message{Key: []byte("first"), Value: []byte("v")}

	// Feed messages in a goroutine during linger period
	go func() {
		time.Sleep(10 * time.Millisecond)
		for i := 0; i < 5; i++ {
			ch <- &Message{Key: []byte{byte(i)}, Value: []byte{byte(i)}}
		}
	}()

	batch := producer.collectBatch(ch, 100, 30*time.Millisecond)
	assert.Greater(t, len(batch), 1, "linger should allow more messages to accumulate")
}

func TestCollectBatch_ClosedProducerReturnsNil(t *testing.T) {
	producer := &KafkaAsyncProducer{}
	producer.closed.Store(true)

	ch := make(chan *Message, 10)
	ch <- &Message{Key: []byte("k"), Value: []byte("v")}

	batch := producer.collectBatch(ch, 10, 0)
	assert.Nil(t, batch)
}

func TestAdaptiveBatchController_DetectionWithin5Seconds(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 2 * time.Second
	cfg.MinBaselineSamples = 10
	cfg.EMAAlpha = 0.5

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	// Simulate ~50 samples/sec baseline for 2 seconds
	start := time.Now()
	for time.Since(start) < 2100*time.Millisecond {
		controller.RecordSendDuration(10 * time.Millisecond)
		time.Sleep(20 * time.Millisecond)
	}

	assert.True(t, controller.Stats().BaselineReady)

	// Simulate sudden degradation
	degradeStart := time.Now()
	for !controller.IsConstrained() && time.Since(degradeStart) < 5*time.Second {
		controller.RecordSendDuration(50 * time.Millisecond)
		time.Sleep(20 * time.Millisecond)
	}

	detectionTime := time.Since(degradeStart)
	assert.True(t, controller.IsConstrained(), "should detect constraint")
	assert.Less(t, detectionTime, 5*time.Second, "detection should happen within 5 seconds")
	t.Logf("Bandwidth constraint detected in %v", detectionTime)
}

func TestAdaptiveBatchFromSettings_Enabled(t *testing.T) {
	logger := &mockAsyncLogger{}
	ctx := context.Background()

	kafkaURL, err := url.Parse("memory://localhost/test-topic")
	require.NoError(t, err)

	ks := &settings.KafkaSettings{
		AdaptiveBatchEnabled:             true,
		AdaptiveBatchConstraintThreshold: 4.0,
		AdaptiveBatchRecoveryThreshold:   2.0,
		AdaptiveBatchMaxLingerMs:         300,
		AdaptiveBatchMaxBatchTarget:      50,
	}

	producer, err := NewKafkaAsyncProducerFromURL(ctx, logger, kafkaURL, ks)
	require.NoError(t, err)
	require.NotNil(t, producer)

	assert.True(t, producer.Config.AdaptiveBatch.Enabled)
	assert.NotNil(t, producer.adaptiveBatcher)
	assert.Equal(t, 4.0, producer.Config.AdaptiveBatch.ConstraintThreshold)
	assert.Equal(t, 2.0, producer.Config.AdaptiveBatch.RecoveryThreshold)
	assert.Equal(t, 300*time.Millisecond, producer.Config.AdaptiveBatch.MaxLingerDelay)
	assert.Equal(t, 50, producer.Config.AdaptiveBatch.MaxBatchTarget)
}

func TestAdaptiveBatchFromSettings_Disabled(t *testing.T) {
	logger := &mockAsyncLogger{}
	ctx := context.Background()

	kafkaURL, err := url.Parse("memory://localhost/test-topic")
	require.NoError(t, err)

	producer, err := NewKafkaAsyncProducerFromURL(ctx, logger, kafkaURL, nil)
	require.NoError(t, err)
	require.NotNil(t, producer)

	assert.False(t, producer.Config.AdaptiveBatch.Enabled)
	assert.Nil(t, producer.adaptiveBatcher)
}

func TestAdaptiveBatchFromSettings_NilSettingsUsesDefaults(t *testing.T) {
	logger := &mockAsyncLogger{}
	ctx := context.Background()

	kafkaURL, err := url.Parse("memory://localhost/test-topic")
	require.NoError(t, err)

	producer, err := NewKafkaAsyncProducerFromURL(ctx, logger, kafkaURL, nil)
	require.NoError(t, err)
	require.NotNil(t, producer)

	defaults := DefaultAdaptiveBatchConfig()
	assert.Equal(t, defaults.Enabled, producer.Config.AdaptiveBatch.Enabled)
	assert.Equal(t, defaults.ConstraintThreshold, producer.Config.AdaptiveBatch.ConstraintThreshold)
	assert.Equal(t, defaults.RecoveryThreshold, producer.Config.AdaptiveBatch.RecoveryThreshold)
}

func TestAdaptiveBatchFromSettings_InMemoryProducer(t *testing.T) {
	logger := &mockAsyncLogger{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kafkaURL, err := url.Parse("memory://localhost/test-adaptive-integration")
	require.NoError(t, err)

	ks := &settings.KafkaSettings{
		AdaptiveBatchEnabled: true,
	}

	producer, err := NewKafkaAsyncProducerFromURL(ctx, logger, kafkaURL, ks)
	require.NoError(t, err)
	require.NotNil(t, producer)

	assert.True(t, producer.Config.AdaptiveBatch.Enabled)
	assert.NotNil(t, producer.adaptiveBatcher)
}

func TestAdaptiveBatchController_NoPanicOnZeroBaseline(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 0
	cfg.MinBaselineSamples = 1
	cfg.EMAAlpha = 1.0

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	assert.NotPanics(t, func() {
		controller.RecordSendDuration(0)
		controller.RecordSendDuration(time.Nanosecond)
	})
}

func TestAdaptiveBatchController_ConcurrentAccess(t *testing.T) {
	cfg := DefaultAdaptiveBatchConfig()
	cfg.Enabled = true
	cfg.BaselineWindow = 10 * time.Millisecond
	cfg.MinBaselineSamples = 2

	controller := NewAdaptiveBatchController(cfg, &mockAsyncLogger{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			controller.RecordSendDuration(10 * time.Millisecond)
		}
	}()

	for i := 0; i < 1000; i++ {
		_ = controller.GetLingerDelay()
		_ = controller.GetBatchTarget()
		_ = controller.IsConstrained()
		_ = controller.IsBackpressured()
		_ = controller.Stats()
		controller.IncrementPending()
		controller.DecrementPending()
	}

	<-done
}
