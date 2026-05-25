# PR Title

`subtreeprocessor: guard newSubtreeChan sends with context cancellation`

# PR Description

## Summary
- Added context-aware helpers for subtree announcements:
  - `sendNewSubtreeRequest(ctx, req)`
  - `waitNewSubtreeRequest(ctx, errCh)`
- Updated complete-subtree and reorg announcement paths to use context-aware send/wait instead of direct blocking channel ops.
- Buffered announcement `ErrChan` (`chan error, 1`) in relevant paths to avoid result-path blocking on cancellation.
- Threaded context through internal completion paths (`addNode`, `addNodePreValidated`, `processCompleteSubtree`, and affected callers) so cancellation can break blocked announcement sends.
- Added regression tests for blocked `newSubtreeChan` + canceled context:
  - `TestProcessCompleteSubtree_ReturnsWhenAnnouncementChannelBlockedAndContextCancelled`
  - `TestAnnounceChainedSubtrees_ReturnsWhenChannelBlockedAndContextCancelled`

## Why
Direct sends to `newSubtreeChan` could block the single subtree processor goroutine when the listener was unavailable or backpressured. Because this goroutine serializes core subtree state transitions, one blocked send could wedge transaction progress or reorg completion.

## Test Plan
- `go test ./services/blockassembly/subtreeprocessor -run 'TestProcessCompleteSubtree_ReturnsWhenAnnouncementChannelBlockedAndContextCancelled|TestAnnounceChainedSubtrees_ReturnsWhenChannelBlockedAndContextCancelled|TestProcessOwnBlockSubtreeNodesParallelPath|TestProcessOwnBlockSubtreeNodesSequentialPath'`
