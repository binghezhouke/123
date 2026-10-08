# Directory handle snapshots

`Node.OpendirHandle` freezes the names and lookup data used by one directory
listing. go-fuse v2.11.0 calls the handle's `FileLookuper` for READDIRPLUS, so
that callback can return the entry and inode directly without repeating the
node-level source, password, or archive-index lookup path.

Cloud directories retain the shared immutable `cloudDirectory` snapshot and
its sorted name slice. ZIP and 7z handles retain the shared completed index and
freeze the sorted names for the current directory. RAR handles copy only the
current directory's discovered names and entry records; those records refer to
immutable member metadata and the handle does not retain or copy the growing
archive index. Its EOF remains stable while scanning continues, and reopening
can see later entries. Complete RAR traversal still waits for the index as
before.

READDIRPLUS attributes come from the same entry metadata used to calculate the
stable inode. The handle bypasses optional cloud metadata refresh for those
attributes, so a refresh cannot mix new timestamps with the old name snapshot.
Normal `Node.Lookup` retains its existing refresh behavior. Directory offsets
are entry counts and support `Seekdir`; go-fuse's interrupted-read replay can
look up earlier entries from the same handle snapshot.

Open handles pin their metadata under a tree-wide budget capped by
`Options.MetadataBytes`; a shared cloud snapshot or ZIP/7z index is charged
once while any handle holds it. Per-handle sorted name arrays for ZIP/7z are
also charged. RAR handles charge the copied names and frozen map of the current
directory's entries per handle; they do not pin the growing index. `Releasedir`
drops the pin and the handle references. If simultaneously retained snapshots
exceed the budget, opening another directory returns `ENOMEM`. This bounds
snapshot retention independently of the metadata cache's TTL/LRU accounting.

This is a local metadata and FUSE-call optimization. Cloud directory snapshots
already share their List result, so directory handles do not reduce cloud List
requests. Measurements below come from the local benchmark and do not represent
cloud latency or a mounted kernel FUSE workload.

## Measurements

`BenchmarkDirectorySnapshotLookup` runs the public node lookup path and the
directory-handle `Readdirent` plus `FileLookuper` path over one cached cloud
directory. It reports runtime and allocations for 1,000 and 10,000 entries.
The tests also run 1,000 and 10,000 direct `FileLookuper` callbacks and verify
each listing uses one `List` call and zero `Infos` calls. They check that the
regular lookup path can refresh attributes and that a refresh does not alter an
already-open handle.

On the local Ryzen 5 5600G, using 10 iterations (`-benchtime=10x`), results
were:

| Entries | Path | Time per full listing | Allocated bytes | Allocations |
| ---: | --- | ---: | ---: | ---: |
| 1,000 | directory handle | 0.44 ms | 302 KB | 5,748 |
| 1,000 | node lookup | 0.79 ms | 750 KB | 10,746 |
| 10,000 | directory handle | 3.68 ms | 3.04 MB | 59,752 |
| 10,000 | node lookup | 8.43 ms | 7.52 MB | 109,750 |

These in-process timings do not include kernel FUSE calls or remote API delay.
