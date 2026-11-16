package aiff

import (
	"errors"
	"fmt"
	"io"
)

// Loop play modes as defined by the AIFF INST chunk specification.
const (
	// LoopModeNone indicates that the loop is not active.
	LoopModeNone int16 = 0
	// LoopModeForward indicates forward looping.
	LoopModeForward int16 = 1
	// LoopModeForwardBackward indicates forward/backward (ping-pong) looping.
	LoopModeForwardBackward int16 = 2
)

// instChunkDataSize is the fixed size in bytes of the INST chunk data.
const instChunkDataSize = 20

// ErrDuplicateInstrument is returned when a file contains more than one INST chunk.
var ErrDuplicateInstrument = errors.New("aiff: duplicate INST chunk")

// Marker is a cue point defined in the MARK chunk. The INST chunk loops
// reference markers by their ID.
type Marker struct {
	// ID is the unique identifier of the marker, referenced by instrument loops.
	ID int16
	// Position is the marker position in sample frames.
	Position uint32
	// Name is the optional marker label.
	Name string
}

// InstrumentLoop is one of the two loops (sustain or release) of an
// instrument. The loop boundaries point to markers of the MARK chunk.
type InstrumentLoop struct {
	// PlayMode is one of LoopModeNone, LoopModeForward, LoopModeForwardBackward.
	PlayMode int16
	// BeginMarkerID is the ID of the marker where the loop starts.
	BeginMarkerID int16
	// EndMarkerID is the ID of the marker where the loop ends.
	EndMarkerID int16
}

// Instrument represents the data of the AIFF INST (instrument) chunk,
// used when the audio is meant to be played back as a sampled instrument.
type Instrument struct {
	// BaseNote is the MIDI note number at which the sample plays back at its
	// original (recorded) sample rate.
	BaseNote uint8
	// Detune adjusts the pitch of the sample in cents (-50 to +50).
	Detune int8
	// LowNote is the lowest MIDI note of the key range.
	LowNote uint8
	// HighNote is the highest MIDI note of the key range.
	HighNote uint8
	// LowVelocity is the lowest MIDI velocity of the velocity range.
	LowVelocity uint8
	// HighVelocity is the highest MIDI velocity of the velocity range.
	HighVelocity uint8
	// Gain is the playback gain in dB.
	Gain int16
	// SustainLoop is the loop played while the note is held.
	SustainLoop InstrumentLoop
	// ReleaseLoop is the loop played after the note is released.
	ReleaseLoop InstrumentLoop
}

// Validate checks the instrument ranges, loop play modes and verifies that
// the loops only reference markers present in the passed marker list.
func (i *Instrument) Validate(markers []Marker) error {
	if err := i.validateStructure(); err != nil {
		return err
	}
	if err := i.SustainLoop.validateRefs("sustain", markers); err != nil {
		return err
	}
	if err := i.ReleaseLoop.validateRefs("release", markers); err != nil {
		return err
	}
	return nil
}

// validateStructure checks the note/velocity ranges and the loop play modes.
func (i *Instrument) validateStructure() error {
	if i == nil {
		return fmt.Errorf("aiff: can't validate a nil instrument")
	}
	if i.LowNote > i.HighNote {
		return fmt.Errorf("aiff: instrument note range is inverted: low %d > high %d", i.LowNote, i.HighNote)
	}
	if i.LowVelocity > i.HighVelocity {
		return fmt.Errorf("aiff: instrument velocity range is inverted: low %d > high %d", i.LowVelocity, i.HighVelocity)
	}
	if err := i.SustainLoop.validateMode("sustain"); err != nil {
		return err
	}
	if err := i.ReleaseLoop.validateMode("release"); err != nil {
		return err
	}
	return nil
}

func (l InstrumentLoop) validateMode(name string) error {
	switch l.PlayMode {
	case LoopModeNone, LoopModeForward, LoopModeForwardBackward:
		return nil
	default:
		return fmt.Errorf("aiff: %s loop has an illegal play mode: %d", name, l.PlayMode)
	}
}

