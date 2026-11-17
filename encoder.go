package aiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/go-audio/audio"
)

// Encoder encodes PCM data into an aiff content.
//
// By default (Encoding left at its zero value) the encoder writes a plain
// AIFF container with big endian integer samples. When Encoding is set to an
// AIFC compression type an AIFC container is written instead. The G.711 μ-law
// compression types ("ulaw" and "ULAW") are supported: the caller provides
// 16 bit signed linear PCM samples which are compressed to 8 bit code words.
type Encoder struct {
	w          io.WriteSeeker
	SampleRate int
	BitDepth   int
	NumChans   int
	// Encoding is the AIFC compression type. Left at [4]byte{} the encoder
	// produces a plain AIFF file. EncodingULaw and EncodingULAW enable the
	// G.711 μ-law output, in which case BitDepth must be 16 (the bit depth
	// of the input samples).
	Encoding [4]byte

	WrittenBytes int
	frames       int
	// dataBytes is the number of bytes written to the SSND chunk, excluding
	// its 8 header bytes and any padding byte.
	dataBytes int

	pcmChunkStarted bool
	pcmChunkSizePos int
	formSizePos     int
	commFramesPos   int
	headerWritten   bool
	closed          bool
	writeErr        error
}

// NewEncoder creates a new encoder to create a new aiff file.
// Don't forget to add Frames to the encoder before writing.
func NewEncoder(w io.WriteSeeker, sampleRate, bitDepth, numChans int) *Encoder {
	return &Encoder{
		w:          w,
		SampleRate: sampleRate,
		BitDepth:   bitDepth,
		NumChans:   numChans,
	}
}

// NewULawEncoder creates an AIFC encoder writing G.711 μ-law compressed
// samples. Samples passed to Write must be 16 bit signed linear PCM values;
// they are compressed to 8 bit code words and the COMM chunk declares 8 bit
// samples. Set Encoding to EncodingULAW before writing to use the upper case
// compression type instead of the default lower case "ulaw".
func NewULawEncoder(w io.WriteSeeker, sampleRate, numChans int) *Encoder {
	return &Encoder{
		w:          w,
		SampleRate: sampleRate,
		BitDepth:   16,
		NumChans:   numChans,
		Encoding:   envUlaw,
	}
}

// NewUlawEncoder is an alias of NewULawEncoder.
func NewUlawEncoder(w io.WriteSeeker, sampleRate, numChans int) *Encoder {
	return NewULawEncoder(w, sampleRate, numChans)
}

// NewMuLawEncoder is an alias of NewULawEncoder.
func NewMuLawEncoder(w io.WriteSeeker, sampleRate, numChans int) *Encoder {
	return NewULawEncoder(w, sampleRate, numChans)
}

// NewAIFCEncoder creates an AIFC encoder using G.711 μ-law compression. When
// no encoding is passed the lower case "ulaw" compression type is used, the
// upper case "ULAW" variant can be selected by passing EncodingULAW. bitDepth
// must be 16, the bit depth of the linear PCM samples passed to Write.
//
// Invalid configurations are reported by Write and Close instead of writing a
// damaged file.
func NewAIFCEncoder(w io.WriteSeeker, sampleRate, bitDepth, numChans int, encoding ...[4]byte) *Encoder {
	enc := envUlaw
	if len(encoding) > 0 {
		enc = encoding[0]
	}
	return &Encoder{
		w:          w,
		SampleRate: sampleRate,
		BitDepth:   bitDepth,
		NumChans:   numChans,
		Encoding:   enc,
	}
}

