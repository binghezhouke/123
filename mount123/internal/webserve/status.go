package webserve

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
)

// Web bytes are recorded as Write returns, including still-open video streams.
// They are application response bytes, not TCP acknowledgements or CDN traffic
// sent directly to the browser. Diagnostics/static requests are excluded.
type webTraffic struct {
	active                                            atomic.Int64
	bytes, completed, cancelled, totalNanos, maxNanos atomic.Uint64
	statuses                                          [6]atomic.Uint64
}

type webSnapshot struct {
	Active        int64  `json:"active"`
	ResponseBytes uint64 `json:"response_bytes"`
	Completed     uint64 `json:"completed"`
	Cancelled     uint64 `json:"cancelled"`
	TotalNanos    uint64 `json:"total_nanos"`
	MaxNanos      uint64 `json:"max_nanos"`
	Success       uint64 `json:"success"`
	Redirect      uint64 `json:"redirect"`
	ClientError   uint64 `json:"client_error"`
	ServerError   uint64 `json:"server_error"`
}

func (t *webTraffic) finish(status int, elapsed time.Duration, cancelled bool) {
	t.completed.Add(1)
	if cancelled {
		t.cancelled.Add(1)
	}
	if status == 0 {
		status = http.StatusOK
	}
	if class := status / 100; class >= 1 && class < len(t.statuses) {
		t.statuses[class].Add(1)
	}
	nanos := uint64(max(elapsed, 0))
	t.totalNanos.Add(nanos)
	for old := t.maxNanos.Load(); nanos > old; old = t.maxNanos.Load() {
		if t.maxNanos.CompareAndSwap(old, nanos) {
			break
		}
	}
}

func (t *webTraffic) snapshot() webSnapshot {
	return webSnapshot{Active: t.active.Load(), ResponseBytes: t.bytes.Load(), Completed: t.completed.Load(),
		Cancelled: t.cancelled.Load(), TotalNanos: t.totalNanos.Load(), MaxNanos: t.maxNanos.Load(),
		Success: t.statuses[2].Load(), Redirect: t.statuses[3].Load(), ClientError: t.statuses[4].Load(), ServerError: t.statuses[5].Load()}
}

type statusRuntime struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	BuildTime       string `json:"build_time"`
	GoVersion       string `json:"go_version"`
	Goroutines      int    `json:"goroutines"`
	HeapBytes       uint64 `json:"heap_bytes"`
	HeapSystemBytes uint64 `json:"heap_system_bytes"`
	GCCount         uint32 `json:"gc_count"`
}

type statusSnapshot struct {
	SchemaVersion         int              `json:"schema_version"`
	SampleIntervalSeconds int              `json:"sample_interval_seconds"`
	IO                    iostats.Snapshot `json:"io"`
	Web                   webSnapshot      `json:"web"`
	Runtime               statusRuntime    `json:"runtime"`
}

// Cache inventory walks the in-memory index. Share one five-second snapshot
// across all viewers so rapid polling cannot multiply that work.
type statusCache struct {
	mu   sync.Mutex
	at   time.Time
	data []byte
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "status.html", pageData{Info: s.info, Title: "运行状态"})
}

func (s *Server) handleStatusData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.service == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"统计服务不可用"}`))
		return
	}
	s.status.mu.Lock()
	now := s.now()
	if s.status.data == nil || now.Before(s.status.at) || now.Sub(s.status.at) >= 5*time.Second {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		data, err := json.Marshal(statusSnapshot{
			SchemaVersion: 1, SampleIntervalSeconds: 5,
			IO: s.service.IOStats(), Web: s.traffic.snapshot(),
			Runtime: statusRuntime{Version: s.info.Version, Commit: s.info.Commit, BuildTime: s.info.BuildTime,
				GoVersion: runtime.Version(), Goroutines: runtime.NumGoroutine(), HeapBytes: mem.HeapAlloc,
				HeapSystemBytes: mem.HeapSys, GCCount: mem.NumGC},
		})
		if err != nil {
			s.status.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"统计快照生成失败"}`))
			return
		}
		s.status.data, s.status.at = data, now
	}
	data := s.status.data
	s.status.mu.Unlock()
	_, _ = w.Write(data)
}
