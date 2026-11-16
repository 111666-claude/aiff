package aiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// floatFixtureHeader is the exact header written by the encoder for a stereo
// 44100 Hz fl32 file. The sample data is appended to it.
var floatFixtureHeader = []byte{
	// FORM + size (patched later) + AIFC
	'F', 'O', 'R', 'M', 0, 0, 0, 0, 'A', 'I', 'F', 'C',
	// FVER chunk
	'F', 'V', 'E', 'R', 0, 0, 0, 4, 0xA2, 0x80, 0x51, 0x40,
	// COMM chunk, size 44
	'C', 'O', 'M', 'M', 0, 0, 0, 44,
	// 2 channels, 2 frames
	0, 2, 0, 0, 0, 2,
	// 32 bits per sample
	0, 32,
	// 44100 as an 80 bit IEEE 754 extended float
	0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0,
	// compression type and pascal string ("32-bit floating point")
	'f', 'l', '3', '2', 21,
	'3', '2', '-', 'b', 'i', 't', ' ', 'f', 'l', 'o', 'a', 't', 'i', 'n', 'g', ' ', 'p', 'o', 'i', 'n', 't',
	// SSND chunk, size 24
	'S', 'S', 'N', 'D', 0, 0, 0, 24,
	0, 0, 0, 0, 0, 0, 0, 0,
}

// floatFixtureSamples is the fixed IEEE 754 sample payload used by the
// fixture: 0.5, -0.5, 1.5, -2.0.
var floatFixtureSamples = []byte{
	0x3F, 0x00, 0x00, 0x00,
	0xBF, 0x00, 0x00, 0x00,
	0x3F, 0xC0, 0x00, 0x00,
	0xC0, 0x00, 0x00, 0x00,
}

