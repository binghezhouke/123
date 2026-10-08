Based on github.com/nwaples/rardecode/v2 v2.2.1, with its original BSD license.
The only upstream source addition is walk.go:
- Walk / WalkMembers visit packed headers without constructing member decoders.
- MemberLocator stores only a header offset and flags, no password/key/reader state.
- OpenMember reparses the archive prefix and selected header using the current
  password, then opens an independent member directly with normal checksum checks.
  Solid members retain the sequential extraction path.
Keep the upstream files unchanged when updating.
