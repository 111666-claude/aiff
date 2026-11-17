package aiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/audio"
)

// memWriteSeeker is an in-memory io.WriteSeeker used to capture encoder output.
type memWriteSeeker struct {
	buf *bytes.Buffer
	pos int64
}

func (w *memWriteSeeker) Write(p []byte) (int, error) {
	if int(w.pos) > w.buf.Len() {
		w.buf.Write(make([]byte, int(w.pos)-w.buf.Len()))
	}
	existing := w.buf.Bytes()
	n := copy(existing[w.pos:], p)
	if n < len(p) {
		w.buf.Write(p[n:])
	}
	w.pos += int64(len(p))
	return len(p), nil
}

func (w *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(w.buf.Len()) + offset
	}
	if w.pos < 0 {
		return 0, errors.New("negative position")
	}
	return w.pos, nil
}

// ulawFixture builds a minimal AIFC container holding the passed 8 bit code
// words. The COMM chunk declares the frame count and the compression type.
func ulawFixture(t *testing.T, enc [4]byte, numChans int, frames int, rate int, payload []byte) []byte {
	t.Helper()
	commBody := make([]byte, 0, 24)
	commBody = binary.BigEndian.AppendUint16(commBody, uint16(numChans))
	commBody = binary.BigEndian.AppendUint32(commBody, uint32(frames))
	commBody = binary.BigEndian.AppendUint16(commBody, 8)
	rateBytes := audio.IntToIEEEFloat(rate)
	commBody = append(commBody, rateBytes[:]...)
	commBody = append(commBody, enc[:]...)
	// empty pascal style compression name, padded to an even size
	commBody = append(commBody, 0)
	if len(commBody)%2 != 0 {
		commBody = append(commBody, 0)
	}

	ssndBody := make([]byte, 0, 8+len(payload))
	ssndBody = binary.BigEndian.AppendUint32(ssndBody, 0) // offset
	ssndBody = binary.BigEndian.AppendUint32(ssndBody, 0) // block size
	ssndBody = append(ssndBody, payload...)

	out := make([]byte, 0, 12+8+len(commBody)+8+len(ssndBody)+1)
	out = append(out, "FORM"...)
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, "AIFC"...)
	out = append(out, "COMM"...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(commBody)))
	out = append(out, commBody...)
	out = append(out, "SSND"...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(ssndBody)))
	out = append(out, ssndBody...)
	if len(ssndBody)%2 != 0 {
		out = append(out, 0)
	}
	binary.BigEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// decoderFromBytes returns a decoder reading the passed container.
func decoderFromBytes(data []byte) *Decoder {
	return NewDecoder(bytes.NewReader(data))
}

func TestULawDecodeKnownCodeWords(t *testing.T) {
	// Values generated with the reference G.711 μ-law expansion (identical to
	// libsndfile and ffmpeg).
	tests := []struct {
		code byte
		want int
	}{
		{0x00, -32124},
		{0x01, -31100},
		{0x7E, -8},
		{0x7F, 0},
		{0x80, 32124},
		{0x81, 31100},
		{0xFE, 8},
		{0xFF, 0},
		{0x55, -716},
		{0xAA, 5372},
		{0x2A, -5372},
		{0xD5, 716},
	}
	for _, tc := range tests {
		if got := ulawDecodeSample(tc.code); got != tc.want {
			t.Errorf("decoding 0x%02X: expected %d, got %d", tc.code, tc.want, got)
		}
	}
}

func TestULawEncodeKnownSamples(t *testing.T) {
	tests := []struct {
		sample int
		want   byte
	}{
		{0, 0xFF},
		{-32124, 0x00},
		{32124, 0x80},
		{-716, 0x55},
		{5372, 0xAA},
		{8, 0xFE},
		{-8, 0x7E},
		{32767, 0x80},
		{-32768, 0x00},
		{-1, 0x7E},
		{1, 0xFF},
		// out of range values are clamped
		{40000, 0x80},
		{-40000, 0x00},
	}
	for _, tc := range tests {
		if got := ulawEncodeSample(tc.sample); got != tc.want {
			t.Errorf("encoding %d: expected 0x%02X, got 0x%02X", tc.sample, tc.want, got)
		}
	}
	// Every code word decodes to a value that encodes back to a code word
	// decoding to the same value. The two zero code words (0x7F and 0xFF) are
	// the only ones that can swap on a round trip.
	for i := 0; i < 256; i++ {
		decoded := ulawDecodeSample(byte(i))
		if back := ulawDecodeSample(ulawEncodeSample(decoded)); back != decoded {
			t.Errorf("0x%02X decoded to %d but round trips to %d", i, decoded, back)
		}
	}
	if got := ulawEncodeSample(ulawDecodeSample(0x7F)); got != 0xFF {
		t.Errorf("the positive zero (0x7F) is expected to encode as 0xFF, got 0x%02X", got)
	}
}

