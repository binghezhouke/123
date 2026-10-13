package mountfs

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"syscall"
	"testing"
)

type zipCheckpointObservation struct {
	points                []zipDeflateCheckpoint
	events                []string
	valid, invalid, close int
}

func (o *zipCheckpointObservation) hooks() zipDeflateCheckpointHooks {
	return zipDeflateCheckpointHooks{
		Want:    func(int64) bool { return true },
		Save:    func(p zipDeflateCheckpoint) { o.points = append(o.points, p); o.events = append(o.events, "save") },
		Valid:   func() { o.valid++; o.events = append(o.events, "valid") },
		Invalid: func() { o.invalid++; o.events = append(o.events, "invalid") },
		Close:   func() { o.close++; o.events = append(o.events, "close") },
	}
}

type zipCheckpointTestSource func(context.Context, []byte, int64) (int, error)

func (f zipCheckpointTestSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return f(ctx, p, off)
}

func zipCheckpointTestInput(compressed, payload []byte) (archiveReaderAt, *member, int64) {
	prefix := bytes.Repeat([]byte("p"), 17)
	data := append(prefix, compressed...)
	return archivePlainInput{reader: bytes.NewReader(data)}, &member{method: zip.Deflate, size: uint64(len(payload)), compressed: uint64(len(compressed)), crc: crc32.ChecksumIEEE(payload)}, int64(len(prefix))
}

func zipCheckpointTestReadAll(t *testing.T, r io.ReadCloser, chunk int) ([]byte, error) {
	t.Helper()
	var result bytes.Buffer
	buffer := make([]byte, chunk)
	for {
		n, err := r.Read(buffer)
		result.Write(buffer[:n])
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return result.Bytes(), err
		}
		if n == 0 {
			t.Fatal("zero read without a terminal error")
		}
	}
}

func zipCheckpointTestCompressed(t *testing.T, payload []byte, level int, empty bool, middle bool) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer, err := flate.NewWriter(&encoded, level)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		for i := 0; i < 3; i++ {
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if middle {
		_, _ = writer.Write(payload[:len(payload)/2])
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write(payload[len(payload)/2:])
	} else {
		_, _ = writer.Write(payload)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestZIPCheckpointStreamPairsDeliveredCRCAndResumes(t *testing.T) {
	mixed := make([]byte, 1<<20)
	state := uint64(1234567)
	for i := range mixed {
		if i%8192 >= 4096 {
			mixed[i] = mixed[i-4096]
		} else {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			mixed[i] = byte(state)
		}
	}
	cases := []struct {
		name                string
		compressed, payload []byte
		chunk               int
	}{
		{"nonaligned-history", []byte{0x72, 0x04, 0x0c, 0x08, 0x00}, []byte("AAAA"), 1},
		{"empty-full-window", zipCheckpointTestCompressed(t, bytes.Repeat([]byte("w"), 32769), flate.NoCompression, true, false), bytes.Repeat([]byte("w"), 32769), 32768},
		{"midstream-flush", zipCheckpointTestCompressed(t, mixed, flate.DefaultCompression, false, true), mixed, 7},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// An independent standard reader checks the handcrafted bit fixture.
			native := flate.NewReader(bytes.NewReader(test.compressed))
			oracle, err := io.ReadAll(native)
			_ = native.Close()
			if err != nil || !bytes.Equal(oracle, test.payload) {
				t.Fatalf("independent fixture error=%v", err)
			}
			source, m, offset := zipCheckpointTestInput(test.compressed, test.payload)
			observed := new(zipCheckpointObservation)
			reader, err := openZIPDeflateCheckpointStream(context.Background(), source, m, offset, nil, observed.hooks())
			if err != nil {
				t.Fatal(err)
			}
			got, err := zipCheckpointTestReadAll(t, reader, test.chunk)
			_ = reader.Close()
			_ = reader.Close()
			if err != nil || !bytes.Equal(got, test.payload) || observed.valid != 1 || observed.invalid != 0 || observed.close != 1 || len(observed.points) == 0 {
				t.Fatalf("full read error=%v valid=%d invalid=%d close=%d points=%d", err, observed.valid, observed.invalid, observed.close, len(observed.points))
			}
			if observed.events[len(observed.events)-2] != "valid" {
				t.Fatalf("Save must precede Valid, events=%v", observed.events)
			}
			unaligned := false
			for _, point := range observed.points {
				unaligned = unaligned || point.BitOffset != 0
				start := max(int64(0), point.OutputOffset-32768)
				if point.OutputOffset >= int64(len(test.payload)) || point.CRC != crc32.ChecksumIEEE(test.payload[:point.OutputOffset]) || !bytes.Equal(point.Window, test.payload[start:point.OutputOffset]) {
					t.Fatalf("checkpoint prefix mismatch out=%d input=%d bits=%d", point.OutputOffset, point.InputOffset, point.BitOffset)
				}
				resumed := new(zipCheckpointObservation)
				reader, err := openZIPDeflateCheckpointStream(context.Background(), source, m, offset, &point, resumed.hooks())
				if err != nil {
					t.Fatal(err)
				}
				suffix, err := zipCheckpointTestReadAll(t, reader, 113)
				_ = reader.Close()
				if err != nil || !bytes.Equal(suffix, test.payload[point.OutputOffset:]) || resumed.valid != 1 || resumed.invalid != 0 || resumed.close != 1 {
					t.Fatalf("resume out=%d bits=%d error=%v valid=%d", point.OutputOffset, point.BitOffset, err, resumed.valid)
				}
			}
			if test.name == "nonaligned-history" && !unaligned {
				t.Fatal("nonaligned checkpoint not observed")
			}
		})
	}
}

