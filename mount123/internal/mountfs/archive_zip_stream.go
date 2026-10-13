package mountfs

import (
	"archive/zip"
	"compress/flate"
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"syscall"

	yzip "github.com/yeka/zip"
	"golang.org/x/crypto/pbkdf2"
)

// zipStreamReaderAt preserves the operation context when the parent input is
// itself an archive member reader. Bare ReaderAt inputs still receive a context
// check before every operation.
type zipStreamReaderAt struct {
	reader  io.ReaderAt
	size    int64
	bounded bool
}

func (r zipStreamReaderAt) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	truncated := false
	if r.bounded {
		if off > r.size || (off == r.size && len(p) != 0) {
			return 0, io.EOF
		}
		if int64(len(p)) > r.size-off {
			p = p[:r.size-off]
			truncated = true
		}
	}
	var n int
	var err error
	if source, ok := r.reader.(archiveReaderAt); ok {
		n, err = source.ReadAtContext(ctx, p, off)
	} else {
		n, err = r.reader.ReadAt(p, off)
	}
	if truncated && err == nil {
		err = io.EOF
	}
	return n, err
}

// openZIPMemberStream binds local-header and payload reads to the supplied
// source, never to the zip.File reader retained by the metadata index.
func openZIPMemberStream(ctx context.Context, source archiveReaderAt, m *member, password []byte) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || m == nil {
		return nil, syscall.EINVAL
	}
	bound := *m
	bound.reader = &contextZIPReaderAt{source: source, gate: make(chan struct{}, 1)}
	offset, err := bound.dataOffset(ctx)
	if err != nil {
		return nil, err
	}
	return openZIPMemberStreamAt(ctx, source, &bound, password, offset)
}

func openZIPMemberStreamAt(ctx context.Context, source archiveReaderAt, m *member, password []byte, offset int64) (io.ReadCloser, error) {
	if m.compressed > math.MaxInt64 || m.size > math.MaxInt64 || offset < 0 || offset > math.MaxInt64-int64(m.compressed) {
		return nil, syscall.EFBIG
	}
	if m.method != zip.Store && m.method != zip.Deflate {
		return nil, syscall.EOPNOTSUPP
	}
	encrypted := m.encrypted || m.flags&1 != 0 || m.aes != nil
	var compressed io.Reader = io.NewSectionReader(encryptedReadAt{ctx: ctx, source: source}, offset, int64(m.compressed))
	var authenticate func() error
	if m.aes != nil {
		info := *m.aes
		if (info.version != 1 && info.version != 2) || info.strength < 1 || info.strength > 3 || info.method != m.method {
			return nil, syscall.EOPNOTSUPP
		}
		keyLen := 8 + int(info.strength)*8
		saltLen := keyLen / 2
		overhead := int64(saltLen + 2 + 10)
		if m.compressed < uint64(overhead) {
			return nil, ErrWrongZIPPassword
		}
		var prefix [18]byte
		if err := readAtFull(ctx, source, prefix[:saltLen+2], offset); err != nil {
			return nil, err
		}
		derived := pbkdf2.Key(password, prefix[:saltLen], 1000, 2*keyLen+2, sha1.New)
		if subtle.ConstantTimeCompare(derived[2*keyLen:], prefix[saltLen:saltLen+2]) != 1 {
			return nil, ErrWrongZIPPassword
		}
		block, err := aes.NewCipher(derived[:keyLen])
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha1.New, derived[keyLen:2*keyLen])
		ciphertext := io.NewSectionReader(encryptedReadAt{ctx: ctx, source: source}, offset+int64(saltLen+2), int64(m.compressed)-overhead)
		tee := &hmacReader{ctx: ctx, r: ciphertext, mac: mac}
		compressed = &aesCTRReader{ctx: ctx, r: tee, stream: newWinZipCTR(block)}
		authenticate = func() error {
			// Only complete decoded EOF authenticates the remaining ciphertext.
			// Close never invokes this finalizer for a partially read member.
			if _, err := io.Copy(io.Discard, tee); err != nil {
				return err
			}
			var auth [10]byte
			if err := readAtFull(ctx, source, auth[:], offset+int64(m.compressed)-10); err != nil {
				return err
			}
			if subtle.ConstantTimeCompare(mac.Sum(nil)[:10], auth[:]) != 1 {
				return ErrWrongZIPPassword
			}
			return ctx.Err()
		}
	} else if encrypted {
		if m.compressed < 12 {
			return nil, ErrWrongZIPPassword
		}
		var header [12]byte
		if err := readAtFull(ctx, source, header[:], offset); err != nil {
			return nil, err
		}
		crypto := yzip.NewZipCrypto(password)
		plainHeader := crypto.Decrypt(header[:])
		check := byte(m.crc >> 24)
		if m.flags&8 != 0 {
			check = byte(m.modifiedTime >> 8)
		}
		if plainHeader[11] != check {
			return nil, ErrWrongZIPPassword
		}
		section := io.NewSectionReader(encryptedReadAt{ctx: ctx, source: source}, offset+12, int64(m.compressed)-12)
		compressed = &zipCryptoReader{ctx: ctx, r: section, z: crypto}
	}
	var decoded io.Reader = compressed
	var closer io.Closer = io.NopCloser(compressed)
	if m.method == zip.Deflate {
		zr := flate.NewReader(compressed)
		decoded, closer = zr, zr
	}
	checked := &zipMemberChecksum{ctx: ctx, reader: decoded, hash: crc32.NewIEEE(), left: int64(m.size), want: m.crc, encrypted: encrypted, checkCRC: m.aes == nil || m.aes.version == 1, authenticate: authenticate}
	return &archiveMemberStream{ctx: ctx, reader: checked, closer: closer}, nil
}

type zipMemberChecksum struct {
	ctx          context.Context
	reader       io.Reader
	hash         hash.Hash32
	left         int64
	want         uint32
	encrypted    bool
	checkCRC     bool
	authenticate func() error
}

func (r *zipMemberChecksum) integrityError() error {
	if r.encrypted {
		return ErrWrongZIPPassword
	}
	return zip.ErrChecksum
}

func (r *zipMemberChecksum) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.left == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n != 0 {
			return 0, r.integrityError()
		}
		return 0, r.finish(err)
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	_, _ = r.hash.Write(p[:n])
	r.left -= int64(n)
	if err != nil {
		err = r.finish(err)
	}
	return n, err
}

func (r *zipMemberChecksum) finish(err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var sourceErr encryptedSourceError
	if errors.As(err, &sourceErr) {
		return sourceErr.err
	}
	if err != io.EOF {
		if r.encrypted {
			return ErrWrongZIPPassword
		}
		return err
	}
	if r.left != 0 || (r.checkCRC && r.hash.Sum32() != r.want) {
		return r.integrityError()
	}
	if r.authenticate != nil {
		if err := r.authenticate(); err != nil {
			var sourceErr encryptedSourceError
			if errors.As(err, &sourceErr) {
				return sourceErr.err
			}
			return err
		}
	}
	return io.EOF
}
