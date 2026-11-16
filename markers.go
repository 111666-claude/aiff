package aiff

import (
	"errors"
	"fmt"
)

// Marker represents an entry of the AIFF MARK chunk: a named position
// in the sound data (e.g. "footstep start", "door slam end").
type Marker struct {
	// ID uniquely identifies the marker inside the file and is used
	// to associate comments (COMT chunk) with this marker.
	ID int16
	// Position is the location of the marker in sample frames.
	Position uint32
	// Name is the marker label. It is stored as a Pascal string and
	// therefore can't exceed 255 bytes.
	Name string
}

// Comment represents an entry of the AIFF COMT chunk.
type Comment struct {
	// Timestamp is expressed in seconds since January 1, 1904 (AIFF epoch).
	Timestamp uint32
	// MarkerID associates the comment with a marker of the MARK chunk.
	// A value of 0 means the comment isn't associated with any marker.
	MarkerID int16
	// Text is the content of the comment (up to 65535 bytes).
	Text string
}

var (
	// ErrDuplicateMarkerID is returned when two markers share the same ID.
	ErrDuplicateMarkerID = errors.New("duplicate marker ID")
	// ErrUnknownMarkerID is returned when a comment references a marker ID
	// that isn't defined in the MARK chunk.
	ErrUnknownMarkerID = errors.New("comment references an unknown marker ID")
	// ErrMarkerPositionOutOfRange is returned when a marker position lies
	// beyond the number of sample frames of the file.
	ErrMarkerPositionOutOfRange = errors.New("marker position out of range")
	// ErrMarkerNameTooLong is returned when a marker name doesn't fit in
	// a Pascal string (255 bytes max).
	ErrMarkerNameTooLong = errors.New("marker name too long")
	// ErrCommentTextTooLong is returned when a comment text doesn't fit in
	// an uint16 length prefixed field (65535 bytes max).
	ErrCommentTextTooLong = errors.New("comment text too long")
)

// maxMarkerNameLen is the size limit of a Pascal string.
const maxMarkerNameLen = 255

// maxCommentTextLen is the size limit of the uint16 length prefixed
// comment text.
const maxCommentTextLen = 0xffff

// AddMarker registers a marker to be serialized in the MARK chunk.
// Markers must be added before the first call to Write.
func (e *Encoder) AddMarker(m Marker) error {
	if len(m.Name) > maxMarkerNameLen {
		return fmt.Errorf("%w: %q is %d bytes, max %d", ErrMarkerNameTooLong, m.Name, len(m.Name), maxMarkerNameLen)
	}
	for _, existing := range e.markers {
		if existing.ID == m.ID {
			return fmt.Errorf("%w: %d", ErrDuplicateMarkerID, m.ID)
		}
	}
	e.markers = append(e.markers, m)
	return nil
}

// AddComment registers a comment to be serialized in the COMT chunk.
// Set MarkerID to 0 for a comment not associated with any marker.
// Comments must be added before the first call to Write.
func (e *Encoder) AddComment(c Comment) error {
	if len(c.Text) > maxCommentTextLen {
		return fmt.Errorf("%w: text is %d bytes, max %d", ErrCommentTextTooLong, len(c.Text), maxCommentTextLen)
	}
	e.comments = append(e.comments, c)
	return nil
}

// validateMetadata cross-checks the registered markers and comments.
// Marker positions are validated separately in Close once the total
// number of frames is known.
func (e *Encoder) validateMetadata() error {
	ids := make(map[int16]bool, len(e.markers))
	for _, m := range e.markers {
		if len(m.Name) > maxMarkerNameLen {
			return fmt.Errorf("%w: %q is %d bytes, max %d", ErrMarkerNameTooLong, m.Name, len(m.Name), maxMarkerNameLen)
		}
		if ids[m.ID] {
			return fmt.Errorf("%w: %d", ErrDuplicateMarkerID, m.ID)
		}
		ids[m.ID] = true
	}
	for _, c := range e.comments {
		if len(c.Text) > maxCommentTextLen {
			return fmt.Errorf("%w: text is %d bytes, max %d", ErrCommentTextTooLong, len(c.Text), maxCommentTextLen)
		}
		if c.MarkerID != 0 && !ids[c.MarkerID] {
			return fmt.Errorf("%w: %d", ErrUnknownMarkerID, c.MarkerID)
		}
	}
	return nil
}