func TestULawDecodeStereoFixture(t *testing.T) {
	// Left and right channels alternate distinct code words so a channel
	// swap or an off-by-one would be detected.
	payload := []byte{
		0x00, 0xFF,
		0x10, 0xEF,
		0x20, 0xDF,
		0x30, 0xCF,
		0x80, 0x7F,
		0x90, 0x6F,
		0xA0, 0x5F,
		0xB0, 0x4F,
	}
	want := []int{
		-32124, 0,
		-15996, 132,
		-7932, 396,
		-3900, 924,
		32124, 0,
		15996, -132,
		7932, -396,
		3900, -924,
	}

	for _, enc := range [][4]byte{envUlaw, encULAW} {
		data := ulawFixture(t, enc, 2, len(payload)/2, 8000, payload)
		d := decoderFromBytes(data)
		buf, err := d.FullPCMBuffer()
		if err != nil {
			t.Fatalf("%q: decoding failed - %v", enc, err)
		}
		if d.Encoding != enc {
			t.Fatalf("expected the encoding to be %q, got %q", enc, d.Encoding)
		}
		if d.BitDepth != 8 {
			t.Fatalf("expected 8 bit samples, got %d", d.BitDepth)
		}
		if d.NumChans != 2 || d.NumSampleFrames != 8 {
			t.Fatalf("unexpected format: %d channels, %d frames", d.NumChans, d.NumSampleFrames)
		}
		if buf.SourceBitDepth != 16 {
			t.Fatalf("expected μ-law to decode to 16 bit PCM, got %d", buf.SourceBitDepth)
		}
		if buf.Format.NumChannels != 2 || buf.Format.SampleRate != 8000 {
			t.Fatalf("unexpected buffer format %+v", buf.Format)
		}
		if len(buf.Data) != len(want) {
			t.Fatalf("expected %d samples, got %d", len(want), len(buf.Data))
		}
		for i := range want {
			if buf.Data[i] != want[i] {
				t.Fatalf("%q: sample %d expected %d, got %d", enc, i, want[i], buf.Data[i])
			}
		}
	}
}

func TestULawDecodeMonoOddPayload(t *testing.T) {
	// An odd number of code words: the chunk gets an even sized pad byte that
	// must not be decoded as a sample.
	payload := []byte{0x00, 0x10, 0x20, 0x30, 0x40}
	want := []int{-32124, -15996, -7932, -3900, -1884}
	data := ulawFixture(t, envUlaw, 1, len(payload), 8000, payload)
	d := decoderFromBytes(data)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decoding failed - %v", err)
	}
	if len(buf.Data) != len(want) {
		t.Fatalf("expected %d samples, got %d (%v)", len(want), len(buf.Data), buf.Data)
	}
	for i := range want {
		if buf.Data[i] != want[i] {
			t.Fatalf("sample %d expected %d, got %d", i, want[i], buf.Data[i])
		}
	}
	if d.PCMLen() != int64(len(payload)) {
		t.Fatalf("expected %d PCM bytes, got %d", len(payload), d.PCMLen())
	}
}

func TestULawDecodeStreamingMatchesFull(t *testing.T) {
	// Streaming reads must return the same samples as the full buffer read,
	// even when the buffer boundaries don't align with the sample count.
	payload := make([]byte, 0, 301)
	for i := 0; i < 301; i++ {
		payload = append(payload, byte(i*37))
	}
	data := ulawFixture(t, envUlaw, 2, len(payload)/2, 8000, payload[:300])
	full, err := decoderFromBytes(data).FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}

	d := decoderFromBytes(data)
	var got []int
	buf := &audio.IntBuffer{Data: make([]int, 7)}
	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		got = append(got, buf.Data[:n]...)
	}
	if len(got) != len(full.Data) {
		t.Fatalf("streamed %d samples, full read returned %d", len(got), len(full.Data))
	}
	for i := range got {
		if got[i] != full.Data[i] {
			t.Fatalf("sample %d differs: streamed %d, full %d", i, got[i], full.Data[i])
		}
	}
}