func TestFloatEncoderFixture(t *testing.T) {
	buf := &bytes.Buffer{}
	w := &memWriteSeeker{buf: buf}
	e := NewFloatEncoder(w, 44100, 2)
	err := e.WriteFloat(&audio.FloatBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   []float64{0.5, -0.5, 1.5, -2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	expected := append([]byte{}, floatFixtureHeader...)
	expected = append(expected, floatFixtureSamples...)
	// patch the FORM size: everything but the ID and size fields
	binary.BigEndian.PutUint32(expected[4:8], uint32(len(expected)-8))

	if got := buf.Bytes(); !bytes.Equal(got, expected) {
		t.Fatalf("encoded file doesn't match the fixture\nexpected:\n% x\ngot:\n% x", expected, got)
	}

	// Decode the fixture from disk and check every header field.
	path := filepath.Join(tempDir(t), "fixture.aifc")
	if err := ioutil.WriteFile(path, expected, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	d.ReadInfo()
	if d.Form != aifcID {
		t.Fatalf("expected an AIFC form, got %q", d.Form)
	}
	if d.Encoding != encFl32 {
		t.Fatalf("expected the fl32 encoding, got %q", d.Encoding)
	}
	if d.EncodingName != "32-bit floating point" {
		t.Fatalf("unexpected encoding name %q", d.EncodingName)
	}
	if d.NumChans != 2 || d.BitDepth != 32 || d.SampleRate != 44100 {
		t.Fatalf("unexpected COMM data: %d channels, %d bits, %d Hz", d.NumChans, d.BitDepth, d.SampleRate)
	}
	if d.NumSampleFrames != 2 {
		t.Fatalf("expected 2 frames, got %d", d.NumSampleFrames)
	}
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if d.PCMLen() != int64(len(floatFixtureSamples)) {
		t.Fatalf("expected %d PCM bytes, got %d", len(floatFixtureSamples), d.PCMLen())
	}
	buf2, err := d.FullPCMFloatBuffer()
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.5, -0.5, 1.5, -2}
	if !reflect.DeepEqual(buf2.Data, want) {
		t.Fatalf("expected samples %v, got %v", want, buf2.Data)
	}
	if buf2.Format.NumChannels != 2 || buf2.Format.SampleRate != 44100 {
		t.Fatalf("unexpected decoded format %+v", buf2.Format)
	}
}

// TestFloatEncoderChunked writes in several calls, checks the sample order and
// the exact float32 values read back. Values outside of [-1, 1] are preserved.
func TestFloatEncoderChunked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chans    int
		encoding [4]byte
		chunks   [][]float64
	}{
		{
			name:     "fl32 mono",
			chans:    1,
			encoding: encFl32,
			chunks:   [][]float64{{0, 0.25, -0.25}, {1, -1}, {1.5, -3.25, 123.5}},
		},
		{
			name:     "FL32 stereo",
			chans:    2,
			encoding: encFL32,
			chunks:   [][]float64{{0.1, -0.1, 2, -2}, {0.5, 0.5}, {-1.5, 4.25, 1e-6, -1e-6}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var all []float64
			for _, c := range tc.chunks {
				all = append(all, c...)
			}
			w := &memWriteSeeker{buf: &bytes.Buffer{}}
			e := NewFloatEncoder(w, 48000, tc.chans)
			e.Encoding = tc.encoding
			for _, c := range tc.chunks {
				if err := e.WriteFloat(&audio.FloatBuffer{
					Format: &audio.Format{NumChannels: tc.chans, SampleRate: 48000},
					Data:   c,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			raw := w.buf.Bytes()
			frames := len(all) / tc.chans
			// FORM size, COMM frame count, SSND size and payload offsets.
			if got := binary.BigEndian.Uint32(raw[4:8]); int(got) != len(raw)-8 {
				t.Fatalf("FORM size is %d, expected %d", got, len(raw)-8)
			}
			if got := binary.BigEndian.Uint32(raw[34:38]); int(got) != frames {
				t.Fatalf("COMM frame count is %d, expected %d", got, frames)
			}
			ssnd := bytes.Index(raw, []byte("SSND"))
			if ssnd < 0 {
				t.Fatal("no SSND chunk")
			}
			if got := binary.BigEndian.Uint32(raw[ssnd+4 : ssnd+8]); int(got) != 8+4*len(all) {
				t.Fatalf("SSND size is %d, expected %d", got, 8+4*len(all))
			}
			if !bytes.Equal(raw[ssnd+16:], float32Bytes(all)) {
				t.Fatal("sample payload is not the expected big endian IEEE 754 sequence")
			}
			// Sample order in the payload must be frame interleaved and identical
			// to a single write.
			single := &memWriteSeeker{buf: &bytes.Buffer{}}
			se := NewFloatEncoder(single, 48000, tc.chans)
			se.Encoding = tc.encoding
			if err := se.WriteFloat(&audio.FloatBuffer{
				Format: &audio.Format{NumChannels: tc.chans, SampleRate: 48000},
				Data:   all,
			}); err != nil {
				t.Fatal(err)
			}
			if err := se.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, single.buf.Bytes()) {
				t.Fatal("chunked writes differ from a single write")
			}

			roundTrip(t, raw, tc.encoding, tc.chans, frames, all)
		})
	}
}

func roundTrip(t *testing.T, raw []byte, encoding [4]byte, chans, frames int, want []float64) {
	t.Helper()
	path := filepath.Join(tempDir(t), "roundtrip.aifc")
	if err := ioutil.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	d.ReadInfo()
	if d.Encoding != encoding {
		t.Fatalf("expected encoding %q, got %q", encoding, d.Encoding)
	}
	if int(d.NumSampleFrames) != frames {
		t.Fatalf("expected %d frames, got %d", frames, d.NumSampleFrames)
	}
	// Decode in small chunks, ensuring the buffering doesn't alter the sample
	// sequence or values.
	var got []float64
	small := &audio.FloatBuffer{
		Format: &audio.Format{NumChannels: chans, SampleRate: 48000},
		Data:   make([]float64, chans*2),
	}
	for {
		n, err := d.PCMFloatBuffer(small)
		got = append(got, small.Data[:n]...)
		if err != nil {
			t.Fatalf("unexpected decode error: %v", err)
		}
		if n < len(small.Data) {
			break
		}
	}
	wantF32 := float32Bytes(want)
	wantValues := make([]float64, len(want))
	for i := range want {
		wantValues[i] = float64(math.Float32frombits(binary.BigEndian.Uint32(wantF32[i*4:])))
	}
	if !reflect.DeepEqual(got, wantValues) {
		t.Fatalf("decoded samples differ\nexpected: %v\ngot:      %v", wantValues, got)
	}

	// FullPCMFloatBuffer must produce the same result.
	if _, err := d.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	d.Reset()
	d.ReadInfo()
	full, err := d.FullPCMFloatBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Data, wantValues) {
		t.Fatalf("FullPCMFloatBuffer differs\nexpected: %v\ngot:      %v", wantValues, full.Data)
	}
	if full.NumFrames() != frames {
		t.Fatalf("expected %d frames, got %d", frames, full.NumFrames())
	}

	// Integer decoding must be refused explicitly.
	if _, err := d.FullPCMBuffer(); !errors.Is(err, ErrFloatPCMIntBuffer) {
		t.Fatalf("expected ErrFloatPCMIntBuffer, got %v", err)
	}
	if _, err := d.PCMBuffer(&audio.IntBuffer{Data: make([]int, 4)}); !errors.Is(err, ErrFloatPCMIntBuffer) {
		t.Fatalf("expected ErrFloatPCMIntBuffer, got %v", err)
	}
}

// tempDir returns a directory that is removed when the test completes.
// It's a placeholder for testing.T.TempDir which isn't available on the
// minimum Go version supported by this package.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := ioutil.TempDir("", "aiff-float")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func float32Bytes(samples []float64) []byte {
	out := make([]byte, 4*len(samples))
	for i, v := range samples {
		binary.BigEndian.PutUint32(out[i*4:], math.Float32bits(float32(v)))
	}
	return out
}