// writeMetaChunks serializes the MARK and COMT chunks if any marker or
// comment was registered. It is a no-op when no metadata was added, keeping
// the output byte-identical to a plain AIFF file.
func (e *Encoder) writeMetaChunks() error {
	if e.metaChunksWritten {
		return nil
	}
	e.metaChunksWritten = true
	if len(e.markers) == 0 && len(e.comments) == 0 {
		return nil
	}
	if err := e.validateMetadata(); err != nil {
		return err
	}
	if len(e.markers) > 0 {
		if err := e.writeMarkersChunk(); err != nil {
			return err
		}
	}
	if len(e.comments) > 0 {
		if err := e.writeCommentsChunk(); err != nil {
			return err
		}
	}
	return nil
}

// writeMarkersChunk serializes the MARK chunk:
//
//	0      4 bytes  "MARK"
//	4      4 bytes  <chunk size>
//	8      2 bytes  <number of markers>
//	per marker:
//	       2 bytes  <marker ID>
//	       4 bytes  <position in sample frames>
//	       1 byte   <name length> + name bytes (Pascal string, padded to even)
//
// The Pascal string pad byte is part of the chunk data and is counted
// in the chunk size.
func (e *Encoder) writeMarkersChunk() error {
	size := 2
	for _, m := range e.markers {
		size += 2 + 4 + 1 + len(m.Name)
		if (1+len(m.Name))%2 != 0 {
			size++ // Pascal string pad byte
		}
	}
	if err := e.AddBE(MARKID); err != nil {
		return fmt.Errorf("%v when writing MARK chunk ID header", err)
	}
	if err := e.AddBE(uint32(size)); err != nil {
		return fmt.Errorf("%v when writing MARK chunk size header", err)
	}
	if err := e.AddBE(uint16(len(e.markers))); err != nil {
		return fmt.Errorf("%v when writing the number of markers", err)
	}
	for _, m := range e.markers {
		if err := e.AddBE(m.ID); err != nil {
			return fmt.Errorf("%v when writing marker ID", err)
		}
		if err := e.AddBE(m.Position); err != nil {
			return fmt.Errorf("%v when writing marker position", err)
		}
		if err := e.AddBE(uint8(len(m.Name))); err != nil {
			return fmt.Errorf("%v when writing marker name length", err)
		}
		if err := e.AddBE([]byte(m.Name)); err != nil {
			return fmt.Errorf("%v when writing marker name", err)
		}
		if (1+len(m.Name))%2 != 0 {
			if err := e.AddBE(uint8(0)); err != nil {
				return fmt.Errorf("%v when writing marker name pad byte", err)
			}
		}
	}
	return nil
}

// writeCommentsChunk serializes the COMT chunk:
//
//	0      4 bytes  "COMT"
//	4      4 bytes  <chunk size>
//	8      2 bytes  <number of comments>
//	per comment:
//	       4 bytes  <timestamp>
//	       2 bytes  <associated marker ID, 0 if none>
//	       2 bytes  <text length> + text bytes
//
// If the chunk size ends up odd, a pad byte follows the chunk data
// (not counted in the chunk size, as per EA IFF 85).
func (e *Encoder) writeCommentsChunk() error {
	size := 2
	for _, c := range e.comments {
		size += 4 + 2 + 2 + len(c.Text)
	}
	if err := e.AddBE(COMTID); err != nil {
		return fmt.Errorf("%v when writing COMT chunk ID header", err)
	}
	if err := e.AddBE(uint32(size)); err != nil {
		return fmt.Errorf("%v when writing COMT chunk size header", err)
	}
	if err := e.AddBE(uint16(len(e.comments))); err != nil {
		return fmt.Errorf("%v when writing the number of comments", err)
	}
	for _, c := range e.comments {
		if err := e.AddBE(c.Timestamp); err != nil {
			return fmt.Errorf("%v when writing comment timestamp", err)
		}
		if err := e.AddBE(c.MarkerID); err != nil {
			return fmt.Errorf("%v when writing comment marker ID", err)
		}
		if err := e.AddBE(uint16(len(c.Text))); err != nil {
			return fmt.Errorf("%v when writing comment text length", err)
		}
		if err := e.AddBE([]byte(c.Text)); err != nil {
			return fmt.Errorf("%v when writing comment text", err)
		}
	}
	if size%2 != 0 {
		// chunk level pad byte, not counted in the chunk size
		if err := e.AddBE(uint8(0)); err != nil {
			return fmt.Errorf("%v when writing COMT pad byte", err)
		}
	}
	return nil
}

// hasMarkerID returns true if a marker with the given ID was parsed.
func (d *Decoder) hasMarkerID(id int16) bool {
	for _, m := range d.Markers {
		if m.ID == id {
			return true
		}
	}
	return false
}
