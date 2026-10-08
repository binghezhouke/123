// Package discimage reads optical images without materializing their contents.
// Each FS belongs to one metadata operation or file handle. Returned file
// readers support ReadAt; no shared file cursor is used for payload reads.
package discimage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"golift.io/udf"
)

const sectorSize = 2048
const maxDirectoryBytes = 32 << 20
const maxMetadataBytes = 64 << 20

// UDF real-time files (type 249) are ordinary readable byte streams, commonly
// used for Blu-ray M2TS/SSIF payloads. They are not device or metadata entries.
const udfRealtimeFile = 249

var ErrFormat = errors.New("invalid optical image")

type Entry struct {
	Name    string
	Size    int64
	Dir     bool
	ModTime time.Time
}

type FS struct {
	Format string
	iso    *iso9660.FileSystem
	udf    *udf.Udf
	source *imageReader
}

// Open prefers UDF when both filesystems are present. Falling back after a UDF
// parse failure could silently expose the bridge's truncated large-file sizes.
func Open(r io.ReaderAt, size int64) (result *FS, err error) {
	defer catchMalformed(&err)
	if r == nil || size < 18*sectorSize {
		return nil, ErrFormat
	}
	source := &imageReader{ReaderAt: r, size: size, left: maxMetadataBytes}
	var primary, terminated, hasUDF bool
descriptors:
	for sector := int64(16); sector < 16+256 && (sector+1)*sectorSize <= size; sector++ {
		var block [sectorSize]byte
		if _, err := source.ReadAt(block[:], sector*sectorSize); err != nil {
			return nil, err
		}
		switch string(block[1:6]) {
		case "NSR02", "NSR03":
			hasUDF = true
			break descriptors
		case "BEA01":
			continue
		case "TEA01":
			break descriptors
		case "CD001":
			if terminated {
				break descriptors
			}
			if block[6] != 1 {
				return nil, ErrFormat
			}
			if block[0] == 255 {
				terminated = true
			}
			if block[0] != 1 && block[0] != 2 {
				continue
			}
			if block[0] == 1 {
				primary = true
			}
			if binary.LittleEndian.Uint16(block[128:]) != sectorSize {
				return nil, syscall.EOPNOTSUPP
			}
			if binary.LittleEndian.Uint32(block[132:]) > maxDirectoryBytes || binary.LittleEndian.Uint32(block[166:]) > maxDirectoryBytes {
				return nil, syscall.EFBIG
			}
			// go-diskfs computes this path-table byte offset as uint32.
			if uint64(binary.LittleEndian.Uint32(block[140:]))*sectorSize > uint64(^uint32(0)) {
				return nil, syscall.EOPNOTSUPP
			}
		default:
			// Volume recognition descriptors are consecutive. Once the
			// sequence ends, do not interpret ordinary file data as headers.
			break descriptors
		}
	}
	result = &FS{source: source}
	if hasUDF {
		result.udf, err = udf.NewUdfFromReader(source)
		result.Format = "udf"
	} else if primary && terminated {
		b := &readOnlyBackend{SectionReader: io.NewSectionReader(source, 0, size)}
		result.iso, err = iso9660.Read(b, size, 0, sectorSize)
		result.Format = "iso9660"
	} else {
		return nil, ErrFormat
	}
	source.left = -1
	if err != nil {
		return nil, err
	}
	return result, nil
}

func catchMalformed(err *error) {
	if recover() != nil {
		*err = ErrFormat
	}
}

func (f *FS) ReadDir(name string) (entries []Entry, err error) {
	defer catchMalformed(&err)
	if !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}
	f.source.left = maxMetadataBytes
	defer func() { f.source.left = -1 }()
	if f.udf != nil {
		files, e := f.udfDirectory(name)
		if e != nil {
			return nil, e
		}
		for i := range files {
			file := &files[i]
			fe, e := file.FileEntry()
			if e != nil {
				return nil, e
			}
			if fe.ICBTag == nil || (fe.ICBTag.FileType != 4 && fe.ICBTag.FileType != 5 && fe.ICBTag.FileType != udfRealtimeFile) {
				continue
			}
			if fe.InformationLength > math.MaxInt64 {
				return nil, ErrFormat
			}
			entries = append(entries, Entry{file.Name(), file.Size(), file.IsDir(), file.ModTime()})
		}
	} else {
		if err = f.validateISODirectory(name); err != nil {
			return nil, err
		}
		files, e := f.iso.ReadDir(name)
		if e != nil {
			return nil, e
		}
		for _, file := range files {
			info, e := file.Info()
			if e != nil {
				return nil, e
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				continue
			}
			entries = append(entries, Entry{info.Name(), info.Size(), info.IsDir(), info.ModTime()})
		}
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Name == "." || !fs.ValidPath(e.Name) || strings.ContainsAny(e.Name, "/\\\x00") || e.Size < 0 {
			return nil, ErrFormat
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("duplicate optical image entry: %w", syscall.EOPNOTSUPP)
		}
		seen[e.Name] = true
	}
	return entries, nil
}