func TestZIPCheckpointStreamRejectsIntegrityFailures(t *testing.T) {
	compressed, payload := []byte{0x72, 0x04, 0x0c, 0x08, 0x00}, []byte("AAAA")
	for _, fault := range []string{"crc", "truncated", "format", "seed-crc"} {
		t.Run(fault, func(t *testing.T) {
			body := append([]byte(nil), compressed...)
			if fault == "truncated" {
				body = body[:len(body)-1]
			} else if fault == "format" {
				body[0] = 7 // Reserved block type 3.
			}
			source, m, offset := zipCheckpointTestInput(body, payload)
			m.compressed = uint64(len(compressed))
			var point *zipDeflateCheckpoint
			if fault == "crc" {
				m.crc ^= 1
			} else if fault == "seed-crc" {
				point = &zipDeflateCheckpoint{OutputOffset: 1, InputOffset: 2, BitOffset: 2, Window: []byte("A"), CRC: crc32.ChecksumIEEE([]byte("A")) ^ 1}
			}
			observed := new(zipCheckpointObservation)
			reader, err := openZIPDeflateCheckpointStream(context.Background(), source, m, offset, point, observed.hooks())
			if err != nil {
				t.Fatal(err)
			}
			_, err = zipCheckpointTestReadAll(t, reader, 13)
			if err == nil || observed.valid != 0 || observed.invalid != 1 {
				t.Fatalf("fault error=%v valid=%d invalid=%d", err, observed.valid, observed.invalid)
			}
			if _, repeat := reader.Read(make([]byte, 1)); !errors.Is(repeat, err) || observed.invalid != 1 {
				t.Fatalf("terminal error=%v, repeated=%v", err, repeat)
			}
			_ = reader.Close()
			_ = reader.Close()
			if observed.close != 1 {
				t.Fatalf("Close hooks=%d", observed.close)
			}
		})
	}
}