func TestULawEncoderContainer(t *testing.T) {
	// 2 channels, 3 frames of 16 bit PCM.
	samples := []int{-32768, 32767, 0, 0, 1000, -1000}
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 8000, 2)
	err := e.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 8000},
		Data:   samples,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	got := w.buf.Bytes()
	expected := []byte{
		// FORM + placeholder size + AIFC
		'F', 'O', 'R', 'M', 0, 0, 0, 0, 'A', 'I', 'F', 'C',
		// FVER chunk
		'F', 'V', 'E', 'R', 0, 0, 0, 4, 0xA2, 0x80, 0x51, 0x40,
		// COMM chunk of 24 bytes
		'C', 'O', 'M', 'M', 0, 0, 0, 24,
		// 2 channels, 3 frames
		0, 2, 0, 0, 0, 3,
		// 8 bit samples
		0, 8,
		// 8000 Hz as an 80 bit IEEE 754 extended float
		0x40, 0x0B, 0xFA, 0, 0, 0, 0, 0, 0, 0,
		// compression type and an empty pascal name padded to an even size
		'u', 'l', 'a', 'w', 0, 0,
		// SSND chunk of 14 bytes (8 header bytes + 6 code words)
		'S', 'S', 'N', 'D', 0, 0, 0, 14,
		0, 0, 0, 0, 0, 0, 0, 0,
		// -32768, 32767, 0, 0, 1000, -1000
		0x00, 0x80, 0xFF, 0xFF, 0xCE, 0x4E,
	}
	binary.BigEndian.PutUint32(expected[4:8], uint32(len(expected)-8))
	if !bytes.Equal(got, expected) {
		t.Fatalf("encoded container doesn't match the fixture\nexpected:\n% x\ngot:\n% x", expected, got)
	}

	// The decoder reads the container back with the expected metadata.
	d := decoderFromBytes(got)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if d.Form != aifcID || d.Encoding != envUlaw {
		t.Fatalf("unexpected container metadata: form %q, encoding %q", d.Form, d.Encoding)
	}
	if d.NumChans != 2 || d.NumSampleFrames != 3 || d.BitDepth != 8 || d.SampleRate != 8000 {
		t.Fatalf("unexpected COMM values: %d channels, %d frames, %d bits, %d Hz",
			d.NumChans, d.NumSampleFrames, d.BitDepth, d.SampleRate)
	}
	if d.SSNDOffset != 0 || d.SSNDBlockSize != 0 {
		t.Fatalf("unexpected SSND header: offset %d, block size %d", d.SSNDOffset, d.SSNDBlockSize)
	}
	if d.PCMLen() != int64(len(samples)) {
		t.Fatalf("expected %d PCM bytes, got %d", len(samples), d.PCMLen())
	}
	wantDecoded := make([]int, len(samples))
	for i, s := range samples {
		wantDecoded[i] = ulawDecodeSample(ulawEncodeSample(s))
	}
	if len(buf.Data) != len(wantDecoded) {
		t.Fatalf("expected %d decoded samples, got %d", len(wantDecoded), len(buf.Data))
	}
	for i := range wantDecoded {
		if buf.Data[i] != wantDecoded[i] {
			t.Fatalf("sample %d expected %d, got %d", i, wantDecoded[i], buf.Data[i])
		}
	}
}

func TestULawEncoderUpperCaseAndPadding(t *testing.T) {
	// A mono buffer with an odd number of frames produces odd sized sample
	// data that must be padded, while the declared SSND and FORM sizes stay
	// correct.
	samples := []int{0, 1000, -1000}
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 8000, 1)
	e.Encoding = encULAW
	for _, s := range samples {
		if err := e.Write(&audio.IntBuffer{
			Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
			Data:   []int{s},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data := w.buf.Bytes()

	if len(data)%2 != 0 {
		t.Fatalf("the container isn't even sized: %d bytes", len(data))
	}
	if got := binary.BigEndian.Uint32(data[4:8]); got != uint32(len(data)-8) {
		t.Fatalf("FORM size is %d, expected %d", got, len(data)-8)
	}
	// Find the SSND chunk and check its declared size and padding.
	var ssndAt int
	for pos := 12; pos+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		if string(data[pos:pos+4]) == "SSND" {
			ssndAt = pos
			// 8 header bytes + 3 code words, the pad byte isn't included.
			if size != 8+len(samples) {
				t.Fatalf("SSND size is %d, expected %d", size, 8+len(samples))
			}
			if pos+8+size+1 != len(data) {
				t.Fatalf("SSND doesn't end with a pad byte: chunk ends at %d, file is %d bytes", pos+8+size, len(data))
			}
			if data[pos+8+size] != 0 {
				t.Fatalf("the SSND pad byte is 0x%02X, expected 0", data[pos+8+size])
			}
			break
		}
		pos += 8 + size
		if size%2 != 0 {
			pos++
		}
	}
	if ssndAt == 0 {
		t.Fatal("no SSND chunk was written")
	}

	d := decoderFromBytes(data)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if d.Encoding != encULAW {
		t.Fatalf("expected the upper case ULAW encoding, got %q", d.Encoding)
	}
	if d.NumSampleFrames != uint32(len(samples)) {
		t.Fatalf("expected %d frames, got %d", len(samples), d.NumSampleFrames)
	}
	if len(buf.Data) != len(samples) {
		t.Fatalf("expected %d decoded samples, got %d", len(samples), len(buf.Data))
	}
}

func TestULawEncoderStereoRoundTrip(t *testing.T) {
	// Interleaved 16 bit stereo: left and right values are distinct so a
	// channel swap is immediately visible.
	left := []int{-30000, -20000, -10000, 0, 10000, 20000, 30000, 100}
	right := []int{30000, 20000, 10000, 0, -10000, -20000, -30000, -100}
	var data []int
	for i := range left {
		data = append(data, left[i], right[i])
	}

	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 44100, 2)
	if err := e.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   data,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	d := decoderFromBytes(w.buf.Bytes())
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(buf.Data) != len(data) {
		t.Fatalf("expected %d samples, got %d", len(data), len(buf.Data))
	}
	for i := range data {
		want := ulawDecodeSample(ulawEncodeSample(data[i]))
		if buf.Data[i] != want {
			t.Fatalf("sample %d (%s) expected %d, got %d", i, map[bool]string{true: "left", false: "right"}[i%2 == 0], want, buf.Data[i])
		}
	}
	// The decoded channels must not be swapped: left channel samples encode
	// as the exact code words of the left values.
	for i := range left {
		if got, want := buf.Data[i*2], ulawDecodeSample(ulawEncodeSample(left[i])); got != want {
			t.Fatalf("left sample %d expected %d, got %d", i, want, got)
		}
		if got, want := buf.Data[i*2+1], ulawDecodeSample(ulawEncodeSample(right[i])); got != want {
			t.Fatalf("right sample %d expected %d, got %d", i, want, got)
		}
	}
}

