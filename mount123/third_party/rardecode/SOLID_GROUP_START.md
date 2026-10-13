# Starting a solid session at an independent group

`OpenSolidSessionFrom(name, startOrdinal, opts...)` and its context variant
accept a known zero-based archive member ordinal, including directory and empty
entries in the ordinal count. The current archive headers are reparsed with the
current options/password; no persisted reader, key, decoder or checkpoint is used.

The target must have a regular header Mode, known positive unpacked size,
compression and `Solid == false`. This forces a fresh compression dictionary.
A solid continuation returns `ErrSolidOpen`. Negative ordinals and unsupported
anchors (directories, empty/unknown-size files, stored files or nonregular Mode)
return `fs.ErrInvalid`; an ordinal past the last member returns `io.EOF` and no
session. Use the original `OpenSolidSession` for complete archive traversal,
including those unsupported anchors. The original APIs retain their behavior.

Packed headers and split-volume blocks before the target are traversed without
constructing their member decoders or checking their data CRC. Their header,
password and volume errors still fail construction. A non-seekable filesystem
may require reading/discarding preceding packed bytes; skipping is not a promise
of constant input I/O. The target decoder is initialized lazily on the first
`Next`, which retains dictionary limits and per-member CRC even with `SkipCheck`.
Construction can thus succeed and the first `Next` still reject its dictionary.

Members report absolute archive `Ordinal`; `Group` begins at zero relative to
this session. Ownership and serialization are unchanged: consume/close a member
before `Next`, serialize all methods including `Close`, and close the session
when done. Constructor failures and terminal errors release every opened volume.
Closing before the first `Next` releases the primed input without decoding it.
Cancellation is checked between headers/volume blocks and decoded reads; a custom
filesystem must itself interrupt blocking I/O. `Volumes()` is a snapshot only
while the session is live, and returns nil after EOF/error/Close.

`solid_session_from_test.go` uses generated real RAR3/RAR5 two-group fixtures.
Official unrar confirms members 0/2 start groups and members 1/3 continue them.
Reading the later group succeeds after an actual payload mutation in the earlier
group (single-volume and seven-volume layouts), while normal archive traversal
rejects that mutation. Separate 98304-byte earlier / 4096-byte later group
fixtures prove a 4096-byte dictionary limit rejects initialization of the earlier
group but permits the later group, ruling out preceding decoder initialization.
Tests additionally cover invalid/continuation starts, selected member CRC,
password/dictionary policy, constructor/active cancellation, missing preceding
volumes, and exactly-once file release including Close before the first Next.
Fixture source, generator, license and independent expected-byte verification
are in `tests/fixtures/rar/GROUP_START_SOURCE.txt`.