func float32Buffer(format *audio.Format, samples ...float64) *audio.Float32Buffer {
	out := make([]float32, len(samples))
	for i, v := range samples {
		out[i] = float32(v)
	}
	return &audio.Float32Buffer{Format: format, Data: out, SourceBitDepth: 32}
}

func TestFloatEncoderFloat32Buffer(t *testing.T) {
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewFloatEncoder(w, 22050, 2)
	data := []float32{0.5, -1.25, 3, -4}
	if err := e.WriteFloat32(float32Buffer(&audio.Format{NumChannels: 2, SampleRate: 22050}, float64(data[0]), float64(data[1]), float64(data[2]), float64(data[3]))); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	ssnd := bytes.Index(w.buf.Bytes(), []byte("SSND"))
	got := w.buf.Bytes()[ssnd+16:]
	for i, v := range data {
		if bits := binary.BigEndian.Uint32(got[i*4:]); bits != math.Float32bits(v) {
			t.Fatalf("sample %d is %#x, expected %#x", i, bits, math.Float32bits(v))
		}
	}
}

func TestFloatEncoderInvalidSamples(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"float32 overflow", 1e39},
		{"negative float32 overflow", -1e39},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &memWriteSeeker{buf: &bytes.Buffer{}}
			e := NewFloatEncoder(w, 44100, 1)
			err := e.WriteFloat(&audio.FloatBuffer{
				Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
				Data:   []float64{0.5, tc.value, 0.5},
			})
			if !errors.Is(err, ErrInvalidSample) {
				t.Fatalf("expected ErrInvalidSample, got %v", err)
			}
			// the binary.Write path caught the sample before anything was
			// written: no header, no sample and no frame count.
			if w.buf.Len() != 0 {
				t.Fatalf("expected no data to be written, got %d bytes", w.buf.Len())
			}
			if e.frames != 0 {
				t.Fatalf("expected no frames to be accounted for, got %d", e.frames)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("close shouldn't report an error on an empty file: %v", err)
			}
		})
	}

	// A float32 buffer can't carry NaN/Inf through the float64 validation
	// without being rejected.
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewFloatEncoder(w, 44100, 1)
	buf := &audio.Float32Buffer{
		Format:         &audio.Format{NumChannels: 1, SampleRate: 44100},
		Data:           []float32{1, float32(math.NaN())},
		SourceBitDepth: 32,
	}
	if err := e.WriteFloat32(buf); !errors.Is(err, ErrInvalidSample) {
		t.Fatalf("expected ErrInvalidSample for a NaN float32, got %v", err)
	}
	if w.buf.Len() != 0 {
		t.Fatalf("expected no data to be written, got %d bytes", w.buf.Len())
	}
}

