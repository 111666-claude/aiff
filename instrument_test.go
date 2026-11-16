package aiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-audio/audio"
)

func testMarkers() []Marker {
	return []Marker{
		{ID: 1, Position: 0, Name: "start"},
		{ID: 2, Position: 256, Name: "lp"}, // even length name -> padded pstring
		{ID: 3, Position: 512, Name: "rel"},
		{ID: 4, Position: 768, Name: "end"},
	}
}

func testInstrument() *Instrument {
	return &Instrument{
		BaseNote:     60,
		Detune:       -3,
		LowNote:      0,
		HighNote:     127,
		LowVelocity:  1,
		HighVelocity: 127,
		Gain:         -6,
		SustainLoop: InstrumentLoop{
			PlayMode:      LoopModeForward,
			BeginMarkerID: 1,
			EndMarkerID:   2,
		},
		ReleaseLoop: InstrumentLoop{
			PlayMode:      LoopModeForwardBackward,
			BeginMarkerID: 3,
			EndMarkerID:   4,
		},
	}
}

func testPCMBuffer(numChans, frames int) *audio.IntBuffer {
	buf := &audio.IntBuffer{
		Format:         &audio.Format{NumChannels: numChans, SampleRate: 44100},
		Data:           make([]int, numChans*frames),
		SourceBitDepth: 16,
	}
	for i := range buf.Data {
		buf.Data[i] = (i*7919)%60000 - 30000
	}
	return buf
}

// TestInstrumentRoundTrip encodes mono and stereo files with MARK and INST
// chunks, writing the PCM in multiple Write calls, and verifies the raw
// bytes, the chunk order, the padding and the decode round trip.
func TestInstrumentRoundTrip(t *testing.T) {
	testCases := []struct {
		name     string
		numChans int
	}{
		{"mono", 1},
		{"stereo", 2},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inst.aiff")
			out, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			e := NewEncoder(out, 44100, 16, tc.numChans)
			e.Markers = testMarkers()
			e.Instrument = testInstrument()

			buf := testPCMBuffer(tc.numChans, 1000)
			// write the PCM in two passes to make sure Close still
			// backfills the FORM and SSND sizes properly.
			half := len(buf.Data) / 2
			if err := e.Write(&audio.IntBuffer{Format: buf.Format, Data: buf.Data[:half]}); err != nil {
				t.Fatal(err)
			}
			if err := e.Write(&audio.IntBuffer{Format: buf.Format, Data: buf.Data[half:]}); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// FORM size must have been backfilled to file size - 8
			if got := binary.BigEndian.Uint32(raw[4:8]); got != uint32(len(raw)-8) {
				t.Fatalf("FORM size wasn't backfilled properly, expected %d, got %d", len(raw)-8, got)
			}

			// chunk order: COMM, MARK, INST, SSND
			commPos := bytes.Index(raw, []byte("COMM"))
			markPos := bytes.Index(raw, []byte("MARK"))
			instPos := bytes.Index(raw, []byte("INST"))
			ssndPos := bytes.Index(raw, []byte("SSND"))
			if commPos < 0 || markPos < 0 || instPos < 0 || ssndPos < 0 {
				t.Fatalf("missing chunk(s) in encoded file: COMM=%d MARK=%d INST=%d SSND=%d", commPos, markPos, instPos, ssndPos)
			}
			if !(commPos < markPos && markPos < instPos && instPos < ssndPos) {
				t.Fatalf("unexpected chunk order: COMM=%d MARK=%d INST=%d SSND=%d", commPos, markPos, instPos, ssndPos)
			}

			// the MARK chunk data size must be even and INST must start
			// right after MARK data (even byte alignment, no dangling pad)
			markSize := int(binary.BigEndian.Uint32(raw[markPos+4 : markPos+8]))
			if markSize%2 != 0 {
				t.Fatalf("MARK chunk data size should be even, got %d", markSize)
			}
			if markPos+8+markSize != instPos {
				t.Fatalf("INST chunk should immediately follow the MARK data, MARK at %d (size %d), INST at %d", markPos, markSize, instPos)
			}
			// the even-length marker name "lp" must be followed by a pad byte
			lpPos := bytes.Index(raw, []byte{2, 'l', 'p'})
			if lpPos < 0 {
				t.Fatal("couldn't find the 'lp' marker name in the MARK chunk")
			}
			if raw[lpPos+3] != 0 {
				t.Fatalf("expected a zero pad byte after the 'lp' marker name, got %#x", raw[lpPos+3])
			}

			// raw INST chunk bytes
			expectedINST := []byte{
				'I', 'N', 'S', 'T', 0, 0, 0, 20,
				60, 0xFD, 0, 127, 1, 127, 0xFF, 0xFA,
				0, 1, 0, 1, 0, 2, // sustain: forward, markers 1-2
				0, 2, 0, 3, 0, 4, // release: forward/backward, markers 3-4
			}
			if !bytes.Equal(raw[instPos:instPos+len(expectedINST)], expectedINST) {
				t.Fatalf("INST chunk raw bytes mismatch\nexpected: %v\ngot:      %v", expectedINST, raw[instPos:instPos+len(expectedINST)])
			}

			// decode the file back
			nf, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer nf.Close()
			d := NewDecoder(nf)
			if err := d.Drain(); err != nil {
				t.Fatalf("failed to drain the encoded file - %v", err)
			}
			if !reflect.DeepEqual(d.Markers, testMarkers()) {
				t.Fatalf("markers didn't round trip\nexpected: %+v\ngot:      %+v", testMarkers(), d.Markers)
			}
			if d.Instrument == nil {
				t.Fatal("expected the decoded file to have an instrument")
			}
			if !reflect.DeepEqual(d.Instrument, testInstrument()) {
				t.Fatalf("instrument didn't round trip\nexpected: %+v\ngot:      %+v", testInstrument(), d.Instrument)
			}

			// PCM round trip
			d.Reset()
			pcm, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pcm.Data, buf.Data) {
				t.Fatal("PCM data didn't round trip")
			}
			if d.NumSampleFrames != 1000 {
				t.Fatalf("expected 1000 sample frames, got %d", d.NumSampleFrames)
			}
		})
	}
}

