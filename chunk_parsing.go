package aiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// parseChunk processes a chunk and stores the valuable information
// on the decoder if supported.
// Note that the audio chunk isn't processed using this approach.
func (d *Decoder) parseChunk(chunk *Chunk) error {
	if chunk == nil {
		return nil
	}

	switch chunk.ID {
	// common chunk parsing
	case COMMID:
		if d.commSize > 0 {
			chunk.Done()
		}
		if err := d.parseCommChunk(uint32(chunk.Size)); err != nil {
			return err
		}
		// if we found the sound data before the COMM,
		// we need to rewind the reader so we can properly
		// set the clip reader.
		if d.rewindBytes > 0 {
			d.r.Seek(-d.rewindBytes, 1)
			d.rewindBytes = 0
		}
	// audio content, should be read a different way
	case SSNDID:
		chunk.Done()
	// Comments Chunk
	case COMTID:
		if err := d.parseCommentsChunk(chunk); err != nil {
			return err
		}
	// Markers Chunk
	case MARKID:
		if err := d.parseMarkersChunk(chunk); err != nil {
			return err
		}
	// Apple/Logic specific chunk
	case bascID:
		if err := d.parseBascChunk(chunk); err != nil {
			fmt.Println("failed to read BASC chunk", err)
		}
	// Apple specific: packed struct AudioChannelLayout of CoreAudio
	case chanID:
		// See https://github.com/nu774/qaac/blob/ce73aac9bfba459c525eec5350da6346ebf547cf/chanmap.cpp
		// for format information
		chunk.Done()
	// Apple specific transient data
	case trnsID:
		// TODO extract and store the transients
		/*
			var v1 uint16
			var sensitivity uint16 // 0 to 100 %
			var transientDivisions uint16 // 1 = whole note
			var v3 uint16
			var v5 uint16
			var v4 uint16
			var nbSlice uint16
			int loopSize = v4 * d.AppleInfo.Beats * 2
			for s := 0; s < nbSlice; s++{
				slicepos := 24 * s + 0x4c;
				var sv1 uint16
				var sv2 uint16
				var sampleBegin uint32
			}
		*/
		chunk.Done()
	// Apple specific categorization
	case cateID:
		if err := d.parseCateChunk(chunk); err != nil {
			fmt.Println("failed to read CATE chunk", err)
		}
		chunk.Done()
	default:
		if Debug {
			fmt.Printf("skipping unknown chunk %#v\n", chunk.ID[:])
		}
		// if we read SSN but didn't read the COMM, we need to track location
		if d.SampleRate == 0 {
			d.rewindBytes += int64(chunk.Size)
		}
		chunk.Done()
	}
	return nil
}

// parseCommentsChunk processes the comments chunk, stores the comment texts
// on the decoder as strings as well as the full comment entries (timestamp,
// associated marker ID, text) and drains the chunk.
func (d *Decoder) parseCommentsChunk(chunk *Chunk) error {
	if chunk.ID != COMTID {
		return fmt.Errorf("unexpected comments chunk ID: %q", chunk.ID)
	}

	br := bytes.NewBuffer(make([]byte, 0, chunk.Size))
	if _, err := io.CopyN(br, chunk.R, int64(chunk.Size)); err != nil {
		return fmt.Errorf("truncated COMT chunk - %v", err)
	}

	var nbrComments uint16
	if err := binary.Read(br, binary.BigEndian, &nbrComments); err != nil {
		return fmt.Errorf("truncated COMT chunk (comment count) - %v", err)
	}
	for i := 0; i < int(nbrComments); i++ {
		var (
			timestamp uint32
			markerID  int16
			count     uint16
		)
		if err := binary.Read(br, binary.BigEndian, &timestamp); err != nil {
			return fmt.Errorf("truncated COMT chunk (comment %d timestamp) - %v", i, err)
		}
		if err := binary.Read(br, binary.BigEndian, &markerID); err != nil {
			return fmt.Errorf("truncated COMT chunk (comment %d marker ID) - %v", i, err)
		}
		if err := binary.Read(br, binary.BigEndian, &count); err != nil {
			return fmt.Errorf("truncated COMT chunk (comment %d text length) - %v", i, err)
		}
		if br.Len() < int(count) {
			return fmt.Errorf("truncated COMT chunk (comment %d text: %d bytes announced, %d available)", i, count, br.Len())
		}
		text := string(bytes.TrimRight(br.Next(int(count)), "\x00"))
		if markerID != 0 && !d.hasMarkerID(markerID) {
			return fmt.Errorf("%w: %d", ErrUnknownMarkerID, markerID)
		}
		d.CommentDetails = append(d.CommentDetails, Comment{
			Timestamp: timestamp,
			MarkerID:  markerID,
			Text:      text,
		})
		d.Comments = append(d.Comments, text)
	}

	return nil
}

