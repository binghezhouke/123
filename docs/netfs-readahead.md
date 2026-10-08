# Consumption-driven read-ahead

Sequential file read-ahead is driven by successful foreground reads on a file
handle. The controller keeps three independent facts: the consumer's forward
frontier, ranges scheduled in the current prediction generation, and bytes
currently in flight. Completed ranges remain in the same range set until the
consumer passes them, so ready-but-unconsumed bytes count toward the same
consumer-relative lead as in-flight requests.

The configured `ReadAheadMaxBytes` (CLI: `-read-ahead-mib`) caps the lead
window, up to 16 MiB. The active window starts at 1 MiB. Individual Range
requests start at 512 KiB and can grow to 4 MiB; at most two are active per
file handle. Chunk size and concurrent request count are independent. The
planner fills the first uncovered range, stopping at the next already planned
range, so a failed request leaves a retryable hole without overlapping a later
request. Failed current-generation work is retried on the next successful
foreground read; this avoids a hot retry loop. A distant seek cancels the old
generation and starts again with the initial window. Small overlapping and
nearby out-of-order reads do not discard useful work.

The controller grows the window after sustained forward consumption, and grows
request chunks when reads wait or consume scheduled data. It estimates the
forward byte rate with a small EWMA. Repeated reads with little scheduled-range
overlap and a long idle gap reduce the active window and chunk size. Planning
also caps a new chunk by the cache-wide remaining background download bytes.
When no background capacity is available, planning waits for the next
foreground read rather than polling. The existing image prefetcher is
unchanged; it retains its directional behavior and nine-file cap.

The mount `io-stats` output reports aggregate, path-free read-ahead counters:

- `read_ahead_scheduled_bytes`: bytes submitted to the shared cache.
- `read_ahead_completed_bytes`: scheduled bytes whose cache operation finished
  successfully, including work that completed after a seek cancelled its
  generation.
- `read_ahead_consumed_bytes`: unique forward overlap between successful
  foreground reads and scheduled ranges. This is a usefulness proxy, not proof
  that a background request supplied those bytes; a foreground request may
  have won the race. Repeated reads do not increase it twice.
- `read_ahead_wasted_bytes`: completed bytes less the consumed counter, floored
  at zero, recorded when a handle closes. This is a conservative aggregate
  estimate and can understate waste when scheduled-but-incomplete ranges were
  consumed or when reads are out of order.
- `read_ahead_foreground_wait`: latency distribution for successful reads
  observed by the controller; it is the total foreground read duration, not an
  isolated prefetch wait measurement.
- `foreground_read_bytes` and `foreground_read_success_time.total_nanos` provide
  application-visible bytes and cumulative successful-read time, so aggregate
  throughput can be estimated as their ratio.
- `download_scheduler`: point-in-time aggregate active and available bytes,
  request limits, and foreground/background queue lengths from the shared
  cache.

Useful-prefetch ratio can be estimated as consumed bytes divided by scheduled
bytes, and wasted-prefetch ratio as wasted bytes divided by completed bytes.
These ratios are workload diagnostics, not exact provenance accounting. Useful
throughput can be estimated as `foreground_read_bytes.bytes` divided by
`foreground_read_success_time.total_nanos`; comparing it with source downloaded
bytes reveals whether speculation is keeping ahead of consumption. The unit tests exercise a fast
source with a stalled consumer and verify that completed plus in-flight lead
stays within the active window, sequential reads hit cached prefetched data,
overlap does not trigger a seek, true seek cancels its generation, Store data
stays inside member bounds, and Close cancels blocked work. They are correctness
checks, not a real-network throughput benchmark. The scheduler's global budget
still takes precedence over each handle's local window.
