module github.com/binghezhouke/123/mount123

go 1.26.0

require (
	github.com/bodgit/sevenzip v1.6.1
	github.com/diskfs/go-diskfs v1.9.5-0.20261007191415-864d5cbcf232
	github.com/hanwen/go-fuse/v2 v2.11.0
	github.com/nwaples/rardecode/v2 v2.2.1
	github.com/yeka/zip v0.0.0-20231116150916-03d6312748a9
	golang.org/x/crypto v0.31.0
	golang.org/x/sys v0.43.0
	golang.org/x/term v0.27.0
	golang.org/x/text v0.42.0
	golift.io/udf v0.1.1-0.20261001180926-4fae2a5ff797
)

require (
	github.com/andybalholm/brotli v1.1.1 // indirect
	github.com/bodgit/plumbing v1.3.0 // indirect
	github.com/bodgit/windows v1.0.1 // indirect
	github.com/djherbis/times v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/spf13/afero v1.11.0 // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	go4.org v0.0.0-20200411211856-f5505b9728dd // indirect
)

// Local additions: header-only walking and direct independent-member opening.
replace github.com/nwaples/rardecode/v2 => ./third_party/rardecode

// Maintained fork: owned solid-stream readers and uncompressed member offsets.
replace github.com/bodgit/sevenzip => github.com/binghezhouke/sevenzip v1.6.2-0.20261009012119-5f9a0e73a37f

// Local fix: iterative expansion of long UDF allocation-descriptor chains.
replace golift.io/udf => ./third_party/udf
