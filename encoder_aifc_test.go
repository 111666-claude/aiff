package aiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/go-audio/audio"
)

// readAIFCContainer parses an encoded AIFF-C sowt container from raw bytes.
func readAIFCContainer(t *testing.T, data []byte) (form []byte, comm, ssnd []byte) {
	t.Helper()
	if len(data) < 12 {
		t.Fatalf("container is only %d bytes long", len(data))
	}
	if string(data[:4]) != "FORM" {
		t.Fatalf("expected a FORM header, got %q", data[:4])
	}
	if got := binary.BigEndian.Uint32(data[4:8]); got != uint32(len(data)-8) {
		t.Fatalf("FORM size is %d, expected %d", got, len(data)-8)
	}
	if len(data)%2 != 0 {
		t.Fatalf("container size %d isn't even", len(data))
	}
	form = data[8:12]

	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		start := pos + 8
		if start+size > len(data) {
			t.Fatalf("%s chunk size %d overflows the container", id, size)
		}
		body := data[start : start+size]
		switch id {
		case "COMM":
			if size%2 != 0 {
				t.Fatalf("COMM chunk size %d isn't even", size)
			}
			comm = body
		case "SSND":
			ssnd = body
		default:
			t.Fatalf("unexpected chunk %q", id)
		}
		pos = start + size
		// odd sized chunks are followed by a pad byte that isn't counted
		// in the chunk size.
		if size%2 != 0 {
			if pos >= len(data) || data[pos] != 0 {
				t.Fatalf("%s chunk of size %d has no pad byte", id, size)
			}
			pos++
		}
	}
	if comm == nil {
		t.Fatal("container has no COMM chunk")
	}
	if ssnd == nil {
		t.Fatal("container has no SSND chunk")
	}
	return form, comm, ssnd
}

func decodeComm(t *testing.T, comm []byte) (chans uint16, frames uint32, depth uint16, rate int, encoding []byte, name string) {
	t.Helper()
	if len(comm) < 18 {
		t.Fatalf("COMM chunk is only %d bytes", len(comm))
	}
	chans = binary.BigEndian.Uint16(comm[0:2])
	frames = binary.BigEndian.Uint32(comm[2:6])
	depth = binary.BigEndian.Uint16(comm[6:8])
	var rateBytes [10]byte
	copy(rateBytes[:], comm[8:18])
	rate = audio.IEEEFloatToInt(rateBytes)
	if len(comm) < 23 {
		t.Fatalf("COMM chunk doesn't contain AIFF-C compression information")
	}
	encoding = comm[18:22]
	nameLen := int(comm[22])
	if len(comm) < 23+nameLen {
		t.Fatalf("COMM chunk compression name is truncated")
	}
	name = string(comm[23 : 23+nameLen])
	return chans, frames, depth, rate, encoding, name
}

func TestAIFCEncoderContainer(t *testing.T) {
	testCases := []struct {
		desc     string
		bitDepth int
		numChans int
		data     []int
	}{
		{"16 bit mono", 16, 1, []int{-32768, 0, 32767, -1}},
		{"16 bit stereo", 16, 2, []int{-32768, 32767, -1, 1, 0, 0}},
		{"24 bit mono", 24, 1, []int{-8388608, 8388607, -1, 0}},
		{"24 bit stereo", 24, 2, []int{-8388608, 8388607, -1, 1, 0, 0}},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			var out bytes.Buffer
			w := &writeSeeker{Buffer: &out}
			enc := NewAIFCEncoder(w, 44100, tc.bitDepth, tc.numChans)
			buf := &audio.IntBuffer{
				Format: &audio.Format{NumChannels: tc.numChans, SampleRate: 44100},
				Data:   tc.data,
			}
			if err := enc.Write(buf); err != nil {
				t.Fatal(err)
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}

			form, comm, ssnd := readAIFCContainer(t, out.Bytes())
			if string(form) != "AIFC" {
				t.Fatalf("expected an AIFC form type, got %q", form)
			}
			chans, frames, depth, rate, encoding, name := decodeComm(t, comm)
			if chans != uint16(tc.numChans) {
				t.Errorf("channel count: expected %d, got %d", tc.numChans, chans)
			}
			expectedFrames := len(tc.data) / tc.numChans
			if frames != uint32(expectedFrames) {
				t.Errorf("frame count: expected %d, got %d", expectedFrames, frames)
			}
			if depth != uint16(tc.bitDepth) {
				t.Errorf("bit depth: expected %d, got %d", tc.bitDepth, depth)
			}
			if rate != 44100 {
				t.Errorf("sample rate: expected 44100, got %d", rate)
			}
			if string(encoding) != "sowt" {
				t.Errorf("expected the sowt compression type, got %q", encoding)
			}
			if name != sowtCompressionName {
				t.Errorf("compression name: expected %q, got %q", sowtCompressionName, name)
			}
			if len(comm)%2 != 0 {
				t.Errorf("COMM chunk size %d isn't even", len(comm))
			}

			if len(ssnd) < 8 {
				t.Fatalf("SSND chunk is too short: %d bytes", len(ssnd))
			}
			if off := binary.BigEndian.Uint32(ssnd[0:4]); off != 0 {
				t.Errorf("expected an SSND offset of 0, got %d", off)
			}
			if blockSize := binary.BigEndian.Uint32(ssnd[4:8]); blockSize != 0 {
				t.Errorf("expected an SSND block size of 0, got %d", blockSize)
			}
			payload := ssnd[8:]
			expectedBytes := expectedFrames * tc.numChans * (tc.bitDepth / 8)
			if len(payload) != expectedBytes {
				t.Fatalf("expected %d sample bytes, got %d", expectedBytes, len(payload))
			}

			// verify the little endian sample bytes, including sign boundaries.
			sampleBytes := tc.bitDepth / 8
			for i, v := range tc.data {
				raw := payload[i*sampleBytes : (i+1)*sampleBytes]
				var got int
				switch tc.bitDepth {
				case 16:
					got = int(int16(binary.LittleEndian.Uint16(raw)))
				case 24:
					got = int(audio.Int24LETo32(raw))
				}
				if got != v {
					t.Errorf("sample %d: expected %d, got %d (bytes %x)", i, v, got, raw)
				}
			}
		})
	}
}

