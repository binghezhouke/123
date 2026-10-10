# Cache measurements and parameter tuning

The read-only mount defaults to a 50 GiB data cache, a 24-hour cloud-directory
TTL and a six-day Remote reader reuse period. Signed URL reuse remains six
days with refresh on expiration/rejection. Content identity and byte bounds
still apply. Immutable archive indexes and optical-image directories do not
need periodic content refresh. The metadata LRU remains independently bounded;
a long TTL is not a guarantee that a directory stays resident.

These defaults favor the user's mostly immutable cloud tree. External changes
may remain invisible until TTL refresh or `mount123 refresh <cloud directory>`.
Automatic refresh serves the previous snapshot while fetching a new one.
Explicit refresh waits for a new single-level cloud listing, does not discard
file payloads, and invalidates kernel directory entries. An open directory
handle keeps its frozen view until reopened. Ordinary readdir/getattr calls
do not identify a user-requested refresh. Linux
[`statx(AT_STATX_FORCE_SYNC)`](https://man7.org/linux/man-pages/man2/statx.2.html)
synchronizes attributes, not an enumeration of child names;
[FUSE invalidation](https://libfuse.github.io/doxygen/notify__inval__entry_8c.html)
is sent by the daemon after its metadata has been refreshed.

## What is measured

`mount123 io-stats` returns one process snapshot, including current runtime
configuration. `-interval 30s -count 10` returns JSONL records with a snapshot
and a comparison to the preceding sample. The default background sampler uses
the same format in `<cache-dir>/io-stats.jsonl`, with a 30-second interval,
immediate startup and final shutdown samples. It performs no platform API calls
and no filesystem directory scan. Each file is bounded to 8 MiB with one rotated
`.1` file. The directory and files are private. Aggregates contain no names,
download URLs, contents or credentials. Statistics logs are auxiliary files,
outside the data-cache byte budget. `-stats-interval=0` disables the writer.

The `cache` snapshot includes:

- Capacity, resident and reserved bytes; unique pinned bytes; entry count;
  protected index budget and occupancy; residency/pins by retention class, and
  the same residency split by protected index family under `index_kinds`
  (`archive_index`, `directory_snapshot`, `remote_identity`, `archive_probe`,
  `unclassified_index`). Each family also carries the objects that left the
  share and why: `over_budget_demotions` for objects pushed back to ordinary
  data because the share was already taken, and `capacity_evictions` for the
  last-resort removal of a still-protected object. `-index-budget-mib` sets the
  share; the default is the cache limit divided by eight, with no 64 MiB
  ceiling, so a 50 GiB cache protects 6.25 GiB of metadata.
- Capacity evictions and bytes, by retention class. Explicit removals and
  corruption cleanup are not capacity evictions. Startup capacity trimming is
  outside operation counters.
- Fill successes/failures, ENOSPC admissions and joins of existing fills.
  Failures can include cancellation. A join includes a read joining a Range
  flight it just started; it is not a count of distinct coalesced callers.
- Foreground/background `Remote.ReadAtContext` requests, full hits, requested,
  hit and miss bytes. Valid ranges are clipped at EOF. Partial hits are the
  union of all extents already published at read start, including coverage
  beyond a gap. In-flight progressive bytes count as misses for this measure.
- Refaults: the first miss/fill request for a recently capacity-evicted cache
  object. A bounded in-memory list remembers at most 4,096 opaque cache IDs.
  Its byte counter measures the evicted objects' sizes, not new network bytes.
  Changed extent boundaries and older evictions can escape this detector; it
  is a lower-bound signal, not a complete working-set estimator.

`io-stats` also reports metadata request pressure, which is what makes a
directory scan visible without a live trace. `readdir`, `lookup`, `getattr` and
`open` each carry cumulative, foreground, background and currently-active
request counts; `index_queue` reports the shared archive-index build queue
(limit, active, background-active, waiting and granted requests), which is
where metadata requests queue when they need capacity. Interval reports turn
the cumulative counters into per-second rates for operations, the index queue
and the download queue, so a sample pair answers "is the traversal still
running?" without reading file names. `scan` reports whether sustained
foreground metadata traffic has put the mount into background load shedding,
with the observed rate, the enter/exit thresholds and the start time.

Range statistics do not cover `ReadMetadataAtContext`, `ReadRangeAtContext`,
direct decoded-member handles or reads satisfied entirely by the kernel page
cache. Some parsers use normal ReadAt internally; therefore this is a storage
API boundary, not a precise split between human content and metadata. Legacy
blocks not yet imported into the extent index are not counted as pre-existing
extent hits. The existing `cache_hit_bytes` aggregate also includes metadata
read APIs and has a different denominator. Do not divide either counter by
FUSE application bytes to claim an overall cache hit rate.

Counters reset on restart; residency is restored from the durable cache.
`started_at` identifies the session, `collected_at` timestamps the sample and
`uptime_seconds` measures its age. Interval analysis rejects session changes,
non-increasing sample time and decreasing counters. Empty ratios remain null.
Inventory scans the in-memory entry map only on a snapshot request; I/O paths
update bounded counters without a full-cache scan.

## How parameters are evaluated

Interval recommendations are advisory. This version changes the requested
capacity/TTL defaults, and does not silently rewrite concurrency or prefetch
settings. An analyzed interval normally requires at least 10 seconds, 32
foreground Range reads and 8 MiB requested; otherwise it is marked
`insufficient_samples` (or `idle` with no relevant activity).

- A full cache plus at least four refaults and 8 MiB of capacity evictions in
  the interval suggests comparing a larger `-cache-gib` on the same workload.
  A single cold traversal without refaults does not earn that recommendation.
- Any ENOSPC admission is reported even with few samples. Check large members
  and pinned files as well as total capacity; increasing the limit is not
  always the right response.
- Sequential read-ahead consumption and unused-on-close bytes retain their
  existing boundaries. Lifetime consumed / (consumed + unused-on-close) is a
  rough accounted-use ratio; consumption may precede completion, and close may
  happen in a later interval. Several waste-reporting closes (at least four),
  at least 64 MiB accounted and a ratio below 35% suggest comparing a smaller
  `-read-ahead-mib`. This does not measure adjacent-picture prefetch separately,
  and unused bytes may still be reused by another handle later.
- Mean transfer-queue wait above 5 ms with foreground transfers still queued
  suggests inspecting request, in-flight byte and staging budgets together.
  It does not automatically increase concurrency based on network speed alone.

Compare application wait time, network bytes and refaults under the same file
set/access order. Separate first read, repeat read and restart reuse. Change
one parameter per comparison and retain configuration-tagged samples. A
percentile from a cumulative histogram cannot be subtracted to get an interval
percentile; the interval report instead derives means from count/time deltas.

## Reproducible capacity experiment

`TestCacheCapacityWorkingSetBaseline` uses 24 immutable 1 MiB objects served by
a local Range HTTP server, fully reads and verifies them twice, and compares
20 MiB with 50 MiB caches. It waits for complete cache publication and excludes
the initial one-byte source probes from workload traffic. Three race-test
runs gave the same results:

| Capacity | First pass requests / bytes | Second pass requests / bytes |
| --- | --- | --- |
| 20 MiB | 24 / 25,165,824 | 24 / 25,165,824 |
| 50 MiB | 24 / 25,165,824 | 0 / 0 |

The first pass demonstrates that extra capacity does not reduce a cold scan's
traffic. The second shows the benefit when the repeated working set fits.
This scaled local model does not establish an optimal production capacity or
measure cloud throughput. Run it with:

```sh
go test -race -run '^TestCacheCapacityWorkingSetBaseline$' -v ./internal/storage
```
