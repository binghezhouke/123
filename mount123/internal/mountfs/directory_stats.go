package mountfs

import "sync/atomic"

// DirectoryStatsSnapshot contains mount-local aggregate counters. It contains
// no paths, account identifiers, or credentials.
type DirectoryStatsSnapshot struct {
	MemoryHits          uint64 `json:"memory_hits"`
	DiskRestores        uint64 `json:"disk_restores"`
	DiskRestoreAttempts uint64 `json:"disk_restore_attempts"`
	ListCalls           uint64 `json:"list_calls"`
	ListErrors          uint64 `json:"list_errors"`
	StaleServed         uint64 `json:"stale_served"`
	StaleFailures       uint64 `json:"stale_failures"`
	RetryBackoffs       uint64 `json:"retry_backoffs"`
	PermanentFailures   uint64 `json:"permanent_failures"`
	CorruptSnapshots    uint64 `json:"corrupt_snapshots"`
	PersistenceFailures uint64 `json:"persistence_failures"`
}

type directoryStatsCounters struct {
	memoryHits          atomic.Uint64
	diskRestores        atomic.Uint64
	diskRestoreAttempts atomic.Uint64
	listCalls           atomic.Uint64
	listErrors          atomic.Uint64
	staleServed         atomic.Uint64
	staleFailures       atomic.Uint64
	retryBackoffs       atomic.Uint64
	permanentFailures   atomic.Uint64
	corruptSnapshots    atomic.Uint64
	persistenceFailures atomic.Uint64
}

// DirectoryStats returns bounded aggregate directory-cache counters.
func (n *Node) DirectoryStats() DirectoryStatsSnapshot {
	if n == nil || n.tree == nil {
		return DirectoryStatsSnapshot{}
	}
	s := &n.tree.directoryStats
	return DirectoryStatsSnapshot{
		MemoryHits: s.memoryHits.Load(), DiskRestores: s.diskRestores.Load(),
		DiskRestoreAttempts: s.diskRestoreAttempts.Load(), ListCalls: s.listCalls.Load(),
		ListErrors: s.listErrors.Load(), StaleServed: s.staleServed.Load(),
		StaleFailures: s.staleFailures.Load(), RetryBackoffs: s.retryBackoffs.Load(),
		PermanentFailures: s.permanentFailures.Load(), CorruptSnapshots: s.corruptSnapshots.Load(),
		PersistenceFailures: s.persistenceFailures.Load(),
	}
}
