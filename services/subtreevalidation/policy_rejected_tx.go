package subtreevalidation

import (
	"context"
	"sync"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"google.golang.org/protobuf/proto"
)

// txPolicyRejectedCache is a bounded in-memory cache of consensus-valid transactions
// that were rejected by local mining policy. Keyed by tx hash, storing raw *bt.Tx.
//
// The cache is populated from the KAFKA_TX_POLICY_REJECTED topic and consulted by
// subtree validation before making an HTTP request to another miner for a missing tx.
type txPolicyRejectedCache struct {
	mu      sync.RWMutex
	entries map[chainhash.Hash]*bt.Tx
	maxSize int
}

func newTxPolicyRejectedCache(maxBytes int) *txPolicyRejectedCache {
	estimatedEntries := maxBytes / 500 // average tx ~500 bytes
	if estimatedEntries < 1024 {
		estimatedEntries = 1024
	}

	return &txPolicyRejectedCache{
		entries: make(map[chainhash.Hash]*bt.Tx, estimatedEntries),
		maxSize: estimatedEntries,
	}
}

func (c *txPolicyRejectedCache) Get(hash chainhash.Hash) (*bt.Tx, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tx, ok := c.entries[hash]
	return tx, ok
}

func (c *txPolicyRejectedCache) Set(hash chainhash.Hash, tx *bt.Tx) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.maxSize {
		c.evictOldest()
	}

	c.entries[hash] = tx
}

// evictOldest removes one arbitrary entry to make room. Called under write lock.
// A random eviction is acceptable here because the cache is best-effort: misses
// just fall back to the HTTP fetch path.
func (c *txPolicyRejectedCache) evictOldest() {
	for k := range c.entries {
		delete(c.entries, k)
		return
	}
}

func (c *txPolicyRejectedCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.entries)
}

// policyRejectedTxMessageHandler returns a Kafka message handler that deserializes
// KafkaTxPolicyRejectedTopicMessage and stores the raw transaction in the cache.
func (u *Server) policyRejectedTxMessageHandler(_ context.Context) func(msg *kafka.KafkaMessage) error {
	return func(msg *kafka.KafkaMessage) error {
		if u.policyRejectedTxCache == nil {
			return nil
		}

		var m kafkamessage.KafkaTxPolicyRejectedTopicMessage
		if err := proto.Unmarshal(msg.Value, &m); err != nil {
			u.logger.Errorf("[policyRejectedTxHandler] proto unmarshal error: %v", err)
			return nil
		}

		if len(m.TxHash) != chainhash.HashSize || len(m.RawTx) == 0 {
			return nil
		}

		tx, err := bt.NewTxFromBytes(m.RawTx)
		if err != nil {
			u.logger.Errorf("[policyRejectedTxHandler] failed to parse tx from bytes: %v", err)
			return nil
		}

		var hash chainhash.Hash
		copy(hash[:], m.TxHash)

		u.policyRejectedTxCache.Set(hash, tx)

		return nil
	}
}

// lookupPolicyRejectedTxs checks the policy-rejected cache for missing transactions
// and returns any that were found, along with the hashes that are still missing.
func (u *Server) lookupPolicyRejectedTxs(missingTxHashes []missingTxHash) (found []missingTx, stillMissing []missingTxHash) {
	if u.policyRejectedTxCache == nil {
		return nil, missingTxHashes
	}

	stillMissing = make([]missingTxHash, 0, len(missingTxHashes))

	for _, mth := range missingTxHashes {
		tx, ok := u.policyRejectedTxCache.Get(mth.hash)
		if ok {
			found = append(found, missingTx{tx: tx, idx: mth.idx})
		} else {
			stillMissing = append(stillMissing, mth)
		}
	}

	return found, stillMissing
}

// missingTxHash pairs a tx hash with its index in the txMetaSlice for cache lookups.
type missingTxHash struct {
	hash chainhash.Hash
	idx  int
}