// parseMarkersChunk processes the markers chunk and stores the markers
// on the decoder and drains the chunk.
func (d *Decoder) parseMarkersChunk(chunk *Chunk) error {
	if chunk.ID != MARKID {
		return fmt.Errorf("unexpected markers chunk ID: %q", chunk.ID)
	}

	br := bytes.NewBuffer(make([]byte, 0, chunk.Size))
	if _, err := io.CopyN(br, chunk.R, int64(chunk.Size)); err != nil {
		return fmt.Errorf("truncated MARK chunk - %v", err)
	}

	var nbrMarkers uint16
	if err := binary.Read(br, binary.BigEndian, &nbrMarkers); err != nil {
		return fmt.Errorf("truncated MARK chunk (marker count) - %v", err)
	}
	seen := make(map[int16]bool, nbrMarkers)
	for i := 0; i < int(nbrMarkers); i++ {
		var (
			id         int16
			position   uint32
			nameLength uint8
		)
		if err := binary.Read(br, binary.BigEndian, &id); err != nil {
			return fmt.Errorf("truncated MARK chunk (marker %d ID) - %v", i, err)
		}
		if err := binary.Read(br, binary.BigEndian, &position); err != nil {
			return fmt.Errorf("truncated MARK chunk (marker %d position) - %v", i, err)
		}
		if err := binary.Read(br, binary.BigEndian, &nameLength); err != nil {
			return fmt.Errorf("truncated MARK chunk (marker %d name length) - %v", i, err)
		}
		if br.Len() < int(nameLength) {
			return fmt.Errorf("truncated MARK chunk (marker %d name: %d bytes announced, %d available)", i, nameLength, br.Len())
		}
		name := string(br.Next(int(nameLength)))
		if (1+int(nameLength))%2 != 0 && br.Len() > 0 {
			// Pascal strings are padded to an even number of bytes
			br.Next(1)
		}
		if seen[id] {
			return fmt.Errorf("%w: %d", ErrDuplicateMarkerID, id)
		}
		seen[id] = true
		// only check the position against the sound data size if the COMM
		// chunk was already parsed.
		if d.commSize > 0 && position > d.NumSampleFrames {
			return fmt.Errorf("%w: marker %d is at frame %d but the file only has %d frames", ErrMarkerPositionOutOfRange, id, position, d.NumSampleFrames)
		}
		d.Markers = append(d.Markers, Marker{ID: id, Position: position, Name: name})
	}

	return nil
}

// parseBascChunk processes the Apple specific BASC chunk
func (d *Decoder) parseBascChunk(chunk *Chunk) error {
	if chunk.ID != bascID {
		return fmt.Errorf("unexpected BASC chunk ID: %q", chunk.ID)
	}
	d.HasAppleInfo = true

	var version uint32
	binary.Read(chunk.R, binary.BigEndian, &version)
	binary.Read(chunk.R, binary.BigEndian, &d.AppleInfo.Beats)
	binary.Read(chunk.R, binary.BigEndian, &d.AppleInfo.Note)
	binary.Read(chunk.R, binary.BigEndian, &d.AppleInfo.Scale)
	binary.Read(chunk.R, binary.BigEndian, &d.AppleInfo.Numerator)
	binary.Read(chunk.R, binary.BigEndian, &d.AppleInfo.Denominator)
	chunk.ReadByte()
	var loopFlag uint16
	binary.Read(chunk.R, binary.BigEndian, &loopFlag)
	// 1  = loop; 2 = one shot
	if loopFlag == 1 {
		d.AppleInfo.IsLooping = true
	}
	chunk.Done()
	return nil
}

func (d *Decoder) parseCateChunk(chunk *Chunk) error {
	if chunk.ID != cateID {
		return fmt.Errorf("unexpected CATE chunk ID: %q", chunk.ID)
	}
	var err error
	d.HasAppleInfo = true

	// skip 4
	tmp := make([]byte, 4)
	if _, err = chunk.R.Read(tmp); err != nil {
		return err
	}

	tmp = make([]byte, 50)
	// 4 main categories: instrument, instrument category, style, substyle
	for i := 0; i < 4; i++ {
		if _, err = chunk.R.Read(tmp); err != nil {
			return err
		}
		if tmp[0] > 0 {
			d.AppleInfo.Tags = append(d.AppleInfo.Tags, nullTermStr(tmp))
		}
	}

	// skip 16
	tmp = make([]byte, 16)
	if _, err = chunk.R.Read(tmp); err != nil {
		return err
	}

	var numDescriptors int16
	binary.Read(chunk.R, binary.BigEndian, &numDescriptors)
	tmp = make([]byte, 50)
	for i := 0; i < int(numDescriptors); i++ {
		if _, err = chunk.R.Read(tmp); err != nil {
			return err
		}
		if tmp[0] > 0 {
			d.AppleInfo.Tags = append(d.AppleInfo.Tags, nullTermStr(tmp))
		}
	}

	chunk.Done()
	return nil
}
