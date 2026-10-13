# RAR group cache starts at independent compressed groups

A newly scanned outer RAR index records whether its parsed group-start header can
initialize an independent compressed SolidSession. The fork derives this fact
from the actual decoder version and checks regular Mode, known positive size,
and absence of solid continuation. Group finalization propagates that fact to
every member of the group; complete and partial DTOs can retain the boolean.

Only a proven start uses `OpenSolidSessionFromContext` when filling the existing
bounded group cache. It skips preceding packed headers/volume blocks without
constructing preceding decoders or verifying their data checksums. The target
header is reparsed and checked by the fork with the current password, and its
members retain normal dictionary and checksum validation. Group identity,
password protection, budget share, retained fill lifetime and build slots are
unchanged. This does not add a checkpoint or an unbounded decoder cache.

Stored, empty or unsupported group starts retain the existing archive-start
session path. Old DTOs lack the new fact and default to that path as well: they
remain readable and are not expired or rebuilt merely for this optimization.
Consequently existing old permanent indexes do not automatically benefit.

Archive producer failures are marked separately from cache admission or
publication failures. Decoding, header, password and source failures propagate
through the shared group handle, preserving the existing error classification;
they cannot trigger a private archive-start retry even if no bytes have reached
the caller. Cache failures before output may retain the existing fallback.
Any failure after output is returned without appending a fallback stream.

Public Service regressions cover real RAR3/RAR5 two-group archives with a corrupt
preceding group, current-group CRC failure without prefix replay, concurrent
same-group readers with one build slot, a group over its cache share, stored and
empty members, and both new and legacy persisted index metadata. Parsed-header
fork tests additionally cover independent/continuing compressed members in
seven native RAR volumes and stored three-volume archives. Native RAR multipart
routing in mountfs is not added by this change; the integration tests use a
single RAR source and do not claim native multipart Service coverage.