// NewEncoderWithEncoding creates an encoder for the given AIFC compression
// type. A zero encoding is equivalent to NewEncoder and produces a plain
// AIFF file. The supported AIFC compression types are EncodingULaw and
// EncodingULAW, both requiring 16 bit input samples. Unsupported combinations
// are reported before anything is written.
func NewEncoderWithEncoding(w io.WriteSeeker, sampleRate, bitDepth, numChans int, encoding [4]byte) (*Encoder, error) {
	e := &Encoder{
		w:          w,
		SampleRate: sampleRate,
		BitDepth:   bitDepth,
		NumChans:   numChans,
		Encoding:   encoding,
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return e, nil
}

// isAIFC reports whether the encoder writes an AIFC container.
func (e *Encoder) isAIFC() bool {
	return e.Encoding != encNotSet
}

// isULaw reports whether the encoder writes G.711 μ-law samples.
func (e *Encoder) isULaw() bool {
	return isULawEncoding(e.Encoding)
}

// commSampleSize returns the number of bits of a stored sample, as declared
// by the COMM chunk. μ-law stores 8 bit code words even though the encoder
// reads 16 bit input samples.
func (e *Encoder) commSampleSize() int {
	if e.isULaw() {
		return 8
	}
	return e.BitDepth
}

// validate checks that the encoder configuration can be represented without
// writing a damaged container.
func (e *Encoder) validate() error {
	if e == nil {
		return fmt.Errorf("%w: nil encoder", ErrInvalidParams)
	}
	if e.w == nil {
		return fmt.Errorf("%w: nil writer", ErrInvalidParams)
	}
	if e.SampleRate <= 0 {
		return fmt.Errorf("%w: invalid sample rate %d", ErrInvalidParams, e.SampleRate)
	}
	if e.NumChans < 1 {
		return fmt.Errorf("%w: invalid number of channels %d", ErrInvalidParams, e.NumChans)
	}
	if !e.isAIFC() {
		switch e.BitDepth {
		case 8, 16, 24, 32:
			return nil
		default:
			return fmt.Errorf("%w: unsupported bit depth %d", ErrInvalidParams, e.BitDepth)
		}
	}
	if !e.isULaw() {
		return fmt.Errorf("%w: AIFC compression type %q", ErrFmtNotSupported, e.Encoding)
	}
	// G.711 input is 16 bit signed linear PCM, stored as 8 bit code words.
	if e.BitDepth != 16 {
		return fmt.Errorf("%w: %q requires 16 bit input samples, got %d", ErrInvalidParams, e.Encoding, e.BitDepth)
	}
	return nil
}

// validateBuffer checks that the buffer can be serialized by the encoder.
func (e *Encoder) validateBuffer(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}
	if !e.isULaw() {
		return nil
	}
	if buf.Format == nil {
		// a format is optional for mono content, the samples are then
		// considered to be a single interleaved stream.
		if e.NumChans > 1 {
			return fmt.Errorf("%w: can't add a buffer without a format to a %d channel encoder", ErrBufferMismatch, e.NumChans)
		}
	} else {
		if buf.Format.NumChannels != e.NumChans {
			return fmt.Errorf("%w: buffer has %d channels, encoder has %d", ErrBufferMismatch, buf.Format.NumChannels, e.NumChans)
		}
		if buf.Format.SampleRate != 0 && buf.Format.SampleRate != e.SampleRate {
			return fmt.Errorf("%w: buffer sample rate %d, encoder sample rate %d", ErrBufferMismatch, buf.Format.SampleRate, e.SampleRate)
		}
	}
	if buf.SourceBitDepth != 0 && buf.SourceBitDepth != 16 {
		return fmt.Errorf("%w: μ-law requires 16 bit samples, buffer holds %d bit samples", ErrBufferMismatch, buf.SourceBitDepth)
	}
	if len(buf.Data)%e.NumChans != 0 {
		return fmt.Errorf("%w: %d samples isn't a whole number of frames for %d channels", ErrBufferMismatch, len(buf.Data), e.NumChans)
	}
	for _, v := range buf.Data {
		if v < -32768 || v > 32767 {
			return fmt.Errorf("%w: %d can't be encoded as G.711, samples must be 16 bit signed PCM", ErrInvalidSample, v)
		}
	}
	return nil
}

// AddBE serializes and adds the passed value using big endian
func (e *Encoder) AddBE(src interface{}) error {
	e.WrittenBytes += binary.Size(src)
	return binary.Write(e.w, binary.BigEndian, src)
}

// AddLE serializes and adds the passed value using little endian
func (e *Encoder) AddLE(src interface{}) error {
	e.WrittenBytes += binary.Size(src)
	return binary.Write(e.w, binary.LittleEndian, src)
}

// addULawSamples compresses 16 bit interleaved samples into μ-law code words.
func addULawSamples(bb *bytes.Buffer, data []int) {
	for _, v := range data {
		bb.WriteByte(ulawEncodeSample(v))
	}
}

func (e *Encoder) addBuffer(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}

	frameCount := buf.NumFrames()
	// setup a buffer so we don't do many writes
	bb := bytes.NewBuffer(nil)
	var err error
	for i := 0; i < frameCount; i++ {
		for j := 0; j < buf.Format.NumChannels; j++ {
			v := buf.Data[i*buf.Format.NumChannels+j]
			switch e.BitDepth {
			case 8:
				if err = binary.Write(bb, binary.BigEndian, uint8(v)); err != nil {
					return err
				}
			case 16:
				if err = binary.Write(bb, binary.BigEndian, int16(v)); err != nil {
					return err
				}
			case 24:
				if err = binary.Write(bb, binary.BigEndian, audio.Int32toInt24BEBytes(int32(v))); err != nil {
					return err
				}
			case 32:
				if err = binary.Write(bb, binary.BigEndian, int32(v)); err != nil {
					return err
				}
			default:
				return fmt.Errorf("can't add frames of bit size %d", e.BitDepth)
			}
		}
		e.frames++
	}
	n, err := e.w.Write(bb.Bytes())
	e.WrittenBytes += n
	e.dataBytes += n
	return err
}