// TestAIFCEncoderRoundTrip writes boundary samples in multiple Write calls and
// reads them back with the package Decoder.
func TestAIFCEncoderRoundTrip(t *testing.T) {
	testCases := []struct {
		desc     string
		bitDepth int
		numChans int
	}{
		{"16 bit mono", 16, 1},
		{"16 bit stereo", 16, 2},
		{"24 bit mono", 24, 1},
		{"24 bit stereo", 24, 2},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			min := -(1 << (tc.bitDepth - 1))
			max := (1 << (tc.bitDepth - 1)) - 1
			// each Write carries one frame to exercise the deferred header
			// updates. 24 bit mono with an odd frame count also exercises
			// the trailing SSND pad byte.
			frames := [][]int{
				{min, max},
				{max, min},
				{-1, 0},
				{0, -1},
				{1, 1},
			}
			data := make([]int, 0, len(frames)*tc.numChans)
			for _, f := range frames {
				data = append(data, f[:tc.numChans]...)
			}

			var out bytes.Buffer
			enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 48000, tc.bitDepth, tc.numChans)
			for i := 0; i < len(data); i += tc.numChans {
				buf := &audio.IntBuffer{
					Format: &audio.Format{NumChannels: tc.numChans, SampleRate: 48000},
					Data:   data[i : i+tc.numChans],
				}
				if err := enc.Write(buf); err != nil {
					t.Fatalf("write of frame %d failed: %v", i/tc.numChans, err)
				}
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}

			dec := NewDecoder(bytes.NewReader(out.Bytes()))
			decoded, err := dec.FullPCMBuffer()
			if err != nil {
				t.Fatal(err)
			}
			if dec.Form != aifcID {
				t.Fatalf("expected the decoder to see AIFC, got %q", dec.Form)
			}
			if dec.Encoding != encSowt {
				t.Fatalf("expected the decoder to see sowt, got %q", dec.Encoding)
			}
			if int(dec.NumChans) != tc.numChans {
				t.Fatalf("channel count: expected %d, got %d", tc.numChans, dec.NumChans)
			}
			if int(dec.BitDepth) != tc.bitDepth {
				t.Fatalf("bit depth: expected %d, got %d", tc.bitDepth, dec.BitDepth)
			}
			if int(dec.SampleRate) != 48000 {
				t.Fatalf("sample rate: expected 48000, got %d", dec.SampleRate)
			}
			if int(dec.NumSampleFrames) != len(data)/tc.numChans {
				t.Fatalf("frame count header: expected %d, got %d", len(data)/tc.numChans, dec.NumSampleFrames)
			}
			if len(decoded.Data) != len(data) {
				t.Fatalf("decoded %d samples, expected %d", len(decoded.Data), len(data))
			}
			for i, expected := range data {
				if decoded.Data[i] != expected {
					t.Fatalf("sample %d: expected %d, got %d", i, expected, decoded.Data[i])
				}
			}

			// the FORM size must match what the decoder reports.
			info := out.Bytes()
			if got := binary.BigEndian.Uint32(info[4:8]); got != uint32(len(info)-8) {
				t.Fatalf("FORM size %d doesn't match the %d byte container", got, len(info))
			}
			if got := binary.BigEndian.Uint32(info[22:26]); got != uint32(len(data)/tc.numChans) {
				t.Fatalf("COMM frame count %d doesn't match %d frames", got, len(data)/tc.numChans)
			}
			_, _, ssnd := readAIFCContainer(t, info)
			expectedBytes := len(data) * (tc.bitDepth / 8)
			if len(ssnd) != 8+expectedBytes {
				t.Fatalf("SSND chunk holds %d bytes, expected %d", len(ssnd), 8+expectedBytes)
			}
			if got := binary.BigEndian.Uint32(ssnd[:4]); got != 0 {
				// SSND size declared in the chunk header is checked by the
				// container parser, this reads the offset field instead.
				t.Fatalf("unexpected SSND offset %d", got)
			}
		})
	}
}

