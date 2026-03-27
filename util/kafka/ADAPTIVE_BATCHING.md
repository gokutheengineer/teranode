# Adaptive Batching for Kafka Producers

## Overview

Adaptive Batching is a feature of the Teranode Kafka async producer that **automatically detects network bandwidth constraints and adjusts batching parameters in real time** to maintain optimal throughput.

Under normal conditions the producer sends messages to the broker as they arrive. When the network degrades (e.g. datacenter congestion, cross-region replication, or bandwidth throttling), sending small individual messages becomes highly inefficient because each send pays the full round-trip overhead. Adaptive Batching solves this by:

1. Accumulating more messages per batch (increasing **batch target**)
2. Holding messages slightly longer before sending (increasing **linger delay**)
3. Applying **backpressure** when the outbound buffer grows too large
4. **Automatically reverting** to normal behavior once the network recovers

The feature is **disabled by default** and fully opt-in.

---

## Quick Start

Adaptive batching is configured through the standard Teranode settings system, alongside all other Kafka configuration.

### Via environment variable or config file

```bash
KAFKA_ADAPTIVE_BATCH_ENABLED=true
```

Or in `settings.conf` / `settings_local.conf`:

```
KAFKA_ADAPTIVE_BATCH_ENABLED=true
```

That's it. All other parameters use sensible defaults. The feature flows through `KafkaSettings` and is automatically applied to every async producer created via `NewKafkaAsyncProducerFromURL`.

### Programmatic configuration

When constructing a producer directly in code (e.g. in tests), set fields on `KafkaProducerConfig`:

```go
cfg := kafka.KafkaProducerConfig{
    // ...standard fields...
    AdaptiveBatch: kafka.AdaptiveBatchConfig{
        Enabled: true,
        // all other fields use sensible defaults
    },
}
producer, err := kafka.NewKafkaAsyncProducer(logger, cfg)
```

---

## How It Works

### Phase 1: Baseline Collection

When the producer starts, it enters a **baseline phase** lasting `BaselineWindow` (default 10 seconds). During this phase every produce acknowledgment latency is recorded as a running average. The baseline is locked in once both conditions are met:

- At least `MinBaselineSamples` (default 5) acknowledgments have been received
- The `BaselineWindow` duration has elapsed

Until the baseline is ready, the producer operates identically to a non-adaptive producer (batch target = 1, linger delay = 0).

### Phase 2: Continuous Monitoring

After baseline, every produce acknowledgment updates an **Exponential Moving Average (EMA)** of latency:

```
currentLatency = alpha * newSample + (1 - alpha) * currentLatency
```

The `EMAAlpha` parameter (default 0.3) controls responsiveness. Higher values react faster to changes; lower values smooth out noise.

### Phase 3: Constraint Detection

On every sample, the controller computes the **latency ratio**:

```
ratio = currentLatency / baselineLatency
```

| Condition | Transition |
|-----------|-----------|
| `ratio >= ConstraintThreshold` (default 3.0x) | Normal -> **Constrained** |
| `ratio <= RecoveryThreshold` (default 1.5x) | Constrained -> **Normal** |

The gap between the two thresholds provides **hysteresis** to prevent rapid mode flapping.

### Phase 4: Parameter Scaling

When constrained, both parameters scale **linearly** with the severity of degradation:

```
scaleFactor = min(ratio / ConstraintThreshold, 5.0)
normalized  = scaleFactor / 5.0    // range [0.2, 1.0]

lingerDelay = MaxLingerDelay * normalized
batchTarget = MinBatchTarget + (MaxBatchTarget - MinBatchTarget) * normalized
```

| Degradation | Linger Delay | Batch Target |
|-------------|-------------|-------------|
| 3x baseline (at threshold) | ~100ms | ~21 |
| 5x baseline | ~167ms | ~34 |
| 10x baseline | ~333ms | ~67 |
| 15x+ baseline (severe) | 500ms | 100 |

### Phase 5: Recovery

When latency drops back below `RecoveryThreshold`, linger delay and batch target **immediately revert** to their unconstrained defaults (`MinLingerDelay` and `MinBatchTarget`).

---

## Configuration Reference