func TestFloatEncoderMismatches(t *testing.T) {
	validFormat := &audio.Format{NumChannels: 2, SampleRate: 44100}
	cases := []struct {
		name string
		enc  func(*Encoder) *Encoder
		buf  *audio.FloatBuffer
		want error
	}{
		{
			name: "wrong channel count",
			enc:  func(e *Encoder) *Encoder { return e },
			buf: &audio.FloatBuffer{Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
				Data: []float64{1, 2}},
			want: ErrBufferMismatch,
		},
		{
			name: "wrong sample rate",
			enc:  func(e *Encoder) *Encoder { return e },
			buf: &audio.FloatBuffer{Format: &audio.Format{NumChannels: 2, SampleRate: 48000},
				Data: []float64{1, 2}},
			want: ErrBufferMismatch,
		},
		{
			name: "partial frame",
			enc:  func(e *Encoder) *Encoder { return e },
			buf:  &audio.FloatBuffer{Format: validFormat, Data: []float64{1, 2, 3}},
			want: ErrTruncatedPCM,
		},
		{
			name: "wrong bit depth",
			enc: func(e *Encoder) *Encoder {
				e.BitDepth = 16
				return e
			},
			buf:  &audio.FloatBuffer{Format: validFormat, Data: []float64{1, 2}},
			want: ErrBufferMismatch,
		},
		{
			name: "nil buffer format for stereo",
			enc:  func(e *Encoder) *Encoder { return e },
			buf:  &audio.FloatBuffer{Data: []float64{1, 2}},
			want: ErrBufferMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &memWriteSeeker{buf: &bytes.Buffer{}}
			e := tc.enc(NewFloatEncoder(w, 44100, 2))
			if err := e.WriteFloat(tc.buf); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if w.buf.Len() != 0 {
				t.Fatalf("expected no data to be written, got %d bytes", w.buf.Len())
			}
		})
	}

	// An integer buffer can't be written to a float encoder and vice versa.
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	f := NewFloatEncoder(w, 44100, 1)
	if err := f.Write(&audio.IntBuffer{Format: &audio.Format{NumChannels: 1, SampleRate: 44100}, Data: []int{1}}); !errors.Is(err, ErrBufferMismatch) {
		t.Fatalf("expected ErrBufferMismatch, got %v", err)
	}
	w2 := &memWriteSeeker{buf: &bytes.Buffer{}}
	i := NewEncoder(w2, 44100, 16, 1)
	if err := i.WriteFloat(&audio.FloatBuffer{Format: &audio.Format{NumChannels: 1, SampleRate: 44100}, Data: []float64{1}}); !errors.Is(err, ErrBufferMismatch) {
		t.Fatalf("expected ErrBufferMismatch, got %v", err)
	}
	if err := i.WriteFloat32(&audio.Float32Buffer{Format: &audio.Format{NumChannels: 1, SampleRate: 44100}, Data: []float32{1}}); !errors.Is(err, ErrBufferMismatch) {
		t.Fatalf("expected ErrBufferMismatch, got %v", err)
	}
	// An unsupported AIFC encoding is rejected instead of writing an opaque
	// header.
	e := NewFloatEncoder(w2, 44100, 1)
	e.Encoding = encSowt
	if err := e.WriteFloat(&audio.FloatBuffer{Format: &audio.Format{NumChannels: 1, SampleRate: 44100}, Data: []float64{1}}); !errors.Is(err, ErrBufferMismatch) {
		t.Fatalf("expected ErrBufferMismatch, got %v", err)
	}
	if _, err := NewFloatEncoderWithEncoding(w2, 44100, 1, encSowt); !errors.Is(err, ErrFmtNotSupported) {
		t.Fatalf("expected ErrFmtNotSupported, got %v", err)
	}
}

