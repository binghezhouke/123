package mountfs

import (
	"archive/zip"
	"compress/flate"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/storage"
	yzip "github.com/yeka/zip"
	"golang.org/x/crypto/pbkdf2"
)

const maxPasswordBytes = 4096

var ErrWrongZIPPassword = errors.New("wrong ZIP password or encrypted data authentication failed")

type aesMemberInfo struct {
	version  uint16
	strength byte
	method   uint16
}

// parseAESExtra validates the WinZip AES parameters before decryption while
// preserving archive/zip's tolerance of padding in unrelated extra fields.
func parseAESExtra(extra []byte) (aesMemberInfo, bool, error) {
	var result aesMemberInfo
	found := false
	for len(extra) > 0 {
		if len(extra) < 4 {
			if len(extra) >= 2 && binary.LittleEndian.Uint16(extra) == 0x9901 {
				return result, false, errors.New("malformed WinZip AES extra field")
			}
			break // Go's archive/zip accepts harmless trailing padding bytes.
		}
		id, n := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if n > len(extra) {
			if id == 0x9901 {
				return result, false, errors.New("malformed WinZip AES extra field length")
			}
			break
		}
		data := extra[:n]
		extra = extra[n:]
		if id != 0x9901 {
			continue
		}
		if found || len(data) != 7 {
			return result, false, errors.New("malformed WinZip AES extra field")
		}
		found = true
		result.version = binary.LittleEndian.Uint16(data[:2])
		if string(data[2:4]) != "AE" {
			return result, false, errors.New("invalid WinZip AES vendor")
		}
		result.strength = data[4]
		result.method = binary.LittleEndian.Uint16(data[5:7])
		if (result.version != 1 && result.version != 2) || result.strength < 1 || result.strength > 3 || (result.method != zip.Store && result.method != zip.Deflate) {
			return result, false, errors.New("unsupported WinZip AES parameters")
		}
	}
	return result, found, nil
}

func normalizedPassword(raw []byte) ([]byte, error) {
	if len(raw) > maxPasswordBytes {
		return nil, syscall.EACCES
	}
	if len(raw) >= 2 && raw[len(raw)-2] == '\r' && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-2]
	} else if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
	}
	if !utf8.Valid(raw) {
		return nil, syscall.EACCES
	}
	return append([]byte(nil), raw...), nil
}

