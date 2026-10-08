# Mount recovery and path diagnosis

The first recovery iteration covers transient read failures, durable cloud
directory snapshots, and path diagnosis. The mount remains read-only. Completed
file payloads and archive indexes continue to use the existing bounded cache.

## Directory recovery

Cloud listings are stored as versioned records scoped to the account and parent
file ID. Records contain the original cloud names and fetch timestamp; mounted
aliases are rebuilt deterministically, including duplicate names. A restart
does not grant the record a new TTL. A fresh record can satisfy listing without
another List request; an expired record can be returned while one background
refresh runs. The configured directory TTL remains 24 hours by default.

Temporary network errors, deadlines, throttling and temporary service errors
preserve the old directory snapshot. Subsequent access retries after bounded
backoff rather than making every `ls` start another request. Permission failures,
missing directories and invalid metadata are treated separately. Manual
`mount123 refresh <directory>` bypasses freshness and fetches one cloud directory
level. Failed cloud fetches leave its previous snapshot usable.

Snapshot files and temporary replacement bytes count toward the same data-cache
budget as file payloads. Replacement is atomic and preserves the old disk
snapshot on failure. If persisting a successful listing fails, that listing is
still usable in memory and `persistence_failures` increases; after a restart, the
previous saved snapshot may be restored until its TTL or an explicit refresh.
Ephemeral caches do not provide cross-restart directory reuse.

This is not a complete offline filesystem. A nonzero mount root still uses
the platform detail endpoint for root validation. Creating a file's remote
reader still resolves/probes the source; uncached bytes require a network
connection. A cached directory is evidence of a previous listing, not a lease
from the cloud server.

## HTTP recovery

Read-only API and HTTP Range failures receive finite retry and time budgets.
Cancellation interrupts backoff. Permanent failures and changed content do not
get treated as temporary outages. Signed URL refresh remains available.

Each Range probe or range flight has a 20-second deadline and at most three
retries, shared with signed-URL refresh. A requested retry delay above five
seconds ends that recovery attempt. Source URL resolution is a separate API
operation. Read-only API requests allow four recovery retries within 20 seconds,
plus the existing one-time token refresh; a paginated listing can involve
multiple such requests. These bounds are not a deadline for reading an entire
file or listing every page of a large directory.

An interrupted Range response may continue from its missing suffix only while
the content validator remains the same. Already delivered bytes are not
replayed into the reader or appended twice to the cache. If identity cannot be
verified, the operation fails instead of publishing an apparently complete
range. Local cache-write errors are returned as local errors.

Network scheduler slots are released before a retry waits. Staging bytes for a
partially received response remain reserved while the prefix is retained. The
existing global foreground reserve, request cap and byte budgets still apply.

## Diagnosis

Use the existing private local control socket:

```sh
mount123 doctor /home/binghe/mnt/123/收集
mount123 doctor /home/binghe/mnt/123/收集/example.txt
mount123 doctor --retry /home/binghe/mnt/123/example.zip
```

The JSON result explains the stage, state, reason and suggested action. It can
include a cloud file ID and archive-index status. It does not return upstream
error text, signed URLs, credentials, passwords or file bytes.

Regular files receive a one-byte sample read, not a full content verification.
Creating a remote reader can additionally perform its normal one-byte probe.
Archive members are checked using index metadata without decompressing their
contents. A nested archive that would require materialization is explicitly
reported as unchecked. Archive diagnosis may start the existing asynchronous
index task; its bounded background scheduler still applies. `--retry` resets
failed, inactive archive indexing; active tasks remain shared and complete
indexes remain reusable. Refresh cloud directory metadata separately with
`mount123 refresh`.

An ISO root is checked by listing its directory metadata. Paths inside an
ISO/UDF image are currently reported as unchecked by doctor; ordinary mounted
access to those paths is unaffected.

Doctor defaults to a 25-second deadline; `-timeout` can shorten it. Pending or
unchecked reports must not be interpreted as successful content verification.

## Measurements

`io-stats` and its periodic JSONL log add two process-local aggregate groups:

- `directory_cache`: memory hits, disk restore attempts/successes, List calls
  and errors, stale responses/failures, retry backoffs, permanent failures,
  corrupt records and persistence failures. These do not count all FUSE
  lookups or all platform API requests.
- `remote_recovery`: HTTP Range attempts, retries, recovered operations,
  exhausted retries, cancellations and bytes received by suffix continuation.
  Source probes are included; platform API-client retry counters are not.

Interval reports subtract counters only within the same process session.
Directory or recovery activity does not establish an optimal data-cache size
or read-ahead window. Completed recovery tests and live verification are
recorded in `mount123/live-validation.md`.
