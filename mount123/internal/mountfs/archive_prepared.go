package mountfs

// Archive inputs freeze one ordered set of volumes for both index parsing and
// later member reads. The raw size never includes synthetic ZIP index bytes.
type archiveSplitKind uint8

const (
	archiveSingle archiveSplitKind = iota
	archiveByteSplit
	archiveZIPDisks
)

type archiveVolumeLayout struct {
	kind  archiveSplitKind
	sizes []int64
}

type preparedArchiveInput struct {
	input    archiveReaderAt
	size     int64
	identity string
	layout   archiveVolumeLayout
	durable  bool
}

// A hint is read from the final ZIP volume before sibling discovery. Full
// geometry is validated after the required volumes have been pinned.
type zipVolumeHint struct {
	kind        archiveSplitKind
	lastDisk    uint32
	centralDisk uint32
}
