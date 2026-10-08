package mountfs

// ImagePrefetchStatsSnapshot contains bounded event counters for image
// prefetching. Counters describe planner and task events, not byte-cache hits.
type ImagePrefetchStatsSnapshot struct {
	Planned            uint64 `json:"planned"`
	Started            uint64 `json:"started"`
	Reused             uint64 `json:"reused"`
	Completed          uint64 `json:"completed"`
	Cancelled          uint64 `json:"cancelled"`
	Failed             uint64 `json:"failed"`
	ForegroundReady    uint64 `json:"foreground_ready"`
	ForegroundInFlight uint64 `json:"foreground_in_flight"`
}

// ImagePrefetchStats returns aggregate prefetch activity for this mount.
func (n *Node) ImagePrefetchStats() ImagePrefetchStatsSnapshot {
	if n == nil || n.tree == nil || n.tree.prefetch == nil {
		return ImagePrefetchStatsSnapshot{}
	}
	p := n.tree.prefetch
	p.statsMu.Lock()
	snapshot := p.stats
	p.statsMu.Unlock()
	return snapshot
}