func TestULawWriterMatchesReferenceEncoding(t *testing.T) {
	// The exact bytes written by the reference G.711 encoder (ffmpeg /
	// libsndfile) for these samples. The COMM chunk is the same as our
	// encoder writes: 8 bit samples, "ulaw" compression type, empty name.
	samples := []int{0, 1000, -1000, 32767, -32768, 1, -1}
	want := []byte{0xFF, 0xCE, 0x4E, 0x80, 0x00, 0xFF, 0x7E}

	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 8000, 1)
	if err := e.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
		Data:   samples,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data := w.buf.Bytes()
	idx := bytes.Index(data, []byte("SSND"))
	if idx < 0 {
		t.Fatal("no SSND chunk")
	}
	payload := data[idx+16 : idx+16+len(want)]
	if !bytes.Equal(payload, want) {
		t.Fatalf("expected code words % x, got % x", want, payload)
	}
}

func TestULawDecoderOriginalAIFFStillWorks(t *testing.T) {
	// Existing AIFF fixtures keep decoding the same way.
	f, err := os.Open(filepath.Join("fixtures", "kick.aif"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if d.Encoding != encNotSet || d.BitDepth != 16 {
		t.Fatalf("unexpected fixture metadata: encoding %q, %d bits", d.Encoding, d.BitDepth)
	}
	if len(buf.Data) != 4484 || buf.Data[0] != 76 {
		t.Fatalf("unexpected fixture samples: %d samples, first %d", len(buf.Data), buf.Data[0])
	}
	if buf.SourceBitDepth != 16 {
		t.Fatalf("expected 16 bit source depth, got %d", buf.SourceBitDepth)
	}
}

// corrupt returns a copy of data with size bytes replaced at pos.
func corrupt(data []byte, pos int, b ...byte) []byte {
	out := append([]byte(nil), data...)
	copy(out[pos:], b)
	return out
}

func TestULawDecodeSSNDOffset(t *testing.T) {
	// The SSND offset is the number of comment bytes stored before the
	// samples. Those bytes must not be decoded and the frame count is
	// computed from the real sample data only.
	payload := []byte{0x00, 0x10, 0x20}
	data := ulawFixture(t, envUlaw, 1, 3, 8000, payload)
	// Patch the SSND header: offset of 4 comment bytes.
	idx := bytes.Index(data, []byte("SSND"))
	if idx < 0 {
		t.Fatal("no SSND chunk")
	}
	binary.BigEndian.PutUint32(data[idx+8:idx+12], 4)
	// Insert the comment ahead of the sample data and grow the chunk.
	ssndSize := binary.BigEndian.Uint32(data[idx+4 : idx+8])
	binary.BigEndian.PutUint32(data[idx+4:idx+8], ssndSize+4)
	var withComment []byte
	withComment = append(withComment, data[:idx+16]...)
	withComment = append(withComment, 'j', 'u', 'n', 'k')
	withComment = append(withComment, data[idx+16:]...)
	binary.BigEndian.PutUint32(withComment[4:8], uint32(len(withComment)-8))

	d := decoderFromBytes(withComment)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decoding failed - %v", err)
	}
	if d.SSNDOffset != 4 {
		t.Fatalf("expected an SSND offset of 4, got %d", d.SSNDOffset)
	}
	if d.PCMLen() != 3 {
		t.Fatalf("expected 3 PCM bytes, got %d", d.PCMLen())
	}
	want := []int{-32124, -15996, -7932}
	for i := range want {
		if buf.Data[i] != want[i] {
			t.Fatalf("sample %d expected %d, got %d", i, want[i], buf.Data[i])
		}
	}
}

func TestULawDecodePadByteCountedInChunkSize(t *testing.T) {
	// Python's aifc module counts the pad byte that keeps chunks even sized
	// in the declared SSND chunk size. Such a file must still decode to the
	// declared number of frames, without the pad byte as an extra sample.
	payload := []byte{0x00, 0x10, 0x20}
	data := ulawFixture(t, envUlaw, 1, 3, 8000, payload)
	idx := bytes.Index(data, []byte("SSND"))
	ssndSize := binary.BigEndian.Uint32(data[idx+4 : idx+8])
	// The helper wrote 8 + 3 = 11 and a pad byte after the chunk: claim 12.
	binary.BigEndian.PutUint32(data[idx+4:idx+8], ssndSize+1)

	d := decoderFromBytes(data)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decoding failed - %v", err)
	}
	if len(buf.Data) != len(payload) {
		t.Fatalf("expected %d samples, got %d", len(payload), len(buf.Data))
	}
	if d.PCMLen() != int64(len(payload)) {
		t.Fatalf("expected %d PCM bytes, got %d", len(payload), d.PCMLen())
	}
}

