package aiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/go-audio/audio"
)

// memWriteSeeker is an in-memory io.WriteSeeker used to exercise the
// encoder without touching the disk.
type memWriteSeeker struct {
	buf []byte
	pos int
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.pos + len(p)
	if end > len(m.buf) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(m.pos) + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("unknown whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek position %d", abs)
	}
	m.pos = int(abs)
	return abs, nil
}

func intBuf(format *audio.Format, data []int) *audio.IntBuffer {
	return &audio.IntBuffer{Format: format, Data: data, SourceBitDepth: 16}
}

// TestMarkerCommentRoundTrip encodes mono and stereo files carrying multiple
// markers and comments (odd and even length names/texts, associated and
// free-standing comments), writing the audio in several Write calls, and
// checks that the decoder reads every field back unchanged.
func TestMarkerCommentRoundTrip(t *testing.T) {
	testCases := []struct {
		desc     string
		numChans int
	}{
		{"mono", 1},
		{"stereo", 2},
	}
	markers := []Marker{
		{ID: 1, Position: 0, Name: "start"},      // 5 chars, no pad
		{ID: 2, Position: 4, Name: "foot"},       // 4 chars, Pascal pad
		{ID: 3, Position: 9, Name: "door slam!"}, // 10 chars, Pascal pad
		{ID: 4, Position: 12, Name: "e"},         // 1 char, no pad
	}
	comments := []Comment{
		{Timestamp: 3600, MarkerID: 2, Text: "footstep begins"}, // 15 chars, odd
		{Timestamp: 7200, MarkerID: 3, Text: "door"},            // 4 chars, even
		{Timestamp: 0, MarkerID: 0, Text: "mix notes"},          // not associated
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			const frames = 16
			format := &audio.Format{NumChannels: tc.numChans, SampleRate: 44100}
			samples := make([]int, frames*tc.numChans)
			for i := range samples {
				samples[i] = i*101 - 700
			}

			w := &memWriteSeeker{}
			e := NewEncoder(w, 44100, 16, tc.numChans)
			for _, m := range markers {
				if err := e.AddMarker(m); err != nil {
					t.Fatalf("failed to add marker %+v - %v", m, err)
				}
			}
			for _, c := range comments {
				if err := e.AddComment(c); err != nil {
					t.Fatalf("failed to add comment %+v - %v", c, err)
				}
			}
			// write the audio in multiple passes
			perWrite := 5 * tc.numChans
			for i := 0; i < len(samples); i += perWrite {
				end := i + perWrite
				if end > len(samples) {
					end = len(samples)
				}
				if err := e.Write(intBuf(format, samples[i:end])); err != nil {
					t.Fatalf("write failed - %v", err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatalf("close failed - %v", err)
			}

			d := NewDecoder(bytes.NewReader(w.buf))
			if err := d.Drain(); err != nil {
				t.Fatalf("drain failed - %v", err)
			}

			if len(d.Markers) != len(markers) {
				t.Fatalf("expected %d markers, got %d", len(markers), len(d.Markers))
			}
			for i, m := range markers {
				if d.Markers[i] != m {
					t.Errorf("marker %d didn't roundtrip, expected %+v, got %+v", i, m, d.Markers[i])
				}
			}
			if len(d.CommentDetails) != len(comments) {
				t.Fatalf("expected %d comments, got %d", len(comments), len(d.CommentDetails))
			}
			for i, c := range comments {
				if d.CommentDetails[i] != c {
					t.Errorf("comment %d didn't roundtrip, expected %+v, got %+v", i, c, d.CommentDetails[i])
				}
				if d.Comments[i] != c.Text {
					t.Errorf("comment text %d didn't roundtrip, expected %q, got %q", i, c.Text, d.Comments[i])
				}
			}

			// the sound data must be intact
			d.Rewind()
			buf, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatalf("failed to read PCM data - %v", err)
			}
			if buf.NumFrames() != frames {
				t.Fatalf("expected %d frames, got %d", frames, buf.NumFrames())
			}
			for i, v := range samples {
				if buf.Data[i] != v {
					t.Fatalf("sample %d didn't roundtrip, expected %d, got %d", i, v, buf.Data[i])
				}
			}
			if uint32(d.NumSampleFrames) != frames {
				t.Fatalf("expected COMM to report %d frames, got %d", frames, d.NumSampleFrames)
			}
		})
	}
}

