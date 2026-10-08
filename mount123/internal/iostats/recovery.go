package iostats

// DirectorySummary covers cloud-listing cache activity, not every FUSE lookup
// or every platform API request. Counters reset with the mount process.
type DirectorySummary struct {
	Status              string `json:"status"`
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

// RecoverySummary covers HTTP Range recovery, including source probes. It
// excludes API-client retries and contains no request identities or URLs.
type RecoverySummary struct {
	Status      string `json:"status"`
	Attempts    uint64 `json:"attempts"`
	Retries     uint64 `json:"retries"`
	Recovered   uint64 `json:"recovered"`
	Exhausted   uint64 `json:"exhausted"`
	Cancelled   uint64 `json:"cancelled"`
	SuffixBytes uint64 `json:"suffix_bytes"`
}

func directoryDelta(p, c DirectorySummary, valid *bool) DirectorySummary {
	if p.Status != "measured" || c.Status != "measured" {
		return DirectorySummary{Status: "unknown"}
	}
	return DirectorySummary{
		Status:              "measured",
		MemoryHits:          deltaUint(p.MemoryHits, c.MemoryHits, valid),
		DiskRestores:        deltaUint(p.DiskRestores, c.DiskRestores, valid),
		DiskRestoreAttempts: deltaUint(p.DiskRestoreAttempts, c.DiskRestoreAttempts, valid),
		ListCalls:           deltaUint(p.ListCalls, c.ListCalls, valid),
		ListErrors:          deltaUint(p.ListErrors, c.ListErrors, valid),
		StaleServed:         deltaUint(p.StaleServed, c.StaleServed, valid),
		StaleFailures:       deltaUint(p.StaleFailures, c.StaleFailures, valid),
		RetryBackoffs:       deltaUint(p.RetryBackoffs, c.RetryBackoffs, valid),
		PermanentFailures:   deltaUint(p.PermanentFailures, c.PermanentFailures, valid),
		CorruptSnapshots:    deltaUint(p.CorruptSnapshots, c.CorruptSnapshots, valid),
		PersistenceFailures: deltaUint(p.PersistenceFailures, c.PersistenceFailures, valid),
	}
}

func recoveryDelta(p, c RecoverySummary, valid *bool) RecoverySummary {
	if p.Status != "measured" || c.Status != "measured" {
		return RecoverySummary{Status: "unknown"}
	}
	return RecoverySummary{Status: "measured",
		Attempts:    deltaUint(p.Attempts, c.Attempts, valid),
		Retries:     deltaUint(p.Retries, c.Retries, valid),
		Recovered:   deltaUint(p.Recovered, c.Recovered, valid),
		Exhausted:   deltaUint(p.Exhausted, c.Exhausted, valid),
		Cancelled:   deltaUint(p.Cancelled, c.Cancelled, valid),
		SuffixBytes: deltaUint(p.SuffixBytes, c.SuffixBytes, valid),
	}
}