func (f *FS) OpenFile(name string) (reader io.ReaderAt, err error) {
	defer catchMalformed(&err)
	if name == "." || !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}
	entries, err := f.ReadDir(path.Dir(name))
	if err != nil {
		return nil, err
	}
	var found bool
	for _, e := range entries {
		if e.Name == path.Base(name) {
			if e.Dir {
				return nil, syscall.EISDIR
			}
			found = true
			break
		}
	}
	if !found {
		return nil, fs.ErrNotExist
	}
	f.source.left = maxMetadataBytes
	defer func() { f.source.left = -1 }()
	if f.udf != nil {
		files, e := f.udfDirectory(path.Dir(name))
		if e != nil {
			return nil, e
		}
		for i := range files {
			if files[i].Name() == path.Base(name) {
				return files[i].NewReader()
			}
		}
		return nil, fs.ErrNotExist
	}
	info, err := f.iso.Stat(name)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*iso9660.StatT)
	if !ok {
		return nil, ErrFormat
	}
	offset := (int64(stat.Location) + int64(stat.ExtAttrSize)) * sectorSize
	if offset < 0 || offset > f.source.size || info.Size() > f.source.size-offset {
		return nil, ErrFormat
	}
	return io.NewSectionReader(f.source, offset, info.Size()), nil
}

func (f *FS) udfDirectory(name string) ([]udf.File, error) {
	files, err := f.udf.ReadDir(nil)
	if err != nil || name == "." {
		return files, err
	}
	for _, component := range strings.Split(name, "/") {
		var dir *udf.File
		for i := range files {
			if files[i].Name() == component {
				dir = &files[i]
				break
			}
		}
		if dir == nil {
			return nil, fs.ErrNotExist
		}
		if _, err := dir.FileEntry(); err != nil {
			return nil, err
		}
		if !dir.IsDir() {
			return nil, syscall.ENOTDIR
		}
		files, err = dir.ReadDir()
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// The library's ISO file reader assumes one contiguous extent. Reject features
// it cannot faithfully represent, rather than exposing a truncated large file.
func (f *FS) validateISODirectory(name string) error {
	info, err := f.iso.Stat(name)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*iso9660.StatT)
	if !ok || !info.IsDir() || info.Size() < 0 {
		return ErrFormat
	}
	if info.Size() > maxDirectoryBytes {
		return syscall.EFBIG
	}
	buf := make([]byte, int(info.Size()))
	off := (int64(stat.Location) + int64(stat.ExtAttrSize)) * sectorSize
	if _, err = f.source.ReadAt(buf, off); err != nil {
		return err
	}
	for pos := 0; pos < len(buf); {
		length := int(buf[pos])
		if length == 0 {
			pos = (pos/sectorSize + 1) * sectorSize
			continue
		}
		if length < 34 || length > len(buf)-pos || pos/sectorSize != (pos+length-1)/sectorSize {
			return ErrFormat
		}
		record := buf[pos : pos+length]
		if record[25]&0x80 != 0 || record[26] != 0 || record[27] != 0 {
			return syscall.EOPNOTSUPP
		}
		if int(record[32])+33 > length {
			return ErrFormat
		}
		start := (33 + int(record[32]) + 1) &^ 1
		if err := f.validateSystemUse(record[start:], 0); err != nil {
			return err
		}
		if record[25]&2 != 0 && binary.LittleEndian.Uint32(record[10:]) > maxDirectoryBytes {
			return syscall.EFBIG
		}
		pos += length
	}
	return nil
}

func (f *FS) validateSystemUse(data []byte, depth int) error {
	if depth > 16 {
		return syscall.EFBIG
	}
	for len(data) >= 4 {
		n := int(data[2])
		if n < 4 || n > len(data) {
			break
		}
		switch string(data[:2]) {
		case "SF", "ZF":
			// Rock Ridge sparse/zisofs data is not a plain contiguous extent.
			return syscall.EOPNOTSUPP
		case "CE":
			if n < 28 {
				return ErrFormat
			}
			offset := int64(binary.LittleEndian.Uint32(data[4:]))*sectorSize + int64(binary.LittleEndian.Uint32(data[12:]))
			size := int64(binary.LittleEndian.Uint32(data[20:]))
			if size > maxDirectoryBytes || size > f.source.left {
				return syscall.EFBIG
			}
			continuation := make([]byte, int(size))
			if _, err := f.source.ReadAt(continuation, offset); err != nil {
				return err
			}
			if err := f.validateSystemUse(continuation, depth+1); err != nil {
				return err
			}
		}
		data = data[n:]
	}
	return nil
}

type imageReader struct {
	io.ReaderAt
	size, left int64
}

func (r *imageReader) Size() int64 { return r.size }
func (r *imageReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > r.size || int64(len(p)) > r.size-off {
		return 0, io.ErrUnexpectedEOF
	}
	if r.left >= 0 {
		if int64(len(p)) > r.left {
			return 0, syscall.EFBIG
		}
		r.left -= int64(len(p))
	}
	n, err := r.ReaderAt.ReadAt(p, off)
	if n != len(p) && err == nil {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

type readOnlyBackend struct{ *io.SectionReader }

func (*readOnlyBackend) Close() error                            { return nil }
func (*readOnlyBackend) Sys() (*os.File, error)                  { return nil, backend.ErrNotSuitable }
func (*readOnlyBackend) Writable() (backend.WritableFile, error) { return nil, syscall.EROFS }
func (*readOnlyBackend) Path() string                            { return "" }
func (r *readOnlyBackend) Stat() (fs.FileInfo, error)            { return imageInfo{r.Size()}, nil }

type imageInfo struct{ size int64 }

func (imageInfo) Name() string       { return "image.iso" }
func (i imageInfo) Size() int64      { return i.size }
func (imageInfo) Mode() fs.FileMode  { return 0444 }
func (imageInfo) ModTime() time.Time { return time.Time{} }
func (imageInfo) IsDir() bool        { return false }
func (imageInfo) Sys() any           { return nil }