func TestAIFCEncoderOddSizedDataIsPadded(t *testing.T) {
	var out bytes.Buffer
	// mono 24 bit with an odd number of frames produces odd byte data.
	enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 44100, 24, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		Data:   []int{-8388608, 8388607, 0},
	}
	if err := enc.Write(buf); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	if len(data)%2 != 0 {
		t.Fatalf("container size %d isn't even", len(data))
	}
	if got := binary.BigEndian.Uint32(data[4:8]); got != uint32(len(data)-8) {
		t.Fatalf("FORM size %d doesn't match the %d byte container", got, len(data)-8)
	}
}

func TestAIFCEncoderRejectsUnsupported(t *testing.T) {
	testCases := []struct {
		desc     string
		bitDepth int
		numChans int
	}{
		{"8 bit mono", 8, 1},
		{"32 bit mono", 32, 1},
		{"16 bit three channels", 16, 3},
		{"24 bit zero channels", 24, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			var out bytes.Buffer
			enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 44100, tc.bitDepth, tc.numChans)
			buf := &audio.IntBuffer{
				Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
				Data:   []int{0, 0},
			}
			if err := enc.Write(buf); err == nil {
				t.Fatal("expected Write to reject the encoder configuration")
			}
			if out.Len() != 0 {
				t.Fatalf("expected no data to be written, got %d bytes", out.Len())
			}
			if err := enc.Close(); err == nil {
				t.Fatal("expected Close to reject the encoder configuration")
			}
		})
	}
}

func TestAIFCEncoderRejectsMismatchedBuffer(t *testing.T) {
	testCases := []struct {
		desc            string
		format          *audio.Format
		data            []int
		sourceBitDepth  int
		expectedEncoder int
		chans           int
	}{
		{"nil format", nil, []int{0, 0}, 0, 16, 1},
		{"channel mismatch", &audio.Format{NumChannels: 2, SampleRate: 44100}, []int{0, 0}, 0, 16, 1},
		{"sample rate mismatch", &audio.Format{NumChannels: 1, SampleRate: 22050}, []int{0, 0}, 0, 16, 1},
		{"bit depth mismatch", &audio.Format{NumChannels: 1, SampleRate: 44100}, []int{0, 0}, 24, 16, 1},
		{"partial frame", &audio.Format{NumChannels: 2, SampleRate: 44100}, []int{0, 0, 0}, 0, 16, 2},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			var out bytes.Buffer
			enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 44100, tc.expectedEncoder, tc.chans)
			buf := &audio.IntBuffer{Format: tc.format, Data: tc.data, SourceBitDepth: tc.sourceBitDepth}
			if err := enc.Write(buf); err == nil {
				t.Fatal("expected Write to reject the mismatched buffer")
			}
			if out.Len() != 0 {
				t.Fatalf("expected no data to be written, got %d bytes", out.Len())
			}
		})
	}
}

func TestAIFCEncoderNilBuffer(t *testing.T) {
	var out bytes.Buffer
	enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 44100, 16, 1)
	if err := enc.Write(nil); err == nil {
		t.Fatal("expected Write to reject a nil buffer")
	}
	if out.Len() != 0 {
		t.Fatalf("expected no data to be written, got %d bytes", out.Len())
	}
}