Adaptive batching is configured through `KafkaSettings` in the standard Teranode settings system. The settings below map to environment variables / config file keys and are applied to all async producers.

### Settings (environment variables / config keys)

| Setting Key | Type | Default | Description |
|-------------|------|---------|-------------|
| `KAFKA_ADAPTIVE_BATCH_ENABLED` | `bool` | `false` | Master switch. When false, the producer uses the standard non-adaptive code path with zero overhead. |
| `KAFKA_ADAPTIVE_BATCH_CONSTRAINT_THRESHOLD` | `float64` | `3.0` | Latency ratio (current EMA / baseline) that triggers constrained mode. A value of 3.0 means "latency has tripled compared to startup". |
| `KAFKA_ADAPTIVE_BATCH_RECOVERY_THRESHOLD` | `float64` | `1.5` | Latency ratio below which the controller exits constrained mode and reverts to normal batching. Must be less than constraint threshold. |
| `KAFKA_ADAPTIVE_BATCH_MAX_LINGER_MS` | `int` | `500` | Maximum application-level linger delay (in milliseconds) under severe bandwidth constraint. The controller interpolates between 0 and this value based on degradation severity. |
| `KAFKA_ADAPTIVE_BATCH_MAX_BATCH_TARGET` | `int` | `100` | Maximum number of messages to accumulate per batch under constraint. Larger values amortize more overhead but increase latency per message. |
| `KAFKA_ADAPTIVE_BATCH_BACKPRESSURE_THRESHOLD` | `int` | `10000` | Number of pending (unacknowledged) produce operations that triggers backpressure. When reached, the produce loop pauses until pending count drops below 75% of this threshold. |

### Internal parameters (AdaptiveBatchConfig)

The following parameters are not exposed as settings but can be tuned when constructing `KafkaProducerConfig` programmatically:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `BaselineWindow` | `time.Duration` | `10s` | How long to collect latency samples before establishing the baseline. |
| `MinBaselineSamples` | `int` | `5` | Minimum acknowledged produces required before baseline is valid. |
| `MinLingerDelay` | `time.Duration` | `0` | Linger delay when unconstrained. Zero = immediate submission. |
| `MinBatchTarget` | `int` | `1` | Batch size when unconstrained. 1 = one-at-a-time (standard behavior). |
| `EMAAlpha` | `float64` | `0.3` | Smoothing factor for the Exponential Moving Average. Range: (0, 1]. Higher = faster response, noisier. |

---

## Backpressure

Backpressure prevents unbounded memory growth when the broker cannot keep up with the produce rate.

**How it works:**

1. Every call to `Produce()` increments a pending counter
2. Every broker acknowledgment (success or failure) decrements it
3. When pending count reaches `BackpressureThreshold`, the produce loop **pauses**
4. The loop resumes when pending count drops below **75%** of the threshold (hysteresis)

During backpressure, the produce loop sleeps in 10ms increments while checking for shutdown signals, so it remains responsive to context cancellation.

---

## Prometheus Metrics

When adaptive batching is enabled, the following Prometheus metrics are emitted:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `teranode_kafka_producer_send_duration_seconds` | Histogram | `topic` | End-to-end produce latency from `Produce()` call to broker acknowledgment. Use this to observe the raw latency the controller is tracking. |
| `teranode_kafka_producer_adaptive_batch_constrained` | Gauge | `topic` | `1` when bandwidth constraint is detected, `0` when normal. Alert on this staying at `1` for extended periods. |
| `teranode_kafka_producer_backpressure_activations_total` | Counter | `topic` | Number of times backpressure was activated. Frequent activations indicate the producer is outpacing network capacity. |

### Example Prometheus Alerts

```yaml
# Alert when a producer is constrained for more than 5 minutes
- alert: KafkaProducerBandwidthConstrained
  expr: teranode_kafka_producer_adaptive_batch_constrained == 1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "Kafka producer detecting sustained bandwidth constraint"

# Alert on frequent backpressure
- alert: KafkaProducerBackpressureFrequent
  expr: rate(teranode_kafka_producer_backpressure_activations_total[5m]) > 1
  labels:
    severity: critical
  annotations:
    summary: "Kafka producer backpressure activating frequently"
```

---

