package storage

import (
	"strings"
)

// cacheClass controls retention priority, not admission or cache correctness.
// Classes are persisted in opaque cache filenames; no cache key is written.
type cacheClass uint8

const (
	cacheSpeculative cacheClass = iota
	cacheProbation
	cacheHot
	cacheIndex
)

const cacheClassCount = int(cacheIndex) + 1

func (c cacheClass) suffix() string {
	switch c {
	case cacheSpeculative:
		return "s"
	case cacheHot:
		return "h"
	case cacheIndex:
		return "i"
	default:
		return "p"
	}
}

func parseCacheClass(s string) (cacheClass, bool) {
	switch strings.ToLower(s) {
	case "s":
		return cacheSpeculative, true
	case "p":
		return cacheProbation, true
	case "h":
		return cacheHot, true
	case "i":
		return cacheIndex, true
	default:
		return cacheProbation, false
	}
}

// foregroundClass derives the initial retention from the operation context.
// Existing cache hits are promoted only by explicit foreground-consumption hooks.
func foregroundClass(background bool) cacheClass {
	if background {
		return cacheSpeculative
	}
	return cacheProbation
}