func (l InstrumentLoop) validateRefs(name string, markers []Marker) error {
	if l.PlayMode == LoopModeNone {
		return nil
	}
	for _, id := range []int16{l.BeginMarkerID, l.EndMarkerID} {
		found := false
		for _, m := range markers {
			if m.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("aiff: %s loop references missing marker ID %d", name, id)
		}
	}
	return nil
}

// parseMarkersChunk processes the MARK chunk and stores the markers on the decoder.
func (d *Decoder) parseMarkersChunk(chunk *Chunk) error {
	if chunk.ID != MARKID {
		return fmt.Errorf("unexpected MARK chunk ID: %q", chunk.ID)
	}
	var numMarkers uint16
	if err := chunk.ReadBE(&numMarkers); err != nil {
		return fmt.Errorf("aiff: truncated MARK chunk, failed to read the marker count - %v", err)
	}
	for i := 0; i < int(numMarkers); i++ {
		var m Marker
		if err := chunk.ReadBE(&m.ID); err != nil {
			return fmt.Errorf("aiff: truncated MARK chunk, failed to read marker %d ID - %v", i, err)
		}
		if err := chunk.ReadBE(&m.Position); err != nil {
			return fmt.Errorf("aiff: truncated MARK chunk, failed to read marker %d position - %v", i, err)
		}
		var nameLen uint8
		if err := chunk.ReadBE(&nameLen); err != nil {
			return fmt.Errorf("aiff: truncated MARK chunk, failed to read marker %d name length - %v", i, err)
		}
		if nameLen > 0 {
			name := make([]byte, nameLen)
			if _, err := io.ReadFull(chunk, name); err != nil {
				return fmt.Errorf("aiff: truncated MARK chunk, failed to read marker %d name - %v", i, err)
			}
			m.Name = string(name)
		}
		// pascal strings are padded so the total (length byte + content) is even
		if (int(nameLen)+1)%2 != 0 {
			if _, err := chunk.ReadByte(); err != nil {
				return fmt.Errorf("aiff: truncated MARK chunk, failed to read marker %d padding - %v", i, err)
			}
		}
		d.Markers = append(d.Markers, m)
	}
	chunk.Done()
	return nil
}

// parseInstrumentChunk processes the INST chunk and stores the instrument on
// the decoder. Ranges, play modes and the chunk structure are validated right
// away; the loop marker references are validated once the entire file has
// been drained (see validateInstrumentRefs) since the MARK chunk is allowed
// to appear after the INST chunk.
func (d *Decoder) parseInstrumentChunk(chunk *Chunk) error {
	if chunk.ID != INSTID {
		return fmt.Errorf("unexpected INST chunk ID: %q", chunk.ID)
	}
	if d.Instrument != nil {
		chunk.Done()
		return ErrDuplicateInstrument
	}
	if chunk.Size < instChunkDataSize {
		chunk.Done()
		return fmt.Errorf("aiff: truncated INST chunk, expected at least %d bytes but the chunk is %d bytes", instChunkDataSize, chunk.Size)
	}
	inst := &Instrument{}
	if err := chunk.ReadBE(&inst.BaseNote); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the base note - %v", err)
	}
	if err := chunk.ReadBE(&inst.Detune); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the detune - %v", err)
	}
	if err := chunk.ReadBE(&inst.LowNote); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the low note - %v", err)
	}
	if err := chunk.ReadBE(&inst.HighNote); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the high note - %v", err)
	}
	if err := chunk.ReadBE(&inst.LowVelocity); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the low velocity - %v", err)
	}
	if err := chunk.ReadBE(&inst.HighVelocity); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the high velocity - %v", err)
	}
	if err := chunk.ReadBE(&inst.Gain); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the gain - %v", err)
	}
	if err := readInstrumentLoop(chunk, &inst.SustainLoop); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the sustain loop - %v", err)
	}
	if err := readInstrumentLoop(chunk, &inst.ReleaseLoop); err != nil {
		return fmt.Errorf("aiff: truncated INST chunk, failed to read the release loop - %v", err)
	}
	chunk.Done()
	if err := inst.validateStructure(); err != nil {
		return err
	}
	d.Instrument = inst
	return nil
}

