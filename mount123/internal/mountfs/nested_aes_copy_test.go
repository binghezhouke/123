package mountfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

type aesCopyRequestTag struct{}

type aesCopyTestInput struct {
	data    []byte
	mu      sync.Mutex
	tags    []int
	failure error
	started chan struct{}
	release chan struct{}
}

func (r *aesCopyTestInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	if tag, ok := ctx.Value(aesCopyRequestTag{}).(int); ok {
		r.tags = append(r.tags, tag)
	}
	failure, started, release := r.failure, r.started, r.release
	r.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-release:
		}
	}
	if failure != nil {
		return 0, failure
	}
	return bytes.NewReader(r.data).ReadAt(p, off)
}

func nestedAESCopyFixture(t *testing.T) ([]byte, *member, *zipIndex) {
	t.Helper()
	data, err := os.ReadFile("testdata/encrypted/copy.7z")
	if err != nil {
		t.Fatal(err)
	}
	members, err := indexNested7z(context.Background(), bytes.NewReader(data), int64(len(data)), []byte("mount-test-password"), defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := nestedIndexFromMembers(members, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range idx.members {
		return data, m, idx
	}
	t.Fatal("AES+Copy fixture has no member")
	return nil, nil, nil
}

func TestNestedAESCopyDirectInputRebindsRequestsAndCloses(t *testing.T) {
	data, m, _ := nestedAESCopyFixture(t)
	source := &aesCopyTestInput{data: data}
	first, cancel := context.WithCancel(context.WithValue(context.Background(), aesCopyRequestTag{}, 1))
	direct, owned, ok, err := nestedCopyMemberReader(first, source, int64(len(data)), m, m.ordinal, []byte("mount-test-password"))
	if err != nil || !ok || owned == nil {
		t.Fatalf("AES direct input=%t owned=%T err=%v", ok, owned, err)
	}
	defer owned.Close()
	cancel()
	second := context.WithValue(context.Background(), aesCopyRequestTag{}, 2)
	golden := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
	buf := make([]byte, 37)
	if n, err := direct.ReadAtContext(second, buf, 19); err != nil || n != len(buf) || !bytes.Equal(buf, golden[19:56]) {
		t.Fatalf("new request read=%d err=%v equal=%t", n, err, bytes.Equal(buf, golden[19:56]))
	}
	source.mu.Lock()
	lastTag := source.tags[len(source.tags)-1]
	source.mu.Unlock()
	if lastTag != 2 {
		t.Fatalf("source received request tag=%d, want 2", lastTag)
	}
	if _, err := direct.ReadAtContext(first, buf, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read err=%v", err)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.ReadAtContext(second, buf, 0); err == nil {
		t.Fatal("closed direct input accepted a new read")
	}
}

func TestNestedAESCopyRejectsStaleMemberMetadata(t *testing.T) {
	for _, change := range []string{"ordinal", "name", "size", "crc", "stream", "offset", "stream-size"} {
		t.Run(change, func(t *testing.T) {
			data, original, _ := nestedAESCopyFixture(t)
			m := *original
			location := *original.sevenStream
			m.sevenStream = &location
			switch change {
			case "ordinal":
				m.ordinal++
			case "name":
				m.name = "changed.bin"
			case "size":
				m.size++
			case "crc":
				m.crc++
			case "stream":
				location.Stream++
			case "offset":
				location.Offset++
			case "stream-size":
				location.Size++
			}
			_, owned, ok, err := nestedCopyMemberReader(context.Background(), archivePlainInput{reader: bytes.NewReader(data)}, int64(len(data)), &m, original.ordinal, []byte("mount-test-password"))
			if owned != nil {
				defer owned.Close()
			}
			if ok || !errors.Is(err, syscall.ESTALE) {
				t.Fatalf("changed %s: direct=%t err=%v, want ESTALE", change, ok, err)
			}
		})
	}
}

func TestNestedAESCopyConstructorPreservesSourceError(t *testing.T) {
	data, m, _ := nestedAESCopyFixture(t)
	sentinel := errors.New("synthetic source transport error")
	_, owned, ok, err := nestedCopyMemberReader(context.Background(), &aesCopyTestInput{data: data, failure: sentinel}, int64(len(data)), m, m.ordinal, []byte("mount-test-password"))
	if owned != nil {
		defer owned.Close()
	}
	if ok || !errors.Is(err, sentinel) || errors.Is(err, syscall.EACCES) {
		t.Fatalf("source failure direct=%t err=%v", ok, err)
	}
}

func TestNestedAESCopyDirectLeafKeepsCRCAndParentOwnership(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "corrupt"}[corrupt], func(t *testing.T) {
			data, m, idx := nestedAESCopyFixture(t)
			if corrupt {
				zr, err := sevenzip.NewReaderWithPassword(bytes.NewReader(data), int64(len(data)), "mount-test-password")
				if err != nil {
					t.Fatal(err)
				}
				ranges, err := zr.PackedRanges(m.sevenStream.Stream)
				if err != nil {
					t.Fatal(err)
				}
				data[ranges[0].Offset+23] ^= 1
			}
			root := NewWithOptions(context.Background(), &directorySnapshotAPI{}, nil, 0, true, Options{})
			parent := &nestedArchive{input: archivePlainInput{reader: bytes.NewReader(data)}, size: int64(len(data)), format: ".7z", index: idx, password: []byte("mount-test-password"), refs: 1}
			handle, size, err := root.tree.openNestedArchiveMember(context.Background(), parent, m)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			if _, ok := handle.owned.(*nestedAESCopyInput); !ok {
				t.Fatalf("direct AES leaf has owned input=%T", handle.owned)
			}
			if _, ok := handle.input.(*archiveCheckedInput); !ok {
				t.Fatalf("direct AES leaf bypassed CRC wrapper: %T", handle.input)
			}
			if err := parent.Close(); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, size)
			_, err = handle.ReadAt(got, 0)
			if corrupt {
				if !errors.Is(err, errArchiveIntegrity) {
					t.Fatalf("corrupt direct leaf err=%v", err)
				}
			} else if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)) {
				t.Fatalf("valid direct leaf err=%v", err)
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			parent.mu.Lock()
			closed := parent.closed
			parent.mu.Unlock()
			if !closed {
				t.Fatal("handle did not release the last parent reference")
			}
			if _, err := handle.ReadAt(got[:1], 0); !errors.Is(err, storage.ErrClosed) {
				t.Fatalf("closed handle read err=%v", err)
			}
		})
	}
}