// TestMarkerCommentRawBytes verifies the exact byte layout of the MARK and
// COMT chunks: Pascal strings, chunk sizes, even byte padding and the FORM
// total size.
func TestMarkerCommentRawBytes(t *testing.T) {
	w := &memWriteSeeker{}
	e := NewEncoder(w, 44100, 16, 1)
	if err := e.AddMarker(Marker{ID: 1, Position: 3, Name: "hit"}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddMarker(Marker{ID: 2, Position: 5, Name: "door"}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddComment(Comment{Timestamp: 0x01020304, MarkerID: 2, Text: "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddComment(Comment{Timestamp: 5, MarkerID: 0, Text: "free"}); err != nil {
		t.Fatal(err)
	}
	format := &audio.Format{NumChannels: 1, SampleRate: 44100}
	if err := e.Write(intBuf(format, []int{1, 2, 3, 4, 5, 6})); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data := w.buf

	// FORM total size
	formSize := binary.BigEndian.Uint32(data[4:8])
	if int(formSize) != len(data)-8 {
		t.Errorf("FORM size should be %d, got %d", len(data)-8, formSize)
	}

	// MARK chunk
	mi := bytes.Index(data, []byte("MARK"))
	if mi < 0 {
		t.Fatal("MARK chunk not found")
	}
	// 2 (count) + (2+4+1+3) + (2+4+1+4+1 pad) = 24
	expectedMark := []byte{
		'M', 'A', 'R', 'K',
		0, 0, 0, 24, // chunk size
		0, 2, // 2 markers
		0, 1, // ID 1
		0, 0, 0, 3, // position 3
		3, 'h', 'i', 't', // Pascal string, even, no pad
		0, 2, // ID 2
		0, 0, 0, 5, // position 5
		4, 'd', 'o', 'o', 'r', 0, // Pascal string padded to even
	}
	if !bytes.Equal(data[mi:mi+len(expectedMark)], expectedMark) {
		t.Errorf("unexpected MARK chunk bytes\nexpected: % x\ngot:      % x", expectedMark, data[mi:mi+len(expectedMark)])
	}

	// COMT chunk
	ci := bytes.Index(data, []byte("COMT"))
	if ci < 0 {
		t.Fatal("COMT chunk not found")
	}
	// 2 (count) + (4+2+2+3) + (4+2+2+4) = 25, odd -> 1 pad byte after the chunk
	expectedComt := []byte{
		'C', 'O', 'M', 'T',
		0, 0, 0, 25, // chunk size (pad byte not counted)
		0, 2, // 2 comments
		1, 2, 3, 4, // timestamp
		0, 2, // marker ID
		0, 3, 'a', 'b', 'c', // text
		0, 0, 0, 5, // timestamp
		0, 0, // no associated marker
		0, 4, 'f', 'r', 'e', 'e', // text
		0, // chunk pad byte
	}
	if !bytes.Equal(data[ci:ci+len(expectedComt)], expectedComt) {
		t.Errorf("unexpected COMT chunk bytes\nexpected: % x\ngot:      % x", expectedComt, data[ci:ci+len(expectedComt)])
	}

	// SSND chunk follows the metadata, untouched by Close
	si := bytes.Index(data, []byte("SSND"))
	if si != ci+len(expectedComt) {
		t.Errorf("SSND chunk should start right after the padded COMT chunk (%d), found at %d", ci+len(expectedComt), si)
	}
	ssndSize := binary.BigEndian.Uint32(data[si+4 : si+8])
	if ssndSize != 8+6*2 {
		t.Errorf("SSND size should be %d, got %d", 8+6*2, ssndSize)
	}
	// COMM frame count patched by Close
	frames := binary.BigEndian.Uint32(data[22:26])
	if frames != 6 {
		t.Errorf("COMM frame count should be 6, got %d", frames)
	}
}

// TestEncoderWithoutMetadata checks that not registering markers or comments
// leaves the output layout exactly as before: FORM, COMM, SSND and nothing else.
func TestEncoderWithoutMetadata(t *testing.T) {
	w := &memWriteSeeker{}
	e := NewEncoder(w, 22050, 16, 1)
	format := &audio.Format{NumChannels: 1, SampleRate: 22050}
	if err := e.Write(intBuf(format, []int{1, -2, 3, -4})); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data := w.buf
	if bytes.Contains(data, []byte("MARK")) || bytes.Contains(data, []byte("COMT")) {
		t.Fatal("no MARK/COMT chunk should be written when no metadata was registered")
	}
	// FORM(12) + COMM(8+18) + SSND(8+8+4*2) = 62
	if len(data) != 62 {
		t.Fatalf("expected a 62 byte file, got %d", len(data))
	}
	if string(data[12:16]) != "COMM" {
		t.Errorf("expected COMM right after the FORM header, got %q", data[12:16])
	}
	if string(data[38:42]) != "SSND" {
		t.Errorf("expected SSND right after COMM, got %q", data[38:42])
	}
}

func TestEncoderMarkerCommentErrors(t *testing.T) {
	format := &audio.Format{NumChannels: 1, SampleRate: 44100}

	t.Run("duplicate marker ID", func(t *testing.T) {
		e := NewEncoder(&memWriteSeeker{}, 44100, 16, 1)
		if err := e.AddMarker(Marker{ID: 1, Name: "a"}); err != nil {
			t.Fatal(err)
		}
		err := e.AddMarker(Marker{ID: 1, Name: "b"})
		if !errors.Is(err, ErrDuplicateMarkerID) {
			t.Fatalf("expected ErrDuplicateMarkerID, got %v", err)
		}
	})

	t.Run("marker name too long", func(t *testing.T) {
		e := NewEncoder(&memWriteSeeker{}, 44100, 16, 1)
		err := e.AddMarker(Marker{ID: 1, Name: strings.Repeat("x", 256)})
		if !errors.Is(err, ErrMarkerNameTooLong) {
			t.Fatalf("expected ErrMarkerNameTooLong, got %v", err)
		}
	})

	t.Run("comment text too long", func(t *testing.T) {
		e := NewEncoder(&memWriteSeeker{}, 44100, 16, 1)
		err := e.AddComment(Comment{Text: strings.Repeat("x", 0xffff+1)})
		if !errors.Is(err, ErrCommentTextTooLong) {
			t.Fatalf("expected ErrCommentTextTooLong, got %v", err)
		}
	})

	t.Run("unknown comment marker reference", func(t *testing.T) {
		e := NewEncoder(&memWriteSeeker{}, 44100, 16, 1)
		if err := e.AddMarker(Marker{ID: 1, Name: "a"}); err != nil {
			t.Fatal(err)
		}
		if err := e.AddComment(Comment{MarkerID: 42, Text: "dangling"}); err != nil {
			t.Fatal(err)
		}
		err := e.Write(intBuf(format, []int{1, 2, 3, 4}))
		if !errors.Is(err, ErrUnknownMarkerID) {
			t.Fatalf("expected ErrUnknownMarkerID, got %v", err)
		}
	})

	t.Run("marker position out of range", func(t *testing.T) {
		e := NewEncoder(&memWriteSeeker{}, 44100, 16, 1)
		if err := e.AddMarker(Marker{ID: 1, Position: 100, Name: "too far"}); err != nil {
			t.Fatal(err)
		}
		if err := e.Write(intBuf(format, []int{1, 2, 3, 4})); err != nil {
			t.Fatal(err)
		}
		err := e.Close()
		if !errors.Is(err, ErrMarkerPositionOutOfRange) {
			t.Fatalf("expected ErrMarkerPositionOutOfRange, got %v", err)
		}
	})
}

// testAIFF builds a minimal AIFF file with the given raw chunks inserted
// between COMM and SSND.
func testAIFF(t *testing.T, numFrames uint32, chunks ...[]byte) []byte {
	t.Helper()
	body := new(bytes.Buffer)
	// COMM
	body.Write(COMMID[:])
	binary.Write(body, binary.BigEndian, uint32(18))
	binary.Write(body, binary.BigEndian, uint16(1))  // channels
	binary.Write(body, binary.BigEndian, numFrames)  // sample frames
	binary.Write(body, binary.BigEndian, uint16(16)) // bit depth
	sr := audio.IntToIEEEFloat(44100)
	body.Write(sr[:]) // sample rate
	for _, c := range chunks {
		body.Write(c)
	}
	// SSND
	body.Write(SSNDID[:])
	binary.Write(body, binary.BigEndian, uint32(8+numFrames*2))
	binary.Write(body, binary.BigEndian, uint32(0)) // offset
	binary.Write(body, binary.BigEndian, uint32(0)) // block size
	body.Write(make([]byte, numFrames*2))

	out := new(bytes.Buffer)
	out.Write(formID[:])
	binary.Write(out, binary.BigEndian, uint32(4+body.Len()))
	out.Write(aiffID[:])
	out.Write(body.Bytes())
	return out.Bytes()
}

func rawChunk(id [4]byte, data []byte) []byte {
	buf := new(bytes.Buffer)
	buf.Write(id[:])
	binary.Write(buf, binary.BigEndian, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}

func TestDecoderMarkerCommentErrors(t *testing.T) {
	t.Run("truncated MARK chunk", func(t *testing.T) {
		// announces 2 markers but only holds part of the first one
		mark := rawChunk(MARKID, []byte{0, 2, 0, 1, 0})
		d := NewDecoder(bytes.NewReader(testAIFF(t, 8, mark)))
		if err := d.Drain(); err == nil || !strings.Contains(err.Error(), "truncated MARK") {
			t.Fatalf("expected a truncated MARK error, got %v", err)
		}
	})

	t.Run("truncated MARK name", func(t *testing.T) {
		// the name length announces more bytes than the chunk holds
		mark := rawChunk(MARKID, []byte{0, 1, 0, 1, 0, 0, 0, 0, 10, 'a', 'b'})
		d := NewDecoder(bytes.NewReader(testAIFF(t, 8, mark)))
		if err := d.Drain(); err == nil || !strings.Contains(err.Error(), "truncated MARK") {
			t.Fatalf("expected a truncated MARK error, got %v", err)
		}
	})

	t.Run("truncated COMT chunk", func(t *testing.T) {
		// announces 1 comment but the text is missing
		comt := rawChunk(COMTID, []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 5})
		d := NewDecoder(bytes.NewReader(testAIFF(t, 8, comt)))
		if err := d.Drain(); err == nil || !strings.Contains(err.Error(), "truncated COMT") {
			t.Fatalf("expected a truncated COMT error, got %v", err)
		}
	})

	t.Run("duplicate marker IDs", func(t *testing.T) {
		markData := []byte{0, 2}
		markData = append(markData, 0, 1, 0, 0, 0, 0, 1, 'a') // id 1
		markData = append(markData, 0, 1, 0, 0, 0, 0, 1, 'b') // id 1 again
		d := NewDecoder(bytes.NewReader(testAIFF(t, 8, rawChunk(MARKID, markData))))
		if err := d.Drain(); !errors.Is(err, ErrDuplicateMarkerID) {
			t.Fatalf("expected ErrDuplicateMarkerID, got %v", err)
		}
	})

	t.Run("marker position out of range", func(t *testing.T) {
		markData := []byte{0, 1, 0, 1, 0, 0, 0, 100, 1, 'a'}
		d := NewDecoder(bytes.NewReader(testAIFF(t, 8, rawChunk(MARKID, markData))))
		if err := d.Drain(); !errors.Is(err, ErrMarkerPositionOutOfRange) {
			t.Fatalf("expected ErrMarkerPositionOutOfRange, got %v", err)
		}
	})

	t.Run("comment with unknown marker ID", func(t *testing.T) {
		comtData := []byte{0, 1, 0, 0, 0, 0, 0, 7, 0, 2, 'h', 'i'}
		d := NewDecoder(bytes.NewReader(testAIFF(t, 8, rawChunk(COMTID, comtData))))
		if err := d.Drain(); !errors.Is(err, ErrUnknownMarkerID) {
			t.Fatalf("expected ErrUnknownMarkerID, got %v", err)
		}
	})
}

// TestDecoderMarkerFixture makes sure the MARK chunk of a real-world file
// (Logic export) is parsed.
func TestDecoderMarkerFixture(t *testing.T) {
	d := NewDecoder(bytes.NewReader(testAIFFWithMarkFixture(t)))
	if err := d.Drain(); err != nil {
		t.Fatalf("drain failed - %v", err)
	}
	if len(d.Markers) != 2 {
		t.Fatalf("expected 2 markers, got %d", len(d.Markers))
	}
	if d.Markers[0] != (Marker{ID: 1, Position: 0, Name: "Tempo: 98.0"}) {
		t.Errorf("unexpected first marker %+v", d.Markers[0])
	}
	if d.Markers[1] != (Marker{ID: 2, Position: 0, Name: "Timestamp: 158848064"}) {
		t.Errorf("unexpected second marker %+v", d.Markers[1])
	}
}

func testAIFFWithMarkFixture(t *testing.T) []byte {
	// the MARK chunk bytes found in fixtures/ring.aif
	markData := []byte{0, 2}
	markData = append(markData, 0, 1, 0, 0, 0, 0)
	markData = append(markData, 11)
	markData = append(markData, []byte("Tempo: 98.0")...)
	markData = append(markData, 0, 2, 0, 0, 0, 0)
	markData = append(markData, 20)
	markData = append(markData, []byte("Timestamp: 158848064")...)
	markData = append(markData, 0) // Pascal string pad byte
	return testAIFF(t, 8, rawChunk(MARKID, markData))
}
