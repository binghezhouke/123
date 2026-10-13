# Nested solid 7z demand decoding

Archive content is immutable at every depth. Parsed indexes, decompressed blocks
and verified persistent checkpoints have no content TTL; capacity eviction can
remove them. Directory snapshots, password sidecars and signed download URLs
have separate mutable lifecycles. Decoder state is an in-process optimization,
not the durable identity of the content.

## Shared forward cursor

Both the web service and FUSE use the same Tree member-read path. A missing block
in an eligible nested solid 7z borrows a group cursor for that block only. Sibling
file handles can remain open without monopolizing the decoder. The group is keyed
by stable parent identity, password identity and a copied layout fingerprint.

For A, B and C in one solid group, reads of A's head, B's head and C's head decode
the required prefix once. Skipped bytes update integrity checks but are not
published as member cache blocks. Reading one head never drains the remaining
group suffix. Existing demanded blocks remain shared across handles and restarts.
A reverse cache hit uses neither a decoder nor a build slot. A reverse miss uses
the existing independent reader and preserves the forward cursor.

Admission occurs before cache flights and reader locks. One active decode lends
its build permit synchronously to ancestors, so a compressed ZIP ancestor works
with one build slot. Waiting for a group is cancellable; pool locks never cover
I/O, decoder close or recursive ancestor release. Each borrow rebinds source I/O
to its current request, so a previous request's cancellation does not poison idle
state. Cancellation during actual decoding retires that state.

## Memory and eligibility

The additional group pool has at most eight entries and 128 MiB of accounted
resident state. Idle entries have no time expiry and are evicted least recently
used under capacity pressure. Busy entries cannot be evicted. Retired entries
remain charged until closing their decoder and ancestor references finishes.
This is separate from disk capacity and the existing ZIP continuation pool, and
is not a bound on total process RSS or transient header parsing.

The first implementation admits the pinned decoder's plain, unfiltered LZMA2
layout only. It accounts for the actual dictionary, two worst-case literal
models for transitions, the packed-input buffer, copied layout, accumulated
ancestor indexes and known input buffers. Dictionary allocation is not clamped
to a small group's output size. Only clear Store/Deflate ZIP ancestry with known
raw sources qualifies. Unknown, encrypted or unsupported coder/ancestor graphs
and entries over budget retain their existing read path. This does not finish
all nested solid archive optimizations in issue #55.

## Integrity and lifecycle

The fork exposes copied stream metadata without constructing a decoder. CRC
presence is explicit, including a declared value of zero. Every consumed byte,
including discarded prefixes, contributes to member and group checksums. A
member boundary does not read later members. At actual group end the final cache
fill checks terminal EOF and group CRC before publishing. Errors, short reads,
source failures and failed checks never become successful fallback fills.

Partial blocks already cached before a later failure retain the existing
partial-range cache semantics; a cache hit is not a new full-group integrity
proof. This change does not revalidate previously persisted partial blocks.
The pool holds parent references until its stream closes. Tree cancellation
closes idle state and interrupts borrowers; active state closes on release.

### CRC metadata upgrade

A single-file 7z group with a declared group CRC inherits that value and has no
slot in the compact substream CRC table. The parser counts only explicit slots;
file headers and copied layouts use the same mapping. A declared zero CRC still
counts as present. Synthetic archives exercise both group orders, bitmap byte
boundaries and absent digests, with a 7-Zip CLI-validated fixture covering the
public Service through Store and Deflate ZIP parents.

When SubStreamsInfo is absent, each group contains one nonempty file. Empty
files and directories do not consume group ordinals. File metadata and reads use
that default mapping; inconsistent file/group counts are rejected before mapping.

Older parsers could save incorrect file CRCs even when parsing succeeded. The
`seven_crc_version` field therefore rejects older persisted 7z indexes on first
access, for both top-level and nested archives. Revision 1 corrected compact
digests; revision 2 also corrects default file/group assignments. Rebuilt indexes
are reusable across restarts. This metadata format migration neither expires archive
content nor removes ZIP/RAR indexes, source ranges or decompressed body blocks.

## Controlled acceptance

A synthetic three-member fixture has one 1 MiB-dictionary LZMA2 solid group;
each member contains 4,194,317 bytes. Through public Service listing and range
reads, with one build slot and either Store or Deflate ZIP ancestry:

- Cold A/B/C 64 KiB heads: one decoder initialization and 9,437,210 returned
  decoder bytes. Disabling admission gives three initializations and 15,728,679
  bytes for identical reads and data (40% fewer returned decoder bytes).
- Warm C/B/A heads: no additional initialization or returned decoder bytes.
- An uncached earlier range followed by a later forward range: one additional
  private initialization, with the shared forward cursor preserved.
- Returned ranges match independently generated plaintext; request contexts are
  canceled between reads. An over-budget group still returns correct data.

These are decoder output counters, not HTTP byte counts, codec-internal read
lookahead, total archive decompression or measured live-cloud latency. Tests also
cover active/idle eviction, waiter cancellation, retained parent lifetime,
shutdown, retirement accounting, explicit zero CRCs, discarded-prefix corruption,
source errors with final bytes, short bodies and failed final-block publication.