func TestNestedAESCopyLazyInitializationCancellationAndClose(t *testing.T) {
	for _, interrupt := range []string{"cancel", "close"} {
		t.Run(interrupt, func(t *testing.T) {
			data, m, idx := nestedAESCopyFixture(t)
			source := &aesCopyTestInput{data: data, started: make(chan struct{}, 1), release: make(chan struct{})}
			root := NewWithOptions(context.Background(), &directorySnapshotAPI{}, nil, 0, true, Options{})
			parent := &nestedArchive{input: source, size: int64(len(data)), format: ".7z", index: idx, password: []byte("mount-test-password"), refs: 1}
			defer parent.Close()
			lazy := &nestedVolumeInput{owner: root.tree, parent: parent, member: m, passwordKnown: true}
			defer lazy.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := lazy.ReadAtContext(ctx, make([]byte, 8), 0)
				done <- err
			}()
			select {
			case <-source.started:
			case <-time.After(time.Second):
				t.Fatal("direct member initialization did not start")
			}
			want := error(context.Canceled)
			if interrupt == "cancel" {
				cancel()
			} else {
				if err := lazy.Close(); err != nil {
					t.Fatal(err)
				}
				want = storage.ErrClosed
			}
			close(source.release)
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("interrupted initialization err=%v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("interrupted initialization did not complete")
			}
			lazy.mu.Lock()
			initialized := lazy.input != nil || lazy.reader != nil
			lazy.mu.Unlock()
			if initialized {
				t.Fatal("interrupted initialization retained an input")
			}
			if interrupt == "cancel" {
				got := make([]byte, 8)
				if n, err := lazy.ReadAtContext(context.Background(), got, 0); err != nil || n != len(got) || string(got) != "mount 7z" {
					t.Fatalf("retry read=%d err=%v", n, err)
				}
			}
		})
	}
}

func TestNestedSourceRecorderTracksZIPContextInput(t *testing.T) {
	for _, sourceErr := range []error{errors.New("synthetic ZIP transport failure"), io.ErrUnexpectedEOF} {
		recorder := &nestedSourceReader{}
		input := nestedSourceInput{input: &aesCopyTestInput{failure: sourceErr}, recorder: recorder}
		if _, err := input.ReadAtContext(context.Background(), make([]byte, 3), 0); !errors.Is(err, sourceErr) || !errors.Is(recorder.sourceError(), sourceErr) {
			t.Fatalf("ZIP source recording err=%v captured=%v", err, recorder.sourceError())
		}
	}
}