func (t *Tree) encryptPassword(key string, password []byte) (protectedPassword, error) {
	if !t.passwordKeyValid {
		return protectedPassword{}, errors.New("mount password protection unavailable")
	}
	block, err := aes.NewCipher(t.passwordKey[:])
	if err != nil {
		return protectedPassword{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return protectedPassword{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return protectedPassword{}, err
	}
	return protectedPassword{nonce: nonce, ciphertext: gcm.Seal(nil, nonce, password, []byte(key))}, nil
}

func (t *Tree) decryptPassword(key string, value protectedPassword) ([]byte, error) {
	block, err := aes.NewCipher(t.passwordKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, value.nonce, value.ciphertext, []byte(key))
}

func (t *Tree) passwordForArchive(ctx context.Context, archive *archiveDescriptor) ([]byte, error) {
	password, err := t.archivePassword(ctx, archive)
	if err == nil && len(password) == 0 {
		return nil, syscall.EACCES
	}
	return password, err
}

func (t *Tree) archivePassword(ctx context.Context, archive *archiveDescriptor) ([]byte, error) {
	if archive == nil || archive.id == 0 {
		return nil, syscall.EACCES
	}
	api, ok := t.api.(PasswordAPI)
	if !ok {
		return nil, syscall.EACCES
	}
	found, key, err := t.findArchivePassword(ctx, archive)
	if err != nil {
		return nil, err
	}
	value, err := t.loadRefreshingMeta(ctx, key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		if found == nil {
			protected, e := t.encryptPassword(key, nil)
			return protected, int64(4096 + len(key)), e
		}
		if found.size <= 0 || found.size > maxPasswordBytes {
			return nil, 0, syscall.EACCES
		}
		raw, e := api.ReadSmallFile(ctx, found.id, maxPasswordBytes)
		if e != nil {
			return nil, 0, e
		}
		defer clear(raw)
		password, e := normalizedPassword(raw)
		if e != nil || len(password) == 0 {
			return nil, 0, syscall.EACCES
		}
		defer clear(password)
		protected, e := t.encryptPassword(key, password)
		if e != nil {
			return nil, 0, e
		}
		return protected, int64(4096 + len(key) + len(protected.ciphertext)), nil
	})
	if err != nil {
		return nil, err
	}
	protected, ok := value.(protectedPassword)
	if !ok {
		return nil, syscall.EACCES
	}
	password, err := t.decryptPassword(key, protected)
	if err != nil {
		return nil, syscall.EACCES
	}
	return password, nil
}

type archivePasswordFile struct {
	id, size int64
	version  string
}

// findArchivePassword checks the archive sidecar in its own directory, then
// walks parent directories for the nearest .mount123.pwd file. Directory
// snapshots are shared through cloudDirectory, so repeated lookups are cheap.
func (t *Tree) findArchivePassword(ctx context.Context, archive *archiveDescriptor) (*archivePasswordFile, string, error) {
	dirID := archive.parentID
	var ownName = archive.name + ".pwd"
	if strings.HasSuffix(strings.ToLower(archive.name), ".7z.001") {
		ownName = archive.name[:len(archive.name)-4] + ".pwd"
	}
	for level := 0; level < 128; level++ {
		directory, err := t.cloudDirectory(ctx, dirID)
		if err != nil {
			return nil, "", err
		}
		var shared *archivePasswordFile
		var own *archivePasswordFile
		for _, f := range directory.files {
			if f.IsDir {
				continue
			}
			if f.Name == ownName && level == 0 {
				if own != nil {
					return nil, "", syscall.EACCES
				}
				own = &archivePasswordFile{f.ID, f.Size, f.Version}
			}
			if f.Name == ".mount123.pwd" {
				if shared != nil {
					return nil, "", syscall.EACCES
				}
				shared = &archivePasswordFile{f.ID, f.Size, f.Version}
			}
		}
		if own != nil {
			return own, fmt.Sprintf("password:%d:%s:%d:%d:%s:sidecar:%d:%s", archive.id, archive.version, archive.size, dirID, archive.name, own.id, own.version), nil
		}
		if shared != nil {
			return shared, fmt.Sprintf("password:%d:%s:%d:shared:%d:%s:g%d", archive.id, archive.version, archive.size, shared.id, shared.version, directory.generation), nil
		}
		if dirID == 0 {
			break
		}
		parent, err := t.detailCached(ctx, dirID)
		if errors.Is(err, syscall.EOPNOTSUPP) {
			break
		}
		if err != nil || parent.ParentID == dirID {
			break
		}
		dirID = parent.ParentID
	}
	key := fmt.Sprintf("password:%d:%s:%d:missing", archive.id, archive.version, archive.size)
	return nil, key, nil
}

func (t *Tree) diskCacheScope() string {
	if t.cache == nil {
		return t.cacheScope
	}
	return t.cache.StableDigest("account-api-v1", t.cacheScope)
}

func (t *Tree) passwordTag(archive *archiveDescriptor, password []byte) string {
	identity := t.cacheScope
	if archive != nil {
		identity += fmt.Sprintf("\x00%d\x00%s\x00%d", archive.id, archive.version, archive.size)
	}
	if t.cache != nil {
		return t.cache.StableDigest("archive-password-v1", identity+"\x00"+string(password))
	}
	// Trees without disk storage only need a process-local discriminator.
	h := hmac.New(sha256.New, t.passwordKey[:])
	_, _ = h.Write([]byte(identity))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(password)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func readAtFull(ctx context.Context, source *storage.Remote, p []byte, off int64) error {
	for len(p) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := source.ReadAtContext(ctx, p, off)
		if n > 0 {
			p = p[n:]
			off += int64(n)
		}
		if err != nil && !(err == io.EOF && len(p) == 0) {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func encryptedMember(ctx context.Context, src *storage.Remote, m *member, password []byte, dst io.Writer) error {
	if m.reader == nil {
		return errors.New("ZIP member index is unavailable")
	}
	if m.compressed > uint64(^uint64(0)>>1) || m.size > uint64(^uint64(0)>>1) {
		return syscall.EFBIG
	}
	offset, err := m.dataOffset(ctx)
	if err != nil {
		return err
	}
	if m.aes != nil {
		return decryptAESMember(ctx, src, m, *m.aes, password, offset, dst)
	}
	if m.compressed < 12 {
		return ErrWrongZIPPassword
	}
	var header [12]byte
	if err := readAtFull(ctx, src, header[:], offset); err != nil {
		return err
	}
	crypto := yzip.NewZipCrypto(password)
	plainHeader := crypto.Decrypt(header[:])
	check := byte(m.crc >> 24)
	if m.flags&8 != 0 {
		check = byte(m.modifiedTime >> 8)
	}
	if plainHeader[11] != check {
		return ErrWrongZIPPassword
	}
	compressed := io.NewSectionReader(encryptedReadAt{ctx: ctx, source: src}, offset+12, int64(m.compressed)-12)
	decrypted := &zipCryptoReader{ctx: ctx, r: compressed, z: crypto}
	var plain io.Reader = decrypted
	var closer io.Closer
	if m.method == zip.Deflate {
		zr := flate.NewReader(decrypted)
		plain, closer = zr, zr
	} else if m.method != zip.Store {
		return syscall.EOPNOTSUPP
	}
	if closer != nil {
		defer closer.Close()
	}
	return copyAndCheckMember(ctx, plain, m, dst)
}

type zipCryptoReader struct {
	ctx context.Context
	r   io.Reader
	z   *yzip.ZipCrypto
}

func (r *zipCryptoReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if n > 0 {
		copy(p[:n], r.z.Decrypt(p[:n]))
	}
	return n, err
}

func decryptAESMember(ctx context.Context, src *storage.Remote, m *member, info aesMemberInfo, password []byte, offset int64, dst io.Writer) error {
	keyLen := 16
	if info.strength == 2 {
		keyLen = 24
	}
	if info.strength == 3 {
		keyLen = 32
	}
	saltLen := keyLen / 2
	overhead := saltLen + 2 + 10
	if m.compressed < uint64(overhead) {
		return ErrWrongZIPPassword
	}
	var prefix [34]byte
	if err := readAtFull(ctx, src, prefix[:saltLen+2], offset); err != nil {
		return err
	}
	derived := pbkdf2.Key(password, prefix[:saltLen], 1000, 2*keyLen+2, sha1.New)
	if subtle.ConstantTimeCompare(derived[2*keyLen:], prefix[saltLen:saltLen+2]) != 1 {
		return ErrWrongZIPPassword
	}
	cipherLen := int64(m.compressed) - int64(overhead)
	block, err := aes.NewCipher(derived[:keyLen])
	if err != nil {
		return err
	}
	stream := newWinZipCTR(block)
	mac := hmac.New(sha1.New, derived[keyLen:2*keyLen])
	ciphertext := io.NewSectionReader(encryptedReadAt{ctx: ctx, source: src}, offset+int64(saltLen+2), cipherLen)
	tee := &hmacReader{ctx: ctx, r: ciphertext, mac: mac}
	plain := &aesCTRReader{ctx: ctx, r: tee, stream: stream}
	var decoded io.Reader = plain
	var closer io.Closer
	if info.method == zip.Deflate {
		zr := flate.NewReader(plain)
		decoded, closer = zr, zr
	} else if info.method != zip.Store {
		return syscall.EOPNOTSUPP
	}
	if closer != nil {
		defer closer.Close()
	}
	if err := copyAndCheckMember(ctx, decoded, m, dst); err != nil {
		return err
	}
	// Deflate may stop before the section ends. Drain the unread ciphertext so
	// the MAC authenticates every byte that belongs to this member.
	if _, err := io.Copy(io.Discard, &contextReader{ctx: ctx, r: tee}); err != nil {
		return err
	}
	var auth [10]byte
	if err := readAtFull(ctx, src, auth[:], offset+int64(m.compressed)-10); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(mac.Sum(nil)[:10], auth[:]) != 1 {
		return ErrWrongZIPPassword
	}
	return ctx.Err()
}

type aesCTR struct {
	block   cipher.Block
	counter [16]byte
	stream  [16]byte
	used    int
}

func newWinZipCTR(block cipher.Block) *aesCTR {
	c := &aesCTR{block: block, used: 16}
	c.counter[0] = 1
	return c
}
func (c *aesCTR) XORKeyStream(dst, src []byte) {
	for i := range src {
		if c.used == 16 {
			c.block.Encrypt(c.stream[:], c.counter[:])
			for j := 0; j < len(c.counter); j++ {
				c.counter[j]++
				if c.counter[j] != 0 {
					break
				}
			}
			c.used = 0
		}
		dst[i] = src[i] ^ c.stream[c.used]
		c.used++
	}
}

type aesCTRReader struct {
	ctx    context.Context
	r      io.Reader
	stream *aesCTR
}

type hmacReader struct {
	ctx context.Context
	r   io.Reader
	mac hash.Hash
}

func (r *hmacReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if n > 0 {
		_, _ = r.mac.Write(p[:n])
	}
	return n, err
}

type encryptedReadAt struct {
	ctx    context.Context
	source *storage.Remote
}
type encryptedSourceError struct{ err error }

func (e encryptedSourceError) Error() string { return e.err.Error() }
func (e encryptedSourceError) Unwrap() error { return e.err }
func (r encryptedReadAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.source.ReadAtContext(r.ctx, p, off)
	if err != nil && err != io.EOF {
		return n, encryptedSourceError{err}
	}
	return n, err
}

func (r *aesCTRReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := r.r.Read(p)
	if n > 0 {
		tmp := make([]byte, n)
		r.stream.XORKeyStream(tmp, p[:n])
		copy(p[:n], tmp)
	}
	return n, e
}

func copyAndCheckMember(ctx context.Context, r io.Reader, m *member, dst io.Writer) error {
	crc := crc32.NewIEEE()
	limited := io.LimitReader(&contextReader{ctx: ctx, r: r}, int64(m.size)+1)
	buf := make([]byte, 64<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, readErr := limited.Read(buf)
		if got > 0 {
			written, writeErr := dst.Write(buf[:got])
			if writeErr != nil {
				return writeErr
			}
			if written != got {
				return io.ErrShortWrite
			}
			_, _ = crc.Write(buf[:got])
			n += int64(got)
			if n > int64(m.size) {
				return ErrWrongZIPPassword
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var sourceErr encryptedSourceError
			if errors.As(readErr, &sourceErr) {
				return sourceErr.err
			}
			return ErrWrongZIPPassword
		}
	}
	if n != int64(m.size) {
		return ErrWrongZIPPassword
	}
	if m.aes == nil || m.aes.version == 1 {
		if crc.Sum32() != m.crc {
			return ErrWrongZIPPassword
		}
	}
	return ctx.Err()
}

// ValidateZIPPassword verifies the password against the smallest encrypted
// member and returns only after its complete size, CRC/HMAC, and stream checks.
func ValidateZIPPassword(ctx context.Context, source *storage.Remote, size int64, password []byte) error {
	return validateZIPPassword(ctx, source, size, password, 0)
}

func validateZIPPassword(ctx context.Context, source *storage.Remote, size int64, password []byte, limit uint64) error {
	if len(password) == 0 || len(password) > maxPasswordBytes || !utf8.Valid(password) {
		return syscall.EACCES
	}
	idx, err := buildZIP(ctx, &Tree{opts: defaults(Options{MetadataBytes: 64 << 20})}, source, size)
	if err != nil {
		return err
	}
	var selected *member
	for _, m := range idx.members {
		if !m.encrypted {
			continue
		}
		if selected == nil || m.size < selected.size {
			selected = m
		}
	}
	if selected == nil {
		return errors.New("archive has no supported encrypted member")
	}
	if limit > 0 && (selected.size > limit || selected.compressed > limit) {
		return syscall.EFBIG
	}
	var discard io.Writer = io.Discard
	return encryptedMember(ctx, source, selected, password, discard)
}

func isPasswordError(err error) bool {
	return errors.Is(err, ErrWrongZIPPassword) || errors.Is(err, yzip.ErrPassword) || errors.Is(err, yzip.ErrAuthentication) || errors.Is(err, yzip.ErrChecksum)
}