func TestULawDecodeErrors(t *testing.T) {
	payload := []byte{0x00, 0x10, 0x20, 0x30}
	valid := func() []byte { return ulawFixture(t, envUlaw, 2, 2, 8000, payload) }

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name:    "frame count larger than the payload",
			data:    ulawFixture(t, envUlaw, 2, 3, 8000, payload),
			wantErr: ErrPCMFrameMismatch,
		},
		{
			name:    "frame count smaller than the payload",
			data:    ulawFixture(t, envUlaw, 2, 1, 8000, payload),
			wantErr: ErrPCMFrameMismatch,
		},
		{
			name: "unsupported compression type",
			data: func() []byte {
				return ulawFixture(t, [4]byte{'a', 'l', 'a', 'w'}, 2, 2, 8000, payload)
			}(),
			wantErr: ErrFmtNotSupported,
		},
		{
			name: "COMM chunk too short",
			data: func() []byte {
				// A COMM chunk declaring 12 bytes.
				d := valid()
				return corrupt(d, 16, 0, 0, 0, 12)
			}(),
			wantErr: ErrUnexpectedData,
		},
		{
			name: "COMM chunk without compression type",
			data: func() []byte {
				d := valid()
				// 18 bytes: enough for plain AIFF, not for an AIFC tag.
				return corrupt(d, 16, 0, 0, 0, 18)
			}(),
			wantErr: ErrUnexpectedData,
		},
		{
			name: "invalid channel count",
			data: func() []byte {
				d := valid()
				// channels field is the first COMM byte
				return corrupt(d, 20, 0, 0)
			}(),
			wantErr: ErrInvalidParams,
		},
		{
			name: "unsupported bit depth",
			data: func() []byte {
				d := valid()
				// sample size field
				return corrupt(d, 26, 0, 12)
			}(),
			wantErr: ErrInvalidParams,
		},
		{
			name: "truncated SSND header",
			data: func() []byte {
				d := valid()
				// SSND size of 4 can't hold the offset and block size fields.
				// The FORM size is adjusted so the walk reaches the SSND.
				d = corrupt(d, 48, 0, 0, 0, 4)
				d = d[:52]
				binary.BigEndian.PutUint32(d[4:8], uint32(len(d)-8))
				return d
			}(),
			wantErr: ErrUnexpectedData,
		},
		{
			name: "SSND offset beyond the sample data",
			data: func() []byte {
				d := valid()
				// SSND offset is the first SSND payload field
				return corrupt(d, 52, 0, 0, 0, 100)
			}(),
			wantErr: ErrUnexpectedData,
		},
		{
			name: "truncated file",
			data: func() []byte {
				d := valid()
				return d[:len(d)-3]
			}(),
			wantErr: ErrTruncatedPCM,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decoderFromBytes(tc.data)
			_, err := d.FullPCMBuffer()
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestULawDecodeErrorsWhileDraining(t *testing.T) {
	// Walking the chunks without reading the PCM must still catch an
	// inconsistent frame count.
	data := ulawFixture(t, envUlaw, 1, 5, 8000, []byte{0x00, 0x10})
	d := decoderFromBytes(data)
	if err := d.Drain(); err == nil {
		t.Fatal("expected draining to report the frame count mismatch")
	} else if !errors.Is(err, ErrPCMFrameMismatch) {
		t.Fatalf("expected %v, got %v", ErrPCMFrameMismatch, err)
	}
	if d.IsValidFile() {
		t.Fatal("a file with a mismatched frame count shouldn't be valid")
	}
}

func TestULawEncoderErrors(t *testing.T) {
	buf := func(chans, rate int, data ...int) *audio.IntBuffer {
		return &audio.IntBuffer{
			Format: &audio.Format{NumChannels: chans, SampleRate: rate},
			Data:   data,
		}
	}

	tests := []struct {
		name    string
		encoder *Encoder
		buf     *audio.IntBuffer
		wantErr error
	}{
		{
			name:    "bit depth must be 16",
			encoder: &Encoder{w: &memWriteSeeker{buf: &bytes.Buffer{}}, SampleRate: 8000, BitDepth: 8, NumChans: 1, Encoding: envUlaw},
			buf:     buf(1, 8000, 0),
			wantErr: ErrInvalidParams,
		},
		{
			name:    "invalid sample rate",
			encoder: NewEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 0, 16, 1),
			buf:     buf(1, 0, 0),
			wantErr: ErrInvalidParams,
		},
		{
			name:    "unsupported compression type",
			encoder: NewEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 16, 1),
			buf:     buf(1, 8000, 0),
			wantErr: ErrFmtNotSupported,
		},
		{
			name:    "nil buffer",
			encoder: NewULawEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 1),
			buf:     nil,
			wantErr: nil,
		},
		{
			name:    "channel count mismatch",
			encoder: NewULawEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 2),
			buf:     buf(1, 8000, 0, 0),
			wantErr: ErrBufferMismatch,
		},
		{
			name:    "partial frame",
			encoder: NewULawEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 2),
			buf:     buf(2, 8000, 0, 0, 0),
			wantErr: ErrBufferMismatch,
		},
		{
			name:    "sample outside of the 16 bit range",
			encoder: NewULawEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 1),
			buf:     buf(1, 8000, 40000),
			wantErr: ErrInvalidSample,
		},
		{
			name:    "buffer bit depth mismatch",
			encoder: NewULawEncoder(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 1),
			buf:     &audio.IntBuffer{Format: &audio.Format{NumChannels: 1}, Data: []int{0}, SourceBitDepth: 8},
			wantErr: ErrBufferMismatch,
		},
	}

	// An encoder with a zero Encoding is a plain AIFF encoder, the
	// "unsupported compression type" case needs an AIFC tag.
	tests[2].encoder.Encoding = [4]byte{'a', 'l', 'a', 'w'}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.encoder.w.(*memWriteSeeker)
			err := tc.encoder.Write(tc.buf)
			if err == nil {
				t.Fatalf("expected an error, got none")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) && err.Error() != tc.wantErr.Error() {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if w.buf.Len() != 0 {
				t.Fatalf("a failed write must not write anything, wrote %d bytes", w.buf.Len())
			}
		})
	}
}

