# Network filesystem cache retention policy

The disk cache still has one hard byte limit. Reservations count toward that limit before a fill starts, pinned entries cannot be evicted, and a fill returns `ENOSPC` if no unpinned victim can make enough room. Cache class changes do not change the byte limit or publication, cancellation, and pin semantics.

Entries use four retention classes. Eviction picks the least recently used unpinned entry from the lowest available class, in this order:

1. `speculative`: data filled by work marked `workqueue.Background`, including read-ahead and prefetch.
2. `probation`: foreground data on its first use, or data that was prefetched and later consumed.
3. `hot`: data that a reader revisits or accesses outside a single forward or reverse pass through its range.
4. `index`: complete persisted archive indexes within the protected-index share.

The first foreground use of a speculative entry promotes it to probation. A foreground fill also starts in probation. The cache tracks a small in-memory scan direction and high-water interval per entry: consecutive reads that move forward or backward keep the entry in probation; revisiting an already-consumed interval promotes it to hot. This lets a single large scan age out earlier scan data without immediately giving every range hot retention. The interval is deliberately approximate and resets after restart.

Archive indexes receive a protected share of `min(cache limit / 8, 64 MiB)`. Indexes within that share are evicted after speculative, probationary, and hot entries, but remain evictable when the overall cache needs space. An index larger than the remaining protected share is still stored as probationary data if the total cache limit permits. This keeps large indexes usable across restart without allowing them to reserve unbounded space.

The class is encoded as a one-letter suffix in the already opaque hashed cache filename (`.s`, `.p`, `.h`, or `.i`). The suffix contains no cache key, account identity, URL, or password. The in-memory scan interval is not persisted. Existing ordinary `.blob` entries and legacy range-extent names load as probationary entries and are upgraded on real use; they do not need manual removal. Archive index reads use the explicit index API so loading metadata does not make it a content hot spot.

Durable caches preserve class suffixes across process restarts. Ephemeral caches use the same admission and eviction decisions inside their per-process directory, then remove that directory on close; stale ephemeral directories are removed on later startup as before. No cache class or index-share state is stored outside the cache files.

`mount123/internal/storage/cache_policy_test.go` exercises the public `Cache`, `Remote.ReadAt`, and archive-index cache APIs with a local HTTP Range server. It warms a repeated image and a small index, scans more data than the cache can hold using 64 KiB reads, then checks that old scan data is evicted, the image and index remain reusable, capacity stays bounded, and durable restart preserves the classes. It also verifies that an index beyond the protected share still enters the ordinary cache class when the total budget allows.

The policy has no per-file hard quota. A sufficiently long sequence of repeatedly revisited data can still displace other hot entries; only small archive indexes receive a separate bounded retention tier. Cache behavior is process-local while running, so an abruptly terminated process can lose the recent in-memory scan direction, while durable class suffixes remain intact.
