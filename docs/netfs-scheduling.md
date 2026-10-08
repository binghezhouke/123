# Remote download scheduling

The storage cache owns one download scheduler shared by every `Remote` created
for that cache. It bounds HTTP range responses across ordinary reads, image
prefetch, and archive scans. The byte counter describes expected response bytes
whose HTTP bodies are still open. It is not a heap, cache-disk, or bandwidth
limit.

The initial defaults are 32 simultaneous requests, 128 MiB of in-flight
response bytes, and 16 MiB reserved for foreground work. These are conservative
starting values, not measured optima. Thirty-two small requests can run
concurrently; larger requests remain constrained by the shared byte budget.
The existing 16 MiB per-file read-ahead therefore remains one request in the
usual case. A response larger than a request's class limit is split into
sequential HTTP ranges; a background request is at most the 112 MiB share left
after the foreground reservation. Splitting does not change the atomic cache
publication: the complete range is verified before it becomes visible.

Callers can tune the scheduler with `storage.NewCacheWithDownloadConfig` or
`storage.NewEphemeralCacheWithDownloadConfig`. The mount command exposes
`-download-requests`, `-download-bytes-mib`, and
`-download-foreground-reserve-mib`. A value of zero in the Go config selects
defaults when the whole config is zero; an explicit zero foreground reserve
disables the byte reservation. The reserve must be smaller than the total
budget.

Background acquisitions leave one request slot for foreground work when the
configured maximum is at least two (with one background slot allowed when the
maximum is one) and use only the byte budget after the foreground reserve.
Requests queue round-robin by remote file, so one archive scan cannot put all
other files behind its own queue. Foreground requests take precedence. After
three foreground grants, a queued background request gets the next turn; if
the byte budget is not yet available for that request, new foreground
admissions wait while existing responses release their reservations. This
lets large queued reads make progress under continuous small foreground load.
The request-count limit and byte limit are acquired together and released
together when the response body closes or a request fails.

Overlapping reads join one range flight. The flight has an independent context
and reference count: canceling one reader leaves the HTTP transfer alive while
another reader still needs it; the last departing reader cancels it. A
foreground join promotes queued work and subsequent ranges on that flight. An
HTTP request already sent cannot be reprioritized. `Cache.Close` cancels active
flights and waits for their cleanup before releasing the cache directory.

`Cache.DownloadStats()` reports current and peak scheduler use, queue counts,
promotions, configured limits, and available background bytes. `Cache.IOStats()`
reports bounded aggregate timing and byte counters. The counters distinguish
network response bytes from completed-cache hits; neither describes buffered
RAM or pending disk writes. The stats are intended for operational feedback
and later tuning, not as proof that the defaults are optimal.