// writeHeader writes the FORM, optional FVER and COMM chunks.
func (e *Encoder) writeHeader() error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.w == nil {
		return fmt.Errorf("can't write to a nil writer")
	}
	if e.headerWritten {
		return nil
	}
	if err := e.validate(); err != nil {
		return err
	}

	form := aiffID
	if e.isAIFC() {
		form = aifcID
	}

	// ID
	if err := e.AddBE(formID); err != nil {
		return fmt.Errorf("%v when writing FORM header", err)
	}
	// size, will need to be updated later on (total size - 8)
	e.formSizePos = e.WrittenBytes
	if err := e.AddBE(uint32(0)); err != nil {
		return fmt.Errorf("%v when writing size header", err)
	}
	// Format
	if err := e.AddBE(form); err != nil {
		return fmt.Errorf("%v when writing format header", err)
	}
	if e.isAIFC() {
		// The FVER chunk is mandatory in AIFC files.
		if err := e.AddBE([]byte("FVER")); err != nil {
			return fmt.Errorf("%v when writing FVER chunk ID header", err)
		}
		if err := e.AddBE(uint32(4)); err != nil {
			return fmt.Errorf("%v when writing FVER chunk size header", err)
		}
		if err := e.AddBE(aifcVersion1); err != nil {
			return fmt.Errorf("%v when writing FVER version", err)
		}
	}
	// comm chunk
	if err := e.AddBE(COMMID); err != nil {
		return fmt.Errorf("%v when writing comm chunk ID header", err)
	}
	// 18 bytes of common data. AIFC adds a 4 byte compression type and a
	// pascal style compression name (length byte + name). The chunk data is
	// padded to an even byte count, the pad byte is part of the declared
	// size. The name is left empty, as done by ffmpeg and libsndfile.
	commSize := uint32(18)
	commPadding := 0
	if e.isAIFC() {
		commSize = 18 + 4 + 1
		if commSize%2 != 0 {
			commSize++
			commPadding = 1
		}
	}
	if err := e.AddBE(commSize); err != nil {
		return fmt.Errorf("%v when writing comm chunk size header", err)
	}
	if err := e.AddBE(uint16(e.NumChans)); err != nil {
		return fmt.Errorf("%v when writing comm chan numbers", err)
	}
	// number of sample frames (unknown at this point)
	// will have to come back and edit
	e.commFramesPos = e.WrittenBytes
	if err := e.AddBE(uint32(0)); err != nil {
		return fmt.Errorf("%v when writing comm num sample frames", err)
	}
	if err := e.AddBE(uint16(e.commSampleSize())); err != nil {
		return fmt.Errorf("%v when writing comm sample size", err)
	}
	// sample rate in IeeeFloat (10 bytes)
	if err := e.AddBE(audio.IntToIEEEFloat(int(e.SampleRate))); err != nil {
		return fmt.Errorf("%v when writing comm sample rate", err)
	}
	if e.isAIFC() {
		if err := e.AddBE(e.Encoding); err != nil {
			return fmt.Errorf("%v when writing comm compression type", err)
		}
		// empty pascal style compression name
		if err := e.AddBE(uint8(0)); err != nil {
			return fmt.Errorf("%v when writing comm compression name length", err)
		}
		for i := 0; i < commPadding; i++ {
			if err := e.AddBE(uint8(0)); err != nil {
				return fmt.Errorf("%v when writing comm chunk padding", err)
			}
		}
	}
	e.headerWritten = true
	return nil
}

// writePCMChunkHeader writes the SSND chunk ID, a placeholder chunk size and
// the offset/block size fields. The real size is patched by Close.
func (e *Encoder) writePCMChunkHeader() error {
	if e.pcmChunkStarted {
		return nil
	}
	if err := e.AddBE([]byte("SSND")); err != nil {
		return fmt.Errorf("%v when writing SSND chunk ID header", err)
	}
	e.pcmChunkSizePos = e.WrittenBytes
	e.pcmChunkStarted = true

	// temporary chunk size, patched by Close.
	if err := e.AddBE(uint32(8)); err != nil {
		return fmt.Errorf("%v when writing SSND chunk size header", err)
	}
	// offset and block size.
	if err := e.AddBE(uint32(0)); err != nil {
		return fmt.Errorf("%v when writing SSND offset", err)
	}
	if err := e.AddBE(uint32(0)); err != nil {
		return fmt.Errorf("%v when writing SSND block size", err)
	}
	return nil
}

