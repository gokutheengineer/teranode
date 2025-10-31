package subtreevalidation

import (
	"sync"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/ordishs/go-utils/expiringmap"
)

// Orphanage manages orphaned transactions that are missing their parent transactions.
// It provides a size-limited storage mechanism with TTL-based expiration.
type Orphanage struct {
	// map stores the orphaned transactions with TTL support
	map_ *expiringmap.ExpiringMap[chainhash.Hash, *bt.Tx]

	// maxSize is the maximum number of transactions that can be stored
	maxSize int

	// lock protects concurrent access to the orphanage
	lock sync.Mutex

	// logger handles logging operations
	logger ulogger.Logger
}

// NewOrphanage creates a new Orphanage instance with the specified configuration.
func NewOrphanage(timeout time.Duration, maxSize int, logger ulogger.Logger) *Orphanage {
	if logger == nil {
		panic("logger cannot be nil")
	}
	if maxSize <= 0 {
		panic("maxSize must be positive")
	}
	if timeout <= 0 {
		panic("timeout must be positive")
	}

	orphanage := &Orphanage{
		map_:    expiringmap.New[chainhash.Hash, *bt.Tx](timeout),
		maxSize: maxSize,
		logger:  logger,
	}

	// Set up eviction function to log when transactions expire
	orphanage.map_.WithEvictionFunction(func(hash chainhash.Hash, tx *bt.Tx) bool {
		orphanage.logger.Debugf("[Orphanage] Transaction %s expired from orphanage", hash.String())
		return false
	})

	return orphanage
}

// Set adds a transaction to the orphanage if there's space.
// Returns true if the transaction was added, false if the orphanage is full.
func (o *Orphanage) Set(txHash chainhash.Hash, tx *bt.Tx) bool {
	if tx == nil {
		o.logger.Warnf("[Orphanage] Cannot add nil transaction for hash %s", txHash.String())
		return false
	}

	o.lock.Lock()
	defer o.lock.Unlock()

	// Check if orphanage is full - if so, reject the new entry
	if o.map_.Len() >= o.maxSize {
		o.logger.Warnf("[Orphanage] Rejecting transaction %s - orphanage is full (%d/%d)",
			txHash.String(), o.map_.Len(), o.maxSize)
		return false
	}

	// Add the transaction
	o.map_.Set(txHash, tx)
	o.logger.Debugf("[Orphanage] Added transaction %s (size: %d/%d)",
		txHash.String(), o.map_.Len(), o.maxSize)

	return true
}

// Get retrieves a transaction from the orphanage.
func (o *Orphanage) Get(txHash chainhash.Hash) (*bt.Tx, bool) {
	o.lock.Lock()
	defer o.lock.Unlock()
	return o.map_.Get(txHash)
}

// Delete removes a transaction from the orphanage.
func (o *Orphanage) Delete(txHash chainhash.Hash) {
	o.lock.Lock()
	defer o.lock.Unlock()

	o.map_.Delete(txHash)
	o.logger.Debugf("[Orphanage] Removed transaction %s (size: %d/%d)",
		txHash.String(), o.map_.Len(), o.maxSize)
}

// Len returns the current number of entries in the orphanage.
func (o *Orphanage) Len() int {
	o.lock.Lock()
	defer o.lock.Unlock()
	return o.map_.Len()
}

// Items returns all transactions in the orphanage.
func (o *Orphanage) Items() []*bt.Tx {
	o.lock.Lock()
	defer o.lock.Unlock()

	items := o.map_.Items()
	// Pre-allocate slice with exact capacity to avoid reallocations
	result := make([]*bt.Tx, 0, len(items))

	for _, tx := range items {
		result = append(result, tx)
	}

	return result
}

// Cleanup logs the current orphanage status.
func (o *Orphanage) Cleanup() {
	o.lock.Lock()
	defer o.lock.Unlock()

	o.logger.Infof("[Orphanage] Cleanup: current size: %d/%d", o.map_.Len(), o.maxSize)
}

// MaxSize returns the maximum size limit of the orphanage.
func (o *Orphanage) MaxSize() int {
	return o.maxSize
}

// IsFull returns true if the orphanage is at its maximum capacity.
func (o *Orphanage) IsFull() bool {
	o.lock.Lock()
	defer o.lock.Unlock()
	return o.map_.Len() >= o.maxSize
}

// Stats returns statistics about the orphanage.
func (o *Orphanage) Stats() (currentSize, maxSize int, utilizationPercent float64) {
	o.lock.Lock()
	defer o.lock.Unlock()

	currentSize = o.map_.Len()
	maxSize = o.maxSize
	if maxSize > 0 {
		utilizationPercent = float64(currentSize) / float64(maxSize) * 100.0
	}
	return
}