func TestNewEncoderWithEncoding(t *testing.T) {
	if _, err := NewEncoderWithEncoding(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 16, 1, encULAW); err != nil {
		t.Fatalf("the upper case ULAW encoding should be accepted: %v", err)
	}
	if _, err := NewEncoderWithEncoding(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 8, 1, envUlaw); err == nil {
		t.Fatal("8 bit input for μ-law should be rejected")
	} else if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("expected %v, got %v", ErrInvalidParams, err)
	}
	// A zero encoding is accepted and equivalent to NewEncoder.
	e, err := NewEncoderWithEncoding(&memWriteSeeker{buf: &bytes.Buffer{}}, 8000, 16, 1, encNotSet)
	if err != nil {
		t.Fatalf("a zero encoding should be accepted: %v", err)
	}
	if e.isAIFC() {
		t.Fatal("a zero encoding must produce a plain AIFF container")
	}
}

func TestULawEncoderCloseWithoutSamples(t *testing.T) {
	// Closing without writing must still produce a valid, empty container.
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 8000, 1)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	d := decoderFromBytes(w.buf.Bytes())
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	if d.NumSampleFrames != 0 || d.NumChans != 1 || d.Encoding != envUlaw {
		t.Fatalf("unexpected empty container: %d frames, %d channels, %q", d.NumSampleFrames, d.NumChans, d.Encoding)
	}
	buf, err := decoderFromBytes(w.buf.Bytes()).FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(buf.Data) != 0 {
		t.Fatalf("expected no samples, got %d", len(buf.Data))
	}

	// Writing after Close is refused.
	if err := e.Write(&audio.IntBuffer{Format: &audio.Format{NumChannels: 1}, Data: []int{0}}); err == nil {
		t.Fatal("writing to a closed encoder should fail")
	}
}

