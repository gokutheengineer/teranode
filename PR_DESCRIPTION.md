# PR Title

`blockassembly: batch DiskTxMap subtree index updates`

# PR Description

## Summary
- Added `UpdateSubtreeIndexes([]chainhash.Hash, int16)` to `DiskTxMap` to batch subtree index updates.
- Grouped hashes by disk shard and flush each affected shard once before read/update writes.
- Kept `UpdateSubtreeIndex` as a compatibility wrapper over the batched API.
- Updated subtree completion paths in `SubtreeProcessor` to use batched updates instead of per-node calls.
- Added warning logs when index maintenance fails instead of silently ignoring failures.
- Added unit tests for successful batch updates and partial-failure behavior.
- Added benchmarks comparing per-node updates vs batched updates.

## Why
Completing large subtrees in disk-backed mode previously performed a synchronous flush per transaction during index updates. This created a serial O(N) flush barrier in a hot path. Batching reduces flush round trips to at most one per affected disk shard and improves subtree completion latency/throughput.

## Test Plan
- `go test ./services/blockassembly/subtreeprocessor -run 'TestDiskTxMap_UpdateSubtreeIndexes|TestDiskTxMap_UpdateSubtreeIndexes_ReturnsErrorButUpdatesExisting'`
- `go test ./services/blockassembly/subtreeprocessor -run '^$' -bench 'BenchmarkDiskTxMap_UpdateSubtreeIndex|BenchmarkDiskTxMap_UpdateSubtreeIndexes_Batched' -benchtime=1x`