// validateInstrumentRefs checks that the instrument loops (if any) only
// reference known markers. It is meant to be called once all the chunks have
// been parsed and returns nil when the file has no instrument.
func (d *Decoder) validateInstrumentRefs() error {
	if d.Instrument == nil {
		return nil
	}
	return d.Instrument.Validate(d.Markers)
}

func readInstrumentLoop(chunk *Chunk, l *InstrumentLoop) error {
	if err := chunk.ReadBE(&l.PlayMode); err != nil {
		return err
	}
	if err := chunk.ReadBE(&l.BeginMarkerID); err != nil {
		return err
	}
	if err := chunk.ReadBE(&l.EndMarkerID); err != nil {
		return err
	}
	return nil
}

// writeMarkersChunk serializes the encoder markers as a MARK chunk.
func (e *Encoder) writeMarkersChunk() error {
	dataSize := 2
	for _, m := range e.Markers {
		if len(m.Name) > 255 {
			return fmt.Errorf("aiff: marker name %q is too long (%d bytes, max 255)", m.Name, len(m.Name))
		}
		// id + position + pascal string (length byte + content + padding)
		dataSize += 6 + 1 + len(m.Name)
		if (len(m.Name)+1)%2 != 0 {
			dataSize++
		}
	}
	if err := e.AddBE(MARKID); err != nil {
		return fmt.Errorf("%v when writing MARK chunk ID header", err)
	}
	if err := e.AddBE(uint32(dataSize)); err != nil {
		return fmt.Errorf("%v when writing MARK chunk size header", err)
	}
	if err := e.AddBE(uint16(len(e.Markers))); err != nil {
		return fmt.Errorf("%v when writing the number of markers", err)
	}
	for _, m := range e.Markers {
		if err := e.AddBE(m.ID); err != nil {
			return fmt.Errorf("%v when writing marker ID", err)
		}
		if err := e.AddBE(m.Position); err != nil {
			return fmt.Errorf("%v when writing marker position", err)
		}
		if err := e.AddBE(uint8(len(m.Name))); err != nil {
			return fmt.Errorf("%v when writing marker name length", err)
		}
		if len(m.Name) > 0 {
			if err := e.AddBE([]byte(m.Name)); err != nil {
				return fmt.Errorf("%v when writing marker name", err)
			}
		}
		// pad the pascal string so its total size is even
		if (len(m.Name)+1)%2 != 0 {
			if err := e.AddBE(uint8(0)); err != nil {
				return fmt.Errorf("%v when writing marker name padding", err)
			}
		}
	}
	return nil
}

// writeInstrumentChunk serializes the encoder instrument as an INST chunk.
// The instrument is validated against the encoder markers so no dangling
// loop reference is ever written out.
func (e *Encoder) writeInstrumentChunk() error {
	if err := e.Instrument.Validate(e.Markers); err != nil {
		return err
	}
	if err := e.AddBE(INSTID); err != nil {
		return fmt.Errorf("%v when writing INST chunk ID header", err)
	}
	if err := e.AddBE(uint32(instChunkDataSize)); err != nil {
		return fmt.Errorf("%v when writing INST chunk size header", err)
	}
	values := []interface{}{
		e.Instrument.BaseNote,
		e.Instrument.Detune,
		e.Instrument.LowNote,
		e.Instrument.HighNote,
		e.Instrument.LowVelocity,
		e.Instrument.HighVelocity,
		e.Instrument.Gain,
		e.Instrument.SustainLoop.PlayMode,
		e.Instrument.SustainLoop.BeginMarkerID,
		e.Instrument.SustainLoop.EndMarkerID,
		e.Instrument.ReleaseLoop.PlayMode,
		e.Instrument.ReleaseLoop.BeginMarkerID,
		e.Instrument.ReleaseLoop.EndMarkerID,
	}
	for _, v := range values {
		if err := e.AddBE(v); err != nil {
			return fmt.Errorf("%v when writing INST chunk data", err)
		}
	}
	return nil
}