func TestULawEncoderOriginalAIFFUnchanged(t *testing.T) {
	// The historical NewEncoder path must still produce byte identical
	// output for the existing fixtures.
	f, err := os.Open(filepath.Join("fixtures", "kick8b.aiff"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewEncoder(w, int(d.SampleRate), int(d.BitDepth), int(d.NumChans))
	if err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	d2 := decoderFromBytes(w.buf.Bytes())
	d2.ReadInfo()
	if d2.Form != aiffID {
		t.Fatalf("expected a plain AIFF container, got %q", d2.Form)
	}
	// The plain AIFF COMM chunk still has its historical 18 byte size.
	if got := binary.BigEndian.Uint32(w.buf.Bytes()[16:20]); got != 18 {
		t.Fatalf("expected an 18 byte COMM chunk, got %d", got)
	}
}

func TestDecodeEncodeULaw(t *testing.T) {
	samples := []int{-32124, 0, 32124, -716, 5372}
	codeWords, err := EncodeULaw(samples)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0xFF, 0x80, 0x55, 0xAA}
	if !bytes.Equal(codeWords, want) {
		t.Fatalf("expected % x, got % x", want, codeWords)
	}
	decoded := DecodeULaw(want)
	if len(decoded) != len(samples) {
		t.Fatalf("expected %d samples, got %d", len(samples), len(decoded))
	}
	for i := range samples {
		if decoded[i] != samples[i] {
			t.Fatalf("sample %d expected %d, got %d", i, samples[i], decoded[i])
		}
	}
	if _, err := EncodeULaw([]int{40000}); !errors.Is(err, ErrInvalidSample) {
		t.Fatalf("expected %v for an out of range sample, got %v", ErrInvalidSample, err)
	}
}

func TestULawEncoderChunkedWrites(t *testing.T) {
	// Several Write calls must produce the same stream as a single write and
	// only Close patches the sizes.
	data := []int{1000, -1000, 2000, -2000, 3000, -3000, 4000, -4000, 5000}
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 8000, 1)
	for _, s := range data {
		if err := e.Write(&audio.IntBuffer{
			Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
			Data:   []int{s},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	d := decoderFromBytes(w.buf.Bytes())
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if d.NumSampleFrames != uint32(len(data)) {
		t.Fatalf("expected %d frames, got %d", len(data), d.NumSampleFrames)
	}
	if len(buf.Data) != len(data) {
		t.Fatalf("expected %d samples, got %d", len(data), len(buf.Data))
	}
	for i := range data {
		want := ulawDecodeSample(ulawEncodeSample(data[i]))
		if buf.Data[i] != want {
			t.Fatalf("sample %d expected %d, got %d", i, want, buf.Data[i])
		}
	}
	// A large odd sized payload exercises the pad byte across many buffers.
	var big []int
	for i := 0; i < 1001; i++ {
		big = append(big, i*31-15500)
	}
	w = &memWriteSeeker{buf: &bytes.Buffer{}}
	e = NewULawEncoder(w, 8000, 1)
	for i := 0; i < len(big); i += 100 {
		end := i + 100
		if end > len(big) {
			end = len(big)
		}
		if err := e.Write(&audio.IntBuffer{
			Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
			Data:   big[i:end],
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if len(w.buf.Bytes())%2 != 0 {
		t.Fatal("the container isn't even sized")
	}
	d = decoderFromBytes(w.buf.Bytes())
	if d.IsValidFile() != true {
		t.Fatalf("expected a valid file, got %v", d.Err())
	}
	buf, err = d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(buf.Data) != len(big) {
		t.Fatalf("expected %d samples, got %d", len(big), len(buf.Data))
	}
}

func TestULawDecoderRewind(t *testing.T) {
	w := &memWriteSeeker{buf: &bytes.Buffer{}}
	e := NewULawEncoder(w, 8000, 2)
	first := []int{1000, -1000, 2000, -2000}
	if err := e.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 8000},
		Data:   first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ulaw.aiff")
	if err := os.WriteFile(path, w.buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Rewind(); err != nil {
		t.Fatal(err)
	}
	again, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Data) != len(buf.Data) {
		t.Fatalf("expected %d samples after rewinding, got %d", len(buf.Data), len(again.Data))
	}
	for i := range buf.Data {
		if again.Data[i] != buf.Data[i] {
			t.Fatalf("sample %d differs after rewinding: %d vs %d", i, buf.Data[i], again.Data[i])
		}
	}
	if buf.SourceBitDepth != 16 {
		t.Fatalf("expected a 16 bit decoded source depth, got %d", buf.SourceBitDepth)
	}
}

func TestULawAccessors(t *testing.T) {
	data := ulawFixture(t, encULAW, 1, 2, 8000, []byte{0x7F, 0xFF})
	d := decoderFromBytes(data)
	if _, err := d.FullPCMBuffer(); err != nil {
		t.Fatal(err)
	}
	if !d.IsULaw() {
		t.Fatal("expected the decoder to report μ-law content")
	}
	if d.SampleBitDepth() != 8 {
		t.Fatalf("expected a stored bit depth of 8, got %d", d.SampleBitDepth())
	}
	if d.DecodedBitDepth() != 16 {
		t.Fatalf("expected a decoded bit depth of 16, got %d", d.DecodedBitDepth())
	}
	plain, err := os.Open(filepath.Join("fixtures", "kick.aif"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if NewDecoder(plain).IsULaw() {
		t.Fatal("a plain AIFF file isn't μ-law")
	}
	var nilDecoder *Decoder
	if nilDecoder.IsULaw() || nilDecoder.DecodedBitDepth() != 0 {
		t.Fatal("nil decoders must report zero values")
	}
}

func TestULawReEncodingIsStable(t *testing.T) {
	// Decoding μ-law content and writing it back must produce the same
	// container: the decoded values always re-encode to code words decoding
	// to themselves.
	var data []int
	for i := 0; i < 64; i++ {
		data = append(data, (i*577)%65536-32768)
	}
	encode := func(in []int) []byte {
		w := &memWriteSeeker{buf: &bytes.Buffer{}}
		e := NewULawEncoder(w, 8000, 1)
		if err := e.Write(&audio.IntBuffer{
			Format: &audio.Format{NumChannels: 1, SampleRate: 8000},
			Data:   in,
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), w.buf.Bytes()...)
	}
	first := encode(data)
	buf, err := decoderFromBytes(first).FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	second := encode(buf.Data)
	if !bytes.Equal(first, second) {
		t.Fatal("re-encoding decoded μ-law content isn't stable")
	}
}

func TestULawDecoderFixtures(t *testing.T) {
	// fixtures/ulaw.aiff is mono 8000 Hz with 16 known G.711 code words,
	// fixtures/ulaw_stereo.aiff is stereo 44100 Hz with distinct left and
	// right code words. Fixtures are left untouched: they are only read.
	tests := []struct {
		path     string
		encoding [4]byte
		chans    uint16
		frames   uint32
		rate     int
		want     []int
	}{
		{
			path:     "fixtures/ulaw.aiff",
			encoding: envUlaw,
			chans:    1,
			frames:   16,
			rate:     8000,
			want: []int{
				-32124, -31100, -5372, -716, -8, 0, 32124, 31100,
				5372, 716, 8, 0, -15996, 132, -1884, 1884,
			},
		},
		{
			path:     "fixtures/ulaw_stereo.aiff",
			encoding: encULAW,
			chans:    2,
			frames:   8,
			rate:     44100,
			want: []int{
				-32124, 0,
				-15996, 132,
				-7932, 396,
				-3900, 924,
				32124, 0,
				15996, -132,
				7932, -396,
				3900, -924,
			},
		},
	}

	for _, tc := range tests {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			f, err := os.Open(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := NewDecoder(f)
			if !d.IsValidFile() {
				t.Fatalf("%s isn't reported as valid", tc.path)
			}
			buf, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatal(err)
			}
			if d.Form != aifcID || d.Encoding != tc.encoding {
				t.Fatalf("unexpected container: form %q, encoding %q", d.Form, d.Encoding)
			}
			if d.NumChans != tc.chans || d.NumSampleFrames != tc.frames ||
				d.SampleRate != tc.rate || d.BitDepth != 8 {
				t.Fatalf("unexpected COMM values: %d ch, %d frames, %d Hz, %d bits",
					d.NumChans, d.NumSampleFrames, d.SampleRate, d.BitDepth)
			}
			if buf.SourceBitDepth != 16 {
				t.Fatalf("expected 16 bit decoded samples, got %d", buf.SourceBitDepth)
			}
			if len(buf.Data) != len(tc.want) {
				t.Fatalf("expected %d samples, got %d", len(tc.want), len(buf.Data))
			}
			for i := range tc.want {
				if buf.Data[i] != tc.want[i] {
					t.Fatalf("sample %d expected %d, got %d", i, tc.want[i], buf.Data[i])
				}
			}

			// Rewriting the decoded content as μ-law must produce the same
			// container.
			w := &memWriteSeeker{buf: &bytes.Buffer{}}
			e := NewULawEncoder(w, tc.rate, int(tc.chans))
			e.Encoding = tc.encoding
			if err := e.Write(buf); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, w.buf.Bytes()) {
				t.Fatalf("re-encoded container doesn't match the original fixture\noriginal:\n% x\nencoded:\n% x",
					original, w.buf.Bytes())
			}
		})
	}
}
