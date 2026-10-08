# Read-only optical image directories

ISO files in cloud directories appear as directories by default. `cd image.iso/`
then uses the existing mount's FUSE nodes, without loop devices, root access or
an additional mount per image. `-iso-dirs=false` preserves the raw ISO file;
this switch is independent of `-zip-dirs`. Recognition is lazy: a cloud listing
marks `.iso` (case insensitive), while entering it probes the image contents.

The first implementation covers cloud ISO files. Images or archives contained
inside an ISO remain ordinary files; this does not add recursive image/archive
expansion. All exposed modes are directory 0555 or regular file 0444, and the
existing mount and node methods reject writes with EROFS.

## Reader and cache design

`internal/discimage` is the read-only adapter. It uses pinned versions of
[go-diskfs](https://github.com/diskfs/go-diskfs/tree/864d5cbcf23237ff311e9735d8c43caf42ce61b0/filesystem/iso9660)
for ISO9660/Joliet/Rock Ridge and
[golift/udf](https://github.com/golift/udf/tree/4fae2a5ff797070a6fa5434cad55f0964ba117e6)
for UDF. Both consume `io.ReaderAt`; no extraction process or writable backend
is exposed. The UDF dependency requires Go 1.26, so the module's minimum Go
version increases accordingly. No Cgo or host optical-filesystem module is
required.

The UDF dependency has a small local fork under `mount123/third_party/udf`,
with its BSD license and source commit retained. A real Blu-ray image exceeded
the upstream limit of 16 nested allocation-descriptor blocks. The fork expands
these iteratively, preserving file order and holes, with cycle detection and
limits of 16 MiB of chain reads, 65,536 descriptor blocks and 1,048,576 output
runs. It avoids a recursion-depth limit on otherwise valid fragmented files.

On a bridge image, UDF takes precedence. An identified UDF filesystem that fails
to parse is reported as an error, rather than falling back to an ISO9660 view
that can have incomplete large-file sizes. Ordinary ISO files use section
readers over their physical extents, bypassing the library's mutable file
cursor. The UDF reader translates allocation extents, including metadata
partition mappings, fragmentation and zero-filled holes.

Only the requested directory and its path ancestors are parsed; there is no
whole-tree scan. Frozen per-directory snapshots reuse the mount's bounded
metadata LRU, single-flight build gate and directory-handle pin budget. Keys
include the immutable source identity and the internal directory path. Source
Range data uses the existing global disk cache and download scheduler, so it
can be reused after restart. Parsed directory snapshots are currently in memory;
this implementation does not persist a separate optical directory index.

Each metadata operation and file open gets its own parser; mutable UDF FileEntry
state is never shared between directory snapshots or file handles. Metadata
parsing is bounded by a 64 MiB read budget and a 32 MiB directory size limit,
with existing entry/name/depth budgets applied to returned snapshots. These
limits concern metadata, not video or image file size. Inodes retain small path
and size records, not an entire parser or recursively expanded tree.

File handles serialize their own request-context changes through a cancelable
gate. Different handles read concurrently. The consumer-driven read-ahead
controller works in member-relative offsets, and the optical reader translates
these to physical Range reads. Background jobs use independent parsers and
bounded copy buffers; they wait for each physical range to finish publishing
before reading the cached bytes. This avoids canceling a shared background fill
after consuming only its first bytes. Foreground reads retain progressive Range
delivery. Adjacent-image prefetch uses the existing directory and file interface.

## Format boundaries

- ISO9660 uses 2048-byte logical blocks, including Joliet Unicode names and
  ordinary Rock Ridge names and directories. Multi-extent ISO Level 3 files,
  interleaved data, and Rock Ridge SF/zisofs ZF extents currently return
  EOPNOTSUPP instead of being silently treated as contiguous files. A UDF bridge
  with large or fragmented files can use its supported UDF layout.
- UDF support includes physical partitions and the library's UDF 2.50/2.60
  metadata partition layout, extended file entries and allocation descriptor
  chains. Regular files and UDF real-time files (type 249, commonly used for
  Blu-ray M2TS/SSIF) are exposed as readable files. Virtual/sparable partitions
  are not implemented by the selected
  library. Its metadata mirror fallback is limited to opening the metadata file
  entry; it does not recover every failed primary metadata block.
- Symbolic links and special device entries are omitted. This is a data browser,
  and does not expose device nodes, emulate a bootable disc or decrypt DVD/Blu-ray
  content protection. Ordinary reads return the bytes stored in the image.

Fixtures and reproduction instructions are in
`mount123/internal/mountfs/testdata/disc/`. Public Node/handle and actual FUSE
tests cover names, directory attributes, read-only behavior, seek/EOF, shared
Range reuse, cancellation and prefetch publication. A larger image places its
video beyond the initial cache window and verifies that listing avoids that
payload extent. These are local correctness checks, not network throughput
claims.

Real-cloud validation also read the last 4 KiB of a 9,829,656,576-byte M2TS file
inside a 14,328,528,896-byte UDF 2.50 image. The cold temporary cache downloaded
2,097,153 bytes including metadata and the source probe; no complete image or
member was downloaded. See `mount123/live-validation.md` for the scope and
checks. This verifies random access to that layout, not full movie playback or
support for every UDF variant.