func TestFloatDecoderTruncated(t *testing.T) {
	raw := append([]byte{}, floatFixtureHeader...)
	// 2 channels, 32 bit samples: 6 bytes is less than a frame and isn't even
	// a full sample.
	raw = append(raw, 0x3F, 0x00, 0x00, 0x00, 0x3F, 0x00)
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
	binary.BigEndian.PutUint32(raw[34:38], 2) // expected frame count from the header
	// fix the SSND chunk size so it matches the truncated payload
	ssnd := bytes.Index(raw, []byte("SSND"))
	binary.BigEndian.PutUint32(raw[ssnd+4:], uint32(8+6))

	path := filepath.Join(tempDir(t), "truncated.aifc")
	if err := ioutil.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	buf := &audio.FloatBuffer{Data: make([]float64, 4)}
	n, err := d.PCMFloatBuffer(buf)
	if !errors.Is(err, ErrTruncatedPCM) {
		t.Fatalf("expected ErrTruncatedPCM, got %v", err)
	}
	if n != 0 {
		t.Fatalf("a partial frame must not be returned, got %d samples", n)
	}

	// A full sample followed by half a sample still returns the full frame and
	// reports the truncation.
	raw = append([]byte{}, floatFixtureHeader...)
	raw = append(raw, 0x3F, 0x00, 0x00, 0x00, 0xBF, 0x00, 0x00, 0x00, 0x3F, 0xC0)
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
	binary.BigEndian.PutUint32(raw[34:38], 2)
	ssnd = bytes.Index(raw, []byte("SSND"))
	binary.BigEndian.PutUint32(raw[ssnd+4:], uint32(8+10))
	path2 := filepath.Join(tempDir(t), "truncated2.aifc")
	if err := ioutil.WriteFile(path2, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f2, err := os.Open(path2)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	d2 := NewDecoder(f2)
	buf2 := &audio.FloatBuffer{Data: make([]float64, 4)}
	n, err = d2.PCMFloatBuffer(buf2)
	if !errors.Is(err, ErrTruncatedPCM) {
		t.Fatalf("expected ErrTruncatedPCM, got %v", err)
	}
	if n != 2 || buf2.Data[0] != 0.5 || buf2.Data[1] != -0.5 {
		t.Fatalf("expected the complete frame, got %d samples %v", n, buf2.Data[:n])
	}
}

func TestFloatDecoderWithOffset(t *testing.T) {
	// Build an fl32 file with a non zero SSND offset: the offset bytes are
	// part of the chunk but must not be decoded as samples.
	raw := append([]byte{}, floatFixtureHeader[:len(floatFixtureHeader)-16]...)
	comment := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	raw = append(raw, 'S', 'S', 'N', 'D')
	for _, v := range []uint32{
		uint32(8 + len(comment) + len(floatFixtureSamples)),
		uint32(len(comment)),
		0,
	} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], v)
		raw = append(raw, b[:]...)
	}
	raw = append(raw, comment...)
	raw = append(raw, floatFixtureSamples...)
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(raw)-8))

	path := filepath.Join(tempDir(t), "offset.aifc")
	if err := ioutil.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if d.PCMLen() != int64(len(floatFixtureSamples)) {
		t.Fatalf("expected %d PCM bytes, got %d", len(floatFixtureSamples), d.PCMLen())
	}
	buf, err := d.FullPCMFloatBuffer()
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.5, -0.5, 1.5, -2}
	if !reflect.DeepEqual(buf.Data, want) {
		t.Fatalf("expected samples %v, got %v", want, buf.Data)
	}
}

func TestFloatIsValidFile(t *testing.T) {
	raw := append([]byte{}, floatFixtureHeader...)
	raw = append(raw, floatFixtureSamples...)
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(raw)-8))

	path := filepath.Join(tempDir(t), "valid.aifc")
	if err := ioutil.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if d := NewDecoder(f); !d.IsValidFile() {
		t.Fatalf("fl32 file should be valid: %+v", d)
	}
}

func TestDecoderRewindFloat(t *testing.T) {
	raw := append([]byte{}, floatFixtureHeader...)
	raw = append(raw, floatFixtureSamples...)
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
	path := filepath.Join(tempDir(t), "rewind.aifc")
	if err := ioutil.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	read := func() []float64 {
		buf := &audio.FloatBuffer{Format: d.Format(), Data: make([]float64, 4)}
		n, err := d.PCMFloatBuffer(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf.Data[:n]
	}
	first := read()
	if err := d.Rewind(); err != nil {
		t.Fatal(err)
	}
	if second := read(); !reflect.DeepEqual(first, second) {
		t.Fatalf("rewound read differs: %v vs %v", first, second)
	}
}

func TestFloatEncoderWriteFailure(t *testing.T) {
	// Fail after the header and halfway through the first frame.
	w := &failingWriteSeeker{limit: 98, buf: &bytes.Buffer{}}
	e := NewFloatEncoder(w, 44100, 2)
	err := e.WriteFloat(&audio.FloatBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   []float64{1, 2, 3, 4},
	})
	if err == nil {
		t.Fatal("expected a write error")
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
	// No partial frame is accounted for.
	if e.frames != 0 {
		t.Fatalf("expected 0 complete frames, got %d", e.frames)
	}
	// Following writes must keep failing with the same error.
	if err2 := e.WriteFloat(&audio.FloatBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   []float64{5, 6},
	}); !errors.Is(err2, io.ErrShortWrite) {
		t.Fatalf("expected the latched error, got %v", err2)
	}
	if err := e.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected Close to report the write error, got %v", err)
	}
}