func TestZIPCheckpointStreamSourceErrorAndCancellationNeverValidate(t *testing.T) {
	compressed, payload := []byte{0x72, 0x04, 0x0c, 0x08, 0x00}, []byte("AAAA")
	t.Run("bytes-plus-source-error", func(t *testing.T) {
		base, m, offset := zipCheckpointTestInput(compressed, payload)
		fault := errors.New("synthetic source error after bytes")
		source := zipCheckpointTestSource(func(ctx context.Context, p []byte, off int64) (int, error) {
			n, _ := base.ReadAtContext(ctx, p, off)
			return n, fault
		})
		observed := new(zipCheckpointObservation)
		reader, err := openZIPDeflateCheckpointStream(context.Background(), source, m, offset, nil, observed.hooks())
		if err != nil {
			t.Fatal(err)
		}
		n, err := reader.Read(make([]byte, 16))
		_ = reader.Close()
		if n == 0 || !errors.Is(err, fault) || observed.valid != 0 || observed.invalid != 0 || len(observed.points) != 0 || observed.close != 1 {
			t.Fatalf("source n=%d error=%v valid=%d invalid=%d saves=%d close=%d", n, err, observed.valid, observed.invalid, len(observed.points), observed.close)
		}
	})
	for _, phase := range []string{"want", "save", "constructor"} {
		t.Run("cancel-"+phase, func(t *testing.T) {
			source, m, offset := zipCheckpointTestInput(compressed, payload)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := new(zipCheckpointObservation)
			hooks := observed.hooks()
			if phase == "want" {
				hooks.Want = func(int64) bool { cancel(); return true }
			} else if phase == "save" {
				original := hooks.Save
				hooks.Save = func(point zipDeflateCheckpoint) { original(point); cancel() }
			} else {
				cancel()
			}
			reader, err := openZIPDeflateCheckpointStream(ctx, source, m, offset, nil, hooks)
			if reader != nil {
				_, err = zipCheckpointTestReadAll(t, reader, 1)
				_ = reader.Close()
				_ = reader.Close()
			}
			if !errors.Is(err, context.Canceled) || observed.valid != 0 || observed.invalid != 0 || observed.close != 1 {
				t.Fatalf("cancel error=%v valid=%d invalid=%d close=%d", err, observed.valid, observed.invalid, observed.close)
			}
		})
	}
}

func TestZIPCheckpointStreamCloseAndEligibility(t *testing.T) {
	compressed, payload := []byte{0x72, 0x04, 0x0c, 0x08, 0x00}, []byte("AAAA")
	source, m, offset := zipCheckpointTestInput(compressed, payload)
	observed := new(zipCheckpointObservation)
	reader, err := openZIPDeflateCheckpointStream(context.Background(), source, m, offset, nil, observed.hooks())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := reader.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("partial n=%d error=%v", n, err)
	}
	_ = reader.Close()
	_ = reader.Close()
	if observed.valid != 0 || observed.invalid != 0 || observed.close != 1 {
		t.Fatalf("partial lifecycle valid=%d invalid=%d close=%d", observed.valid, observed.invalid, observed.close)
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("read after Close=%v", err)
	}
	for _, kind := range []string{"stored", "encrypted", "aes", "invalid-point"} {
		copy := *m
		var point *zipDeflateCheckpoint
		if kind == "stored" {
			copy.method = zip.Store
		} else if kind == "encrypted" {
			copy.flags = 1
		} else if kind == "aes" {
			copy.aes = &aesMemberInfo{}
		} else {
			point = &zipDeflateCheckpoint{OutputOffset: int64(len(payload))}
		}
		observed := new(zipCheckpointObservation)
		_, err := openZIPDeflateCheckpointStream(context.Background(), source, &copy, offset, point, observed.hooks())
		want := syscall.EOPNOTSUPP
		if kind == "invalid-point" {
			want = syscall.EINVAL
		}
		if !errors.Is(err, want) || observed.close != 1 || observed.invalid != 0 {
			t.Fatalf("eligibility %s error=%v close=%d invalid=%d", kind, err, observed.close, observed.invalid)
		}
	}
}
