package storage

import "strings"

// indexKind separates the object families that share the protected index
// budget. Archive indexes, directory snapshots, remote identity descriptors and
// archive probe records are all metadata, but they fail in different ways, so
// diagnostics report their residency and eviction separately.
//
// The kind is encoded in the cache filename rather than kept in memory: after a
// restart the file name is the only thing left, and a cache that misattributes
// its protected bytes is worse than one that admits a gap.
type indexKind uint8

const (
	indexKindArchive indexKind = iota
	indexKindDirectory
	indexKindIdentity
	indexKindProbe
	// indexKindUnknown covers objects written before kinds were recorded and
	// objects whose demotion removed the family from the file name. They stay
	// inside the shared budget and are reported as unclassified instead of
	// being guessed at.
	indexKindUnknown
	indexKindCount
)

var indexKindNames = [indexKindCount]string{
	"archive_index",
	"directory_snapshot",
	"remote_identity",
	"archive_probe",
	"unclassified_index",
}

func (k indexKind) name() string {
	if k >= indexKindCount {
		return indexKindNames[indexKindUnknown]
	}
	return indexKindNames[k]
}

// suffix is a single character so a protected file name stays short. Letters
// are chosen to be distinct from the retention class letters s/p/h/i.
func (k indexKind) suffix() string {
	switch k {
	case indexKindArchive:
		return "a"
	case indexKindDirectory:
		return "d"
	case indexKindIdentity:
		return "r"
	case indexKindProbe:
		return "b"
	default:
		return "u"
	}
}

func parseIndexKind(s string) (indexKind, bool) {
	switch strings.ToLower(s) {
	case "a":
		return indexKindArchive, true
	case "d":
		return indexKindDirectory, true
	case "r":
		return indexKindIdentity, true
	case "b":
		return indexKindProbe, true
	case "u":
		return indexKindUnknown, true
	default:
		return indexKindUnknown, false
	}
}

// indexKindForKey classifies a persisted metadata object by its private key
// prefix. Unrecognized keys keep the shared protection but are reported as
// unclassified.
func indexKindForKey(key string) indexKind {
	switch {
	case strings.HasPrefix(key, "directory-snapshot:"):
		return indexKindDirectory
	case strings.HasPrefix(key, "remote-identity-v1:"):
		return indexKindIdentity
	case strings.HasPrefix(key, "archive-probes:"):
		return indexKindProbe
	case strings.HasPrefix(key, "archive-index"):
		return indexKindArchive
	default:
		return indexKindUnknown
	}
}