// Write adds PCM data to the container. For μ-law encoders the buffer holds
// 16 bit signed linear PCM samples that are compressed to 8 bit code words;
// samples are interleaved per frame so the channel order is preserved.
func (e *Encoder) Write(buf *audio.IntBuffer) error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.closed {
		return fmt.Errorf("%w: can't write to a closed encoder", ErrInvalidParams)
	}
	if e.writeErr != nil {
		return e.writeErr
	}
	if err := e.validate(); err != nil {
		return err
	}
	if err := e.validateBuffer(buf); err != nil {
		return err
	}
	if err := e.writeHeader(); err != nil {
		return e.latch(err)
	}
	if err := e.writePCMChunkHeader(); err != nil {
		return e.latch(err)
	}
	if e.isULaw() {
		// Serialize the whole buffer before writing so that an invalid
		// sample never leaves a partial frame behind.
		bb := bytes.NewBuffer(make([]byte, 0, len(buf.Data)))
		addULawSamples(bb, buf.Data)
		n, err := e.w.Write(bb.Bytes())
		e.WrittenBytes += n
		// Only whole frames are accounted for.
		frameSize := e.NumChans
		e.dataBytes += n / frameSize * frameSize
		e.frames += n / frameSize
		if err != nil {
			return e.latch(fmt.Errorf("failed to write samples: %w", err))
		}
		if n != bb.Len() {
			return e.latch(fmt.Errorf("failed to write samples: %w", io.ErrShortWrite))
		}
		return nil
	}
	if err := e.addBuffer(buf); err != nil {
		return e.latch(err)
	}
	return nil
}

// latch records the first write error so a caller can't keep writing to a
// partially written file.
func (e *Encoder) latch(err error) error {
	if err == nil {
		return nil
	}
	if e.writeErr == nil {
		e.writeErr = err
	}
	return e.writeErr
}

// Close flushes the content to disk, make sure the headers are up to date
// Note that the underlying writter is NOT being closed.
func (e *Encoder) Close() error {
	if e == nil {
		return fmt.Errorf("can't close a nil encoder")
	}
	if e.closed {
		return e.writeErr
	}
	if err := e.validate(); err != nil {
		return err
	}
	if !e.headerWritten {
		if err := e.writeHeader(); err != nil {
			return e.latch(err)
		}
	}
	// Make sure the file holds an SSND chunk, even when no sample was added.
	if err := e.writePCMChunkHeader(); err != nil {
		return e.latch(err)
	}
	e.closed = true

	// AIFF chunks hold an even number of bytes: pad odd sized sample data.
	// The pad byte isn't counted in the SSND chunk size but is part of the
	// FORM size.
	if e.isAIFC() && e.dataBytes%2 != 0 {
		n, err := e.w.Write([]byte{0})
		e.WrittenBytes += n
		if err != nil {
			return e.latch(fmt.Errorf("%v when writing the SSND chunk padding", err))
		}
	}

	// go back and write total size
	if _, err := e.w.Seek(int64(e.formSizePos), 0); err != nil {
		return err
	}
	if err := e.AddBE(uint32(e.WrittenBytes) - 8); err != nil {
		return fmt.Errorf("%v when writing the total written bytes", err)
	}
	if _, err := e.w.Seek(int64(e.commFramesPos), 0); err != nil {
		return err
	}
	if err := e.AddBE(uint32(e.frames)); err != nil {
		return fmt.Errorf("%v when writing the total of frames", err)
	}
	// rewrite the audio chunk length header
	if e.pcmChunkSizePos > 0 {
		if _, err := e.w.Seek(int64(e.pcmChunkSizePos), 0); err != nil {
			return err
		}
		// The chunk size covers the offset and block size fields plus the
		// sample bytes; the pad byte of an odd sized chunk isn't included.
		chunksize := uint32(e.dataBytes + 8)
		if err := e.AddBE(chunksize); err != nil {
			return fmt.Errorf("%v when writing audio chunk size header", err)
		}
	}
	// jump to the end of the file.
	e.w.Seek(0, 2)
	switch e.w.(type) {
	case *os.File:
		e.w.(*os.File).Sync()
	}
	return e.writeErr
}
