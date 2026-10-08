// BSD 3-Clause License
//
// Copyright (c) 2017, Vladimir Jigulin
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:
//
// 1. Redistributions of source code must retain the above copyright notice,
//    this list of conditions and the following disclaimer.
// 2. Redistributions in binary form must reproduce the above copyright notice,
//    this list of conditions and the following disclaimer in the documentation
//    and/or other materials provided with the distribution.
// 3. Neither the name of the copyright holder nor the names of its contributors
//    may be used to endorse or promote products derived from this software
//    without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
// ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
// LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
// CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
// SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
// CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
// ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

// Minimal synthetic UDF 2.50 image with a metadata partition, a two-extent
// file, and a sparse file. The descriptor-builder layout is adapted from
// golift.io/udf image_test.go (https://github.com/golift/udf/blob/4fae2a5/image_test.go).
// Run from a module with golift.io/udf available; it writes udf250-metadata.iso.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"golift.io/udf"
)

const (
	sector    = 2048
	partStart = 400
	sectors   = 512
	vds       = 32
)

type extent struct {
	length, lbn uint32
	part        uint16
}

func main() {
	img := make([]byte, sectors*sector)
	writeVRS(img)
	writeVolume(img)
	// Physical partition blocks: metadata FE=0, FSD=1, mapped root FE/data=10/11,
	// mapped split.bin FE=13, payload extents=21 and 30.
	placePart(img, 0, writeEFE(250, 0, 12*sector, []extent{{sector, 1, 0}, {9 * sector, 10, 0}, {2 * sector, 26, 0}}))
	placePart(img, 1, writeFSD(1, 1))
	root := appendFID(nil, "", 1, 1, 0x08)
	root = appendFID(root, "split.bin", 4, 1, 0)
	root = appendFID(root, "sparse.bin", 5, 1, 0)
	placePart(img, 10, writeEFE(4, 0, uint64(len(root)), []extent{{uint32(len(root)), 2, 0}}))
	placePart(img, 11, payload(root))
	placePart(img, 13, writeEFE(5, 1, 8, []extent{{4, 21, 0}, {4, 30, 0}}))
	const holeSize = 2 << 20
	placePart(img, 14, writeEFE(5, 1, holeSize+4, []extent{{uint32(2<<30) | holeSize, 0, 0}, {4, 31, 0}}))
	placePart(img, 21, payload([]byte("ABCD")))
	placePart(img, 30, payload([]byte("EFGH")))
	placePart(img, 31, payload([]byte("TAIL")))

	image, err := udf.NewUdfFromReader(bytesReaderAt(img))
	if err != nil {
		panic(fmt.Errorf("open UDF 2.50 image: %w", err))
	}
	files, err := image.ReadDir(nil)
	if err != nil {
		panic(fmt.Errorf("read root: %w", err))
	}
	for i := range files {
		if files[i].Name() != "split.bin" {
			continue
		}
		r, err := files[i].NewReader()
		if err != nil {
			panic(err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			panic(err)
		}
		if string(got) != "ABCDEFGH" {
			panic(fmt.Sprintf("got %q", got))
		}
		sparse := filesByName(image, "sparse.bin")
		sparseReader, err := sparse.NewReader()
		if err != nil {
			panic(err)
		}
		if sparseReader.Size() != holeSize+4 {
			panic(fmt.Sprintf("sparse size=%d", sparseReader.Size()))
		}
		gap := make([]byte, 4)
		if _, err := sparseReader.ReadAt(gap, holeSize-2); err != nil || string(gap) != "\x00\x00TA" {
			panic(fmt.Sprintf("sparse gap=%q err=%v", gap, err))
		}
		if err := os.WriteFile("udf250-metadata.iso", img, 0o644); err != nil {
			panic(err)
		}
		fmt.Printf("revision=%#04x size=%d bytes split=%q sparse-size=%d; wrote udf250-metadata.iso\n", image.Revision(), len(img), got, sparseReader.Size())
		return
	}
	panic("split.bin not found")
}

func filesByName(image *udf.Udf, name string) *udf.File {
	files, err := image.ReadDir(nil)
	if err != nil {
		panic(err)
	}
	for i := range files {
		if files[i].Name() == name {
			return &files[i]
		}
	}
	panic("missing " + name)
}

// bytesReaderAt wraps bytes without retaining a mutable cursor.
func bytesReaderAt(b []byte) io.ReaderAt { return readerAt(b) }

type readerAt []byte

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r)) {
		return 0, io.EOF
	}
	n := copy(p, r[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func writeVolume(img []byte) {
	place(img, vds, tagSector(1))
	place(img, vds+1, tagSector(5))
	pd := img[(vds+1)*sector:]
	binary.LittleEndian.PutUint16(pd[22:], 0)
	binary.LittleEndian.PutUint32(pd[188:], partStart)
	binary.LittleEndian.PutUint32(pd[192:], 80)
	place(img, vds+2, writeLVD())
	place(img, vds+3, tagSector(8))
	avdp := tagSector(2)
	putShortAD(avdp[16:], 4*sector, vds)
	place(img, 256, avdp)
}

func writeVRS(img []byte) {
	for i, id := range []string{"BEA01", "NSR03", "TEA01"} {
		sec := make([]byte, sector)
		copy(sec[1:6], id)
		sec[6] = 1
		place(img, uint32(16+i), sec)
	}
}

func writeLVD() []byte {
	sec := tagSector(6)
	binary.LittleEndian.PutUint32(sec[212:], sector)
	copy(sec[217:], "*OSTA UDF Compliant")
	binary.LittleEndian.PutUint16(sec[240:], 0x0250)
	putLongAD(sec[248:], sector, 0, 1)
	maps := append(type1Map(), metadataMap()...)
	binary.LittleEndian.PutUint32(sec[264:], uint32(len(maps)))
	binary.LittleEndian.PutUint32(sec[268:], 2)
	copy(sec[440:], maps)
	return sec
}

func type1Map() []byte {
	b := make([]byte, 6)
	b[0], b[1] = 1, 6
	binary.LittleEndian.PutUint16(b[2:], 1)
	return b
}
func metadataMap() []byte {
	b := make([]byte, 64)
	b[0], b[1] = 2, 64
	copy(b[5:], "*UDF Metadata Partition")
	binary.LittleEndian.PutUint16(b[36:], 1)
	binary.LittleEndian.PutUint32(b[40:], 0)
	binary.LittleEndian.PutUint32(b[44:], 0xffffffff)
	binary.LittleEndian.PutUint32(b[48:], 0xffffffff)
	binary.LittleEndian.PutUint32(b[52:], 32)
	binary.LittleEndian.PutUint16(b[56:], 1)
	return b
}

func writeFSD(rootLBN uint32, rootPart uint16) []byte {
	b := tagSector(0x100)
	putLongAD(b[400:], sector, rootLBN, rootPart)
	return b
}
func writeEFE(fileType byte, flags uint16, size uint64, ads []extent) []byte {
	b := tagSector(0x10a)
	binary.LittleEndian.PutUint16(b[20:], 4)
	b[27] = fileType
	binary.LittleEndian.PutUint16(b[34:], flags)
	binary.LittleEndian.PutUint64(b[56:], size)
	binary.LittleEndian.PutUint64(b[64:], size)
	stride := 8
	if flags&7 == 1 {
		stride = 16
	}
	binary.LittleEndian.PutUint32(b[212:], uint32(stride*len(ads)))
	for i, ad := range ads {
		if stride == 16 {
			putLongAD(b[216+stride*i:], ad.length, ad.lbn, ad.part)
		} else {
			putShortAD(b[216+stride*i:], ad.length, ad.lbn)
		}
	}
	return b
}
func appendFID(dst []byte, name string, lbn uint32, part uint16, flags byte) []byte {
	identLen := 0
	if name != "" {
		identLen = 1 + len(name)
	}
	padded := (38 + identLen + 3) &^ 3
	rec := make([]byte, padded)
	binary.LittleEndian.PutUint16(rec[0:], 0x101)
	binary.LittleEndian.PutUint16(rec[2:], 3)
	binary.LittleEndian.PutUint16(rec[16:], 1)
	rec[18] = flags
	rec[19] = byte(identLen)
	putLongAD(rec[20:], sector, lbn, part)
	if identLen > 0 {
		rec[38] = 8
		copy(rec[39:], name)
	}
	return append(dst, rec...)
}
func tagSector(id uint16) []byte {
	b := make([]byte, sector)
	binary.LittleEndian.PutUint16(b, id)
	binary.LittleEndian.PutUint16(b[2:], 3)
	return b
}
func putShortAD(b []byte, length, lbn uint32) {
	binary.LittleEndian.PutUint32(b, length)
	binary.LittleEndian.PutUint32(b[4:], lbn)
}
func putLongAD(b []byte, length, lbn uint32, part uint16) {
	binary.LittleEndian.PutUint32(b, length)
	binary.LittleEndian.PutUint32(b[4:], lbn)
	binary.LittleEndian.PutUint16(b[8:], part)
}
func payload(b []byte) []byte                    { sec := make([]byte, sector); copy(sec, b); return sec }
func place(img []byte, lbn uint32, b []byte)     { copy(img[int(lbn)*sector:], b) }
func placePart(img []byte, lbn uint32, b []byte) { place(img, partStart+lbn, b) }