// testCOMMChunk builds a minimal valid COMM chunk (mono, 16bit, 44100Hz).
func testCOMMChunk() []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("COMM")
	binary.Write(buf, binary.BigEndian, uint32(18))
	binary.Write(buf, binary.BigEndian, uint16(1))
	binary.Write(buf, binary.BigEndian, uint32(0))
	binary.Write(buf, binary.BigEndian, uint16(16))
	rate := audio.IntToIEEEFloat(44100)
	buf.Write(rate[:])
	return buf.Bytes()
}

func testSSNDChunk() []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("SSND")
	binary.Write(buf, binary.BigEndian, uint32(8))
	binary.Write(buf, binary.BigEndian, uint32(0))
	binary.Write(buf, binary.BigEndian, uint32(0))
	return buf.Bytes()
}

func markChunkBytes(markers []Marker) []byte {
	data := new(bytes.Buffer)
	binary.Write(data, binary.BigEndian, uint16(len(markers)))
	for _, m := range markers {
		binary.Write(data, binary.BigEndian, m.ID)
		binary.Write(data, binary.BigEndian, m.Position)
		data.WriteByte(uint8(len(m.Name)))
		data.WriteString(m.Name)
		if (len(m.Name)+1)%2 != 0 {
			data.WriteByte(0)
		}
	}
	buf := new(bytes.Buffer)
	buf.WriteString("MARK")
	binary.Write(buf, binary.BigEndian, uint32(data.Len()))
	buf.Write(data.Bytes())
	return buf.Bytes()
}

func instChunkBytes(inst *Instrument) []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("INST")
	binary.Write(buf, binary.BigEndian, uint32(20))
	buf.WriteByte(inst.BaseNote)
	buf.WriteByte(byte(inst.Detune))
	buf.WriteByte(inst.LowNote)
	buf.WriteByte(inst.HighNote)
	buf.WriteByte(inst.LowVelocity)
	buf.WriteByte(inst.HighVelocity)
	binary.Write(buf, binary.BigEndian, inst.Gain)
	binary.Write(buf, binary.BigEndian, inst.SustainLoop.PlayMode)
	binary.Write(buf, binary.BigEndian, inst.SustainLoop.BeginMarkerID)
	binary.Write(buf, binary.BigEndian, inst.SustainLoop.EndMarkerID)
	binary.Write(buf, binary.BigEndian, inst.ReleaseLoop.PlayMode)
	binary.Write(buf, binary.BigEndian, inst.ReleaseLoop.BeginMarkerID)
	binary.Write(buf, binary.BigEndian, inst.ReleaseLoop.EndMarkerID)
	return buf.Bytes()
}

