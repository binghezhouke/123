# SolidSession acceptance coverage

`solid_session_acceptance_test.go` uses the licensed upstream fixtures in
`tests/fixtures/rar`; their pinned source, hashes and expected output are in
`SOURCE.txt`. The recording `fs.FS` counts each successful `Open` and each
`Close` on the actual returned file, including files released during volume
transitions. It does not substitute a fake solid archive.

- Real RAR3/RAR5 solid members: successful completion, explicit session close,
  cancellation, failing output writer and dictionary rejection release the
  opened volume exactly once. Repeated session `Next`, `Read`, `WriteTo` and
  `Close` preserve terminal behavior without reopening volumes.
- CRC mismatch in the second real solid member: changing the declared data
  CRC and recomputing its header CRC preserves compressed bytes and solid
  dependency. Reading the member or draining it through `Next` must return
  `ErrBadFileChecksum` even with `SkipCheck`; subsequent calls retain that
  terminal error and release the volume.
- Dictionary budget: both 2048-byte solid members decode with a 2048-byte
  budget, while 2047 bytes is rejected. The upstream decoder clamps the
  declared dictionary to known member size before checking the budget, so
  these small fixtures do not prove behavior for a multi-megabyte dictionary.
- Real RAR3/RAR5 stored three-volume archives: exact bytes are independently
  generated from the recorded line format, the opened-volume snapshot is
  checked before EOF, and all three files are closed exactly once.
- Split-member failures: missing part2 retains `fs.ErrNotExist`; cancellation
  after opening part2 retains `context.Canceled`; an actual payload mutation
  in part2 retains `ErrBadFileChecksum`. All opened files are released exactly
  once before returning the error.
- A bad main-header CRC in part2 retains `ErrBadHeaderCRC` and closes both the
  previous volume and the newly opened invalid volume exactly once.
- If the filesystem `Close` returns an error, cancellation and member CRC
  errors remain the session's terminal errors. Explicit session `Close`
  reports the filesystem error once; subsequent operations report the normal
  closed-session state, and subsequent closes do not retry the file close.

The multi-volume fixtures are **not solid**. These tests prove session volume
handling, missing-volume behavior and file release; they do not prove solid
dictionary continuity across a volume boundary. Issue #86 remains open for
that acceptance gap. Existing encrypted-member tests remain applicable, but
do not supply a solid encrypted multi-volume sample.

The volume snapshot is inspected while the session is live: currently EOF
automatically closes the archive and `Volumes()` returns nil afterward. The
tests do not treat volume names as content identity. Filesystem close counts
prove release of file handles, not physical erasure of password/key bytes
from Go-managed memory. Cancellation is checked between bounded reads; these
tests do not prove interruption of a blocked underlying filesystem read.
