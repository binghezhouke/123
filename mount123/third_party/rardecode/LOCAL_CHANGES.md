Based on github.com/nwaples/rardecode/v2 v2.2.1, with its original BSD license.
The local header-walking addition is `walk.go`:
- Walk / WalkMembers visit packed headers without constructing member decoders.
- MemberLocator stores only a header offset and flags, no password/key/reader state.
- OpenMember reparses the archive prefix and selected header using the current
  password, then opens an independent member directly with normal checksum checks.
  Solid members retain the sequential extraction path.
Preserve these additions and the changes listed below when updating upstream.

`bufio.go` advances past a buffer with no RAR signature candidate. The original
`findSig` branch retained the same buffer and decremented its offset, causing
an unbounded CPU loop on self-extracting prefixes and invalid archives.
Subprocess-bounded public walk/member tests cover RAR 3/5, boundary-spanning
signatures, false candidates, and the maximum self-extracting prefix scan.

The second local addition, `solid_session.go`, exposes a serialized sequential
solid decoder session. It keeps the upstream decoder state alive between
members and is used by mount123 to build a bounded group cache; it does not
add global caching to the decoder.

`volume.go` detaches the owned file before closing it. If opening or parsing the
next volume fails, later session cleanup must not close the previous file again.
Repeated `fileVolume.Close` calls are consequently idempotent, including after a
failed transition between volumes.

`OpenSolidSessionFrom` / `OpenSolidSessionFromContext` in `solid_session.go`
start at a known archive member ordinal. They walk packed headers/volume blocks
without constructing preceding decoders, then lazily initialize only the target
compressed independent member on the first `Next`. See `SOLID_GROUP_START.md`
for target restrictions and checksum/cancellation semantics. Upstream `reader.go`,
`OpenReader`, and `OpenMember` are unchanged.

`FileHeader.CanStartSolidSession` exposes the supported independent-group-start
predicate. A private compression bit in `reader.go` is set by `volume.go` from
the parsed block's real decoder version; callers cannot infer it from names or
packed/unpacked ratios. `OpenSolidSessionFromContext` uses the same predicate.