func buildAIFFFile(chunks ...[]byte) []byte {
	body := new(bytes.Buffer)
	body.WriteString("AIFF")
	for _, c := range chunks {
		body.Write(c)
	}
	out := new(bytes.Buffer)
	out.WriteString("FORM")
	binary.Write(out, binary.BigEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// TestInstrumentDecodeErrors makes sure malformed INST/MARK content is
// reported with explicit errors.
func TestInstrumentDecodeErrors(t *testing.T) {
	validInst := func() *Instrument {
		return &Instrument{
			BaseNote: 60, HighNote: 127, HighVelocity: 127,
			SustainLoop: InstrumentLoop{PlayMode: LoopModeForward, BeginMarkerID: 1, EndMarkerID: 2},
		}
	}

	testCases := []struct {
		name   string
		file   []byte
		errStr string
	}{
		{
			"truncated INST chunk",
			buildAIFFFile(testCOMMChunk(), []byte{'I', 'N', 'S', 'T', 0, 0, 0, 10, 60, 0, 0, 127, 1, 127, 0, 0}, testSSNDChunk()),
			"truncated INST chunk",
		},
		{
			"duplicate INST chunk",
			buildAIFFFile(testCOMMChunk(), instChunkBytes(validInst()), instChunkBytes(validInst()), testSSNDChunk()),
			"duplicate INST chunk",
		},
		{
			"inverted note range",
			buildAIFFFile(testCOMMChunk(), instChunkBytes(&Instrument{BaseNote: 60, LowNote: 100, HighNote: 20, HighVelocity: 127}), testSSNDChunk()),
			"note range is inverted",
		},
		{
			"inverted velocity range",
			buildAIFFFile(testCOMMChunk(), instChunkBytes(&Instrument{BaseNote: 60, HighNote: 127, LowVelocity: 90, HighVelocity: 10}), testSSNDChunk()),
			"velocity range is inverted",
		},
		{
			"illegal loop play mode",
			buildAIFFFile(testCOMMChunk(), instChunkBytes(&Instrument{
				BaseNote: 60, HighNote: 127, HighVelocity: 127,
				SustainLoop: InstrumentLoop{PlayMode: 9, BeginMarkerID: 1, EndMarkerID: 2},
			}), testSSNDChunk()),
			"illegal play mode",
		},
		{
			"loop referencing a missing marker",
			buildAIFFFile(testCOMMChunk(), instChunkBytes(validInst()), testSSNDChunk()),
			"missing marker ID 1",
		},
		{
			"truncated MARK chunk",
			buildAIFFFile(testCOMMChunk(), []byte{'M', 'A', 'R', 'K', 0, 0, 0, 2, 0, 1}, testSSNDChunk()),
			"truncated MARK chunk",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder(bytes.NewReader(tc.file))
			err := d.Drain()
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.errStr)
			}
			if !strings.Contains(err.Error(), tc.errStr) {
				t.Fatalf("expected an error containing %q, got %q", tc.errStr, err.Error())
			}
		})
	}

	// the duplicate INST error should be identifiable
	d := NewDecoder(bytes.NewReader(buildAIFFFile(testCOMMChunk(), instChunkBytes(validInst()), instChunkBytes(validInst()), testSSNDChunk())))
	if err := d.Drain(); !errors.Is(err, ErrDuplicateInstrument) {
		t.Fatalf("expected ErrDuplicateInstrument, got %v", err)
	}
}

// TestInstrumentEncodeErrors makes sure the encoder refuses to write
// instruments with invalid ranges, modes or dangling marker references.
func TestInstrumentEncodeErrors(t *testing.T) {
	testCases := []struct {
		name       string
		markers    []Marker
		instrument *Instrument
		errStr     string
	}{
		{
			"dangling loop reference",
			nil,
			&Instrument{BaseNote: 60, HighNote: 127, HighVelocity: 127,
				SustainLoop: InstrumentLoop{PlayMode: LoopModeForward, BeginMarkerID: 1, EndMarkerID: 2}},
			"missing marker ID 1",
		},
		{
			"inverted note range",
			testMarkers(),
			&Instrument{BaseNote: 60, LowNote: 127, HighNote: 0, HighVelocity: 127},
			"note range is inverted",
		},
		{
			"illegal release loop mode",
			testMarkers(),
			&Instrument{BaseNote: 60, HighNote: 127, HighVelocity: 127,
				ReleaseLoop: InstrumentLoop{PlayMode: 42, BeginMarkerID: 3, EndMarkerID: 4}},
			"illegal play mode",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.aiff")
			out, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			e := NewEncoder(out, 44100, 16, 1)
			e.Markers = tc.markers
			e.Instrument = tc.instrument
			err = e.Write(testPCMBuffer(1, 16))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.errStr)
			}
			if !strings.Contains(err.Error(), tc.errStr) {
				t.Fatalf("expected an error containing %q, got %q", tc.errStr, err.Error())
			}
		})
	}
}

// TestInstrumentAbsent makes sure files without MARK/INST chunks behave as
// before and that an encoder without instrument doesn't emit those chunks.
func TestInstrumentAbsent(t *testing.T) {
	f, err := os.Open("fixtures/kick8b.aiff")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	if err := d.Drain(); err != nil {
		t.Fatalf("draining a file without INST chunk failed - %v", err)
	}
	if d.Instrument != nil {
		t.Fatalf("expected no instrument, got %+v", d.Instrument)
	}
	if len(d.Markers) != 0 {
		t.Fatalf("expected no markers, got %+v", d.Markers)
	}

	// an encoder without markers/instrument must not emit MARK/INST chunks
	path := filepath.Join(t.TempDir(), "plain.aiff")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEncoder(out, 22050, 8, 1)
	if err := e.Write(testPCMBuffer(1, 32)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("INST")) || bytes.Contains(raw, []byte("MARK")) {
		t.Fatal("encoder emitted MARK/INST chunks without being configured to")
	}
}