// memWriteSeeker is a minimal in memory io.WriteSeeker.
type memWriteSeeker struct {
	buf *bytes.Buffer
	pos int
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	if m.pos > m.buf.Len() {
		// extend with zeros to mimic a file seeked past its end.
		m.buf.Write(make([]byte, m.pos-m.buf.Len()))
	}
	b := m.buf.Bytes()
	if m.pos+len(p) > len(b) {
		m.buf.Write(make([]byte, m.pos+len(p)-len(b)))
		b = m.buf.Bytes()
	}
	n := copy(b[m.pos:], p)
	m.pos += n
	return n, nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int
	switch whence {
	case io.SeekStart:
		next = int(offset)
	case io.SeekCurrent:
		next = m.pos + int(offset)
	case io.SeekEnd:
		next = m.buf.Len() + int(offset)
	}
	if next < 0 {
		return int64(m.pos), errors.New("negative seek position")
	}
	m.pos = next
	return int64(m.pos), nil
}

// failingWriteSeeker accepts limit bytes then fails with io.ErrShortWrite.
type failingWriteSeeker struct {
	limit int
	buf   *bytes.Buffer
	pos   int
}

func (f *failingWriteSeeker) Write(p []byte) (int, error) {
	m := &memWriteSeeker{buf: f.buf, pos: f.pos}
	if f.pos+len(p) > f.limit {
		remaining := f.limit - f.pos
		if remaining < 0 {
			remaining = 0
		}
		n, _ := m.Write(p[:remaining])
		f.pos = m.pos
		return n, io.ErrShortWrite
	}
	n, err := m.Write(p)
	f.pos = m.pos
	return n, err
}

func (f *failingWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	m := &memWriteSeeker{buf: f.buf, pos: f.pos}
	pos, err := m.Seek(offset, whence)
	f.pos = m.pos
	return pos, err
}

func TestPCMFloatBufferIntegerContainers(t *testing.T) {
	cases := []struct {
		file  string
		first []float64
	}{
		{"fixtures/kick.aif", []float64{76, 76, 75, 75, 72}},
		{"fixtures/kick8b.aiff", nil},
		{"fixtures/zipper24b.aiff", nil},
		{"fixtures/zipper.aiff", nil},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := NewDecoder(f)
			buf := &audio.FloatBuffer{Data: make([]float64, 64)}
			n, err := d.PCMFloatBuffer(buf)
			if err != nil {
				t.Fatal(err)
			}
			if n != 64 {
				t.Fatalf("expected 64 samples, got %d", n)
			}
			for i := range tc.first {
				if buf.Data[i] != tc.first[i] {
					t.Fatalf("sample %d is %v, expected %v", i, buf.Data[i], tc.first[i])
				}
			}
		})
	}

	// 24 bit signed samples must be sign extended when converted.
	f, err := os.Open("fixtures/zipper24b.aiff")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	ibuf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	d2 := NewDecoder(f)
	fbuf, err := d2.FullPCMFloatBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(fbuf.Data) != len(ibuf.Data) {
		t.Fatalf("expected %d samples, got %d", len(ibuf.Data), len(fbuf.Data))
	}
	for i, v := range ibuf.Data {
		if fbuf.Data[i] != float64(v) {
			t.Fatalf("sample %d is %v, expected %v", i, fbuf.Data[i], float64(v))
		}
	}
}

func TestFloatEncoderCloseIdempotent(t *testing.T) {
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewFloatEncoder(w, 44100, 1)
	if err := e.WriteFloat(&audio.FloatBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		Data:   []float64{0.5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	size := w.buf.Len()
	if err := e.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if w.buf.Len() != size {
		t.Fatal("closing twice changed the file")
	}
	if err := e.WriteFloat(&audio.FloatBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		Data:   []float64{0.5},
	}); err == nil {
		t.Fatal("expected a write after close to fail")
	}
}