// TestEncoderDefaultRemainsAIFF guards the original NewEncoder behavior.
func TestEncoderDefaultRemainsAIFF(t *testing.T) {
	var out bytes.Buffer
	enc := NewEncoder(&writeSeeker{Buffer: &out}, 44100, 16, 1)
	buf := &audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		Data:   []int{-32768, 32767},
	}
	if err := enc.Write(buf); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	if string(data[8:12]) != "AIFF" {
		t.Fatalf("expected the default encoder to write AIFF, got %q", data[8:12])
	}
	if string(data[12:16]) != "COMM" {
		t.Fatalf("expected a COMM chunk, got %q", data[12:16])
	}
	if got := binary.BigEndian.Uint32(data[16:20]); got != 18 {
		t.Fatalf("expected an 18 byte COMM chunk, got %d", got)
	}
	// big endian samples, the AIFF header is 38 bytes and the SSND chunk
	// header adds 16 more bytes.
	if data[54] != 0x80 || data[55] != 0x00 {
		t.Fatalf("expected big endian -32768, got %x", data[54:56])
	}
	if data[56] != 0x7f || data[57] != 0xff {
		t.Fatalf("expected big endian 32767, got %x", data[56:58])
	}
}

func TestAIFCEncoderFileRoundTrip(t *testing.T) {
	dir := "testOutput"
	if err := os.MkdirAll(dir, 0777); err != nil {
		t.Fatal(err)
	}
	path := dir + "/aifc_sowt_test.aifc"
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := NewAIFCEncoder(f, 44100, 16, 2)
	data := []int{-32768, 32767, -1, 1, 0, 0, 32767, -32768}
	if err := enc.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 2, SampleRate: 44100},
		Data:   data,
	}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatal("expected the encoded file to be a valid AIFF-C file")
	}
	if dec.Encoding != encSowt {
		t.Fatalf("expected sowt encoding, got %q", dec.Encoding)
	}
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(buf.Data) != len(data) {
		t.Fatalf("decoded %d samples, expected %d", len(buf.Data), len(data))
	}
	for i := range data {
		if buf.Data[i] != data[i] {
			t.Fatalf("sample %d: expected %d, got %d", i, data[i], buf.Data[i])
		}
	}
}

func TestAIFCEncoderCloseBeforeWrite(t *testing.T) {
	var out bytes.Buffer
	enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 44100, 16, 1)
	if err := enc.Close(); err == nil {
		t.Fatal("expected Close without Write to fail")
	}
}

// TestAIFCEncoderStreamingDecode reads the encoded samples through the
// streaming PCMBuffer API, including an odd sized SSND chunk.
func TestAIFCEncoderStreamingDecode(t *testing.T) {
	data := []int{-8388608, 8388607, -1, 0, 1}
	var out bytes.Buffer
	enc := NewAIFCEncoder(&writeSeeker{Buffer: &out}, 44100, 24, 1)
	if err := enc.Write(&audio.IntBuffer{
		Format: &audio.Format{NumChannels: 1, SampleRate: 44100},
		Data:   data,
	}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(bytes.NewReader(out.Bytes()))
	dec.ReadInfo()
	got := make([]int, 0, 8)
	buf := &audio.IntBuffer{Data: make([]int, 2)}
	for {
		n, err := dec.PCMBuffer(buf)
		if n > 0 {
			got = append(got, buf.Data[:n]...)
		}
		if err != nil {
			t.Fatalf("streaming decode failed: %v", err)
		}
		if n == 0 {
			break
		}
	}
	if len(got) != len(data) {
		t.Fatalf("streaming decode returned %d samples, expected %d (%v)", len(got), len(data), got)
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("sample %d: expected %d, got %d", i, data[i], got[i])
		}
	}
}

// writeSeeker adapts a bytes.Buffer to io.WriteSeeker for the encoder tests,
// allowing the header fields written by Close to be patched in place.
type writeSeeker struct {
	*bytes.Buffer
	pos int64
}

func (w *writeSeeker) Write(p []byte) (int, error) {
	if w.pos > int64(w.Buffer.Len()) {
		w.Buffer.Write(make([]byte, w.pos-int64(w.Buffer.Len())))
	}
	if w.pos < int64(w.Buffer.Len()) {
		copy(w.Buffer.Bytes()[w.pos:], p)
		if int(w.pos)+len(p) > w.Buffer.Len() {
			w.Buffer.Write(p[w.Buffer.Len()-int(w.pos):])
		}
		w.pos += int64(len(p))
		return len(p), nil
	}
	n, err := w.Buffer.Write(p)
	w.pos += int64(n)
	return n, err
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = w.pos + offset
	case io.SeekEnd:
		next = int64(w.Buffer.Len()) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if next < 0 {
		return 0, errors.New("negative position")
	}
	w.pos = next
	return next, nil
}