## Tuning Guide

### Low-latency environments (datacenter, same-region)

Fast detection with minimal false positives:

```bash
KAFKA_ADAPTIVE_BATCH_ENABLED=true
KAFKA_ADAPTIVE_BATCH_CONSTRAINT_THRESHOLD=2.5
KAFKA_ADAPTIVE_BATCH_RECOVERY_THRESHOLD=1.3
KAFKA_ADAPTIVE_BATCH_MAX_LINGER_MS=200
KAFKA_ADAPTIVE_BATCH_MAX_BATCH_TARGET=50
```

### High-latency environments (cross-region, satellite)

Wider thresholds to avoid triggering on normal jitter:

```bash
KAFKA_ADAPTIVE_BATCH_ENABLED=true
KAFKA_ADAPTIVE_BATCH_CONSTRAINT_THRESHOLD=5.0
KAFKA_ADAPTIVE_BATCH_RECOVERY_THRESHOLD=2.0
KAFKA_ADAPTIVE_BATCH_MAX_LINGER_MS=1000
KAFKA_ADAPTIVE_BATCH_MAX_BATCH_TARGET=200
```

### High-throughput topics (txmeta, subtrees)

Increase backpressure tolerance and batch ceiling:

```bash
KAFKA_ADAPTIVE_BATCH_ENABLED=true
KAFKA_ADAPTIVE_BATCH_MAX_BATCH_TARGET=500
KAFKA_ADAPTIVE_BATCH_BACKPRESSURE_THRESHOLD=50000
```

---

## Architecture

```
 Messages in                        Broker
     |                                ^
     v                                |
 +--------+    +-------------------+  |
 | Input  |--->| collectBatch()    |  |
 | Channel|    | (linger + drain)  |  |
 +--------+    +-------------------+  |
                    |                  |
                    v                  |
               +---------+  Produce() |  Callback
               | franz-go |---------->|--------+
               | client   |           |        |
               +---------+            |        v
                                      |  RecordSendDuration()
                                      |        |
                                      |        v
                                      |  +------------------+
                                      |  | AdaptiveBatch    |
                                      |  | Controller       |
                                      |  |                  |
                                      |  | EMA tracking     |
                                      |  | State machine    |
                                      |  | Parameter tuning |
                                      |  +------------------+
                                      |        |
                                      |        v
                                      |  GetLingerDelay()
                                      |  GetBatchTarget()
                                      |  (fed back to collectBatch)
```

**Key points:**

- `collectBatch()` blocks on the first message, sleeps for `lingerDelay`, then non-blocking drains up to `batchTarget`
- The franz-go client handles the actual network batching and compression
- The controller runs **entirely in the produce callback goroutine** -- no background threads
- When disabled (`Enabled: false`), `NewAdaptiveBatchController` returns `nil` and the producer uses the original `produceStandard()` code path with **zero overhead**

---

## Frequently Asked Questions

**Q: Does enabling adaptive batching add latency when the network is healthy?**
No. When unconstrained, `MinLingerDelay` defaults to 0 and `MinBatchTarget` defaults to 1, so the producer behaves identically to the standard path.

**Q: Can the controller oscillate rapidly between constrained and normal?**
The hysteresis gap between `ConstraintThreshold` (3.0x) and `RecoveryThreshold` (1.5x) prevents this. The EMA smoothing also dampens transient spikes.

**Q: Does this replace the franz-go linger/batch settings?**
No. The adaptive batching works **on top of** franz-go's internal batching. The franz-go `ProducerLinger` and `ProducerBatchMaxBytes` settings (configured via `FlushFrequency` and `FlushBytes`) still apply. The adaptive layer accumulates messages at the application level before submitting them to franz-go.

**Q: What happens during the baseline window?**
The producer operates normally with no batching adjustments. Constraint detection begins only after the baseline is established.

**Q: Is this safe for ordered topics?**
Yes. Message ordering within a partition is preserved. The adaptive layer only changes *when* messages are submitted, not *how* they are ordered.

**Q: How do I know if it's working?**
Check the `teranode_kafka_producer_adaptive_batch_constrained` Prometheus metric or look for `[adaptive-batch]` log lines at INFO/WARN level.
