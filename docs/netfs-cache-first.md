# Cached-source restoration and stage measurements

The mount can open previously cached content without first requesting a new
download URL. This is intended for the mostly immutable, read-only cloud tree.

## Identity before networking

The first source open still resolves its URL and probes byte zero. When the
source has a strong, quoted HTTP ETag, or no usable ETag and a valid HTTP
Last-Modified date, the mount saves a small identity record scoped to the
account/API, file ID, content version and size. The record retains
the original probe timestamp. Restoration uses the source TTL (normally six
days); both disk restoration and in-memory reuse respect the original expiry.
An API version containing only
`:<size>` is insufficient because it carries no content version.

The descriptor contains no URL, token or password. It uses the existing bounded
metadata-object cache, including atomic replacement and capacity accounting.
Admission failure leaves the current source usable, but loses this optimization
on the next reopen. Missing, expired, malformed or unsupported-validator records use
the normal network probe. Existing content cache keys remain compatible.

A restored source serves covered bytes immediately. A cache gap resolves a URL
only when needed and sends the saved ETag in `If-Match`, or the saved date in
`If-Unmodified-Since`; response validators, size and Content-Range are still
checked. A changed entity invalidates its
descriptor and the matching in-memory source. A subsequent open can probe a
new source; an already open handle never changes its content namespace.

Last-Modified has second-level precision and is weaker than a strong ETag.
It is accepted together with the API content version for this mostly immutable
read-only workload; real 123 CDN endpoints can omit ETag. An endpoint with
neither supported validator retains the eager-probe behavior.

Local hits intentionally do not revalidate cloud permissions or external
changes. Visibility follows the directory snapshot and source-identity TTLs.
This is not a complete offline mount: nonzero roots still require Detail,
missing directory/index/data records need networking, and encrypted archives
still require their password sidecar. A descriptor is not a password cache.
Prefetch can also request uncached bytes even when a foreground read hits.

## Stage statistics

`io-stats`, its interval output and the periodic JSONL log contain a fixed
`stages` object, guarded by `stage_schema_version`:

| Field | Measured work |
| --- | --- |
| `directory_lookup` | Preparing a cloud-directory snapshot, including local restoration or a cold List |
| `source_prepare` | Source lookup/construction, including cached identity restoration |
| `url_resolve` | Calling the URL resolver; a reused in-memory direct link creates no sample |
| `source_probe` | Initial HTTP source probe, including its bounded recovery |
| `build_queue` | Waiting for a metadata/decompression build slot |
| `archive_index` | Archive index preparation in the instrumented ZIP/7z/RAR paths |
| `decompression` | Member decryption/decompression after acquiring a build slot, including required reads |
| `file_open` | Foreground FUSE Open, including any work it waits for |

These are inclusive, overlapping durations. Do not add them to calculate a
request's latency or interpret decompression duration as CPU time alone. The
existing HTTP queue, body-first-byte, transfer and cache-publication metrics
remain available. No stage samples means `unknown`, not a measured zero.
Interval reports use count/time deltas and means, not percentile subtraction.
Older logs without the schema marker remain unknown for new measurements.

Cache `fill_failures` retains its previous meaning. With
`fill_outcome_version=1`, `fill_cancelled` counts those failures caused by
`context.Canceled`; `fill_errors` counts the remainder, including deadlines.
These classify the existing bounded-object fill counters; they are not a count
of every failed FUSE request or every growing-member decode task.

The `image_prefetch` object reports bounded planner/task events, not byte-cache
hit rates. `foreground_ready` means a target appears in the recent completion
history, not proof that its data survived later cache eviction;
`foreground_in_flight` means a task for that target existed at foreground Open.
When the image tracker is enabled, zero events are measured zeros. When it is
disabled, the group is unknown. See `design/mount-prefetch.md` for lifecycle and
prediction rules.
