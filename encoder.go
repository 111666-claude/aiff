package aiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/go-audio/audio"
)

// Encoder encodes PCM data into an aiff content.
//
// The zero value of Encoding writes a plain AIFF container with big endian
// integer samples. When Encoding is set to one of the AIFC compression types
// an AIFC container is written instead.
type Encoder struct {
	w          io.WriteSeeker
	SampleRate int
	BitDepth   int
	NumChans   int
	// Encoding is the AIFC compression type. left at [4]byte{} the encoder
	// produces a plain (integer PCM, big endian) AIFF file.
	Encoding [4]byte

	WrittenBytes    int
	frames          int
	sampleBytes     int
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

// NewFloatEncoder creates an encoder writing AIFC files using the fl32
// (32 bit big endian IEEE 754 floating point) compression type.
// Samples must be provided via WriteFloat or WriteFloat32.
func NewFloatEncoder(w io.WriteSeeker, sampleRate, numChans int) *Encoder {
	return &Encoder{
		w:          w,
		SampleRate: sampleRate,
		BitDepth:   32,
		NumChans:   numChans,
		Encoding:   encFl32,
	}
}

// NewFloatEncoderWithEncoding is like NewFloatEncoder but allows selecting
// either the fl32 or FL32 compression type.
func NewFloatEncoderWithEncoding(w io.WriteSeeker, sampleRate, numChans int, encoding [4]byte) (*Encoder, error) {
	if !isFloatEncoding(encoding) {
		return nil, fmt.Errorf("%w: %q is not a supported floating point encoding", ErrFmtNotSupported, encoding)
	}
	e := NewFloatEncoder(w, sampleRate, numChans)
	e.Encoding = encoding
	return e, nil
}

// isFloat reports whether the encoder writes floating point samples.
func (e *Encoder) isFloat() bool {
	return isFloatEncoding(e.Encoding)
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

// addIntSamples serializes integer samples into bb. Integer samples are
// always big endian.
func (e *Encoder) addIntSamples(bb *bytes.Buffer, buf *audio.IntBuffer, numChans int) error {
	frameCount := len(buf.Data) / numChans
	var err error
	for i := 0; i < frameCount; i++ {
		for j := 0; j < numChans; j++ {
			v := buf.Data[i*numChans+j]
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
				return fmt.Errorf("%w: can't add frames of bit size %d", ErrFmtNotSupported, e.BitDepth)
			}
		}
	}
	return nil
}

// Write adds an integer PCM buffer to the file.
// It returns ErrBufferMismatch if the encoder was configured for floating
// point samples and a float buffer is expected instead.
func (e *Encoder) Write(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}
	if e.isFloat() {
		return fmt.Errorf("%w: floating point encoding %q requires WriteFloat", ErrBufferMismatch, e.Encoding)
	}
	if e.BitDepth != 8 && e.BitDepth != 16 && e.BitDepth != 24 && e.BitDepth != 32 {
		return fmt.Errorf("%w: can't add frames of bit size %d", ErrFmtNotSupported, e.BitDepth)
	}
	if buf.SourceBitDepth != 0 && buf.SourceBitDepth != e.BitDepth {
		return fmt.Errorf("%w: buffer bit depth %d, encoder bit depth %d", ErrBufferMismatch, buf.SourceBitDepth, e.BitDepth)
	}
	numChans, err := e.checkBufferFormat(buf.Format)
	if err != nil {
		return err
	}
	if len(buf.Data)%numChans != 0 {
		return fmt.Errorf("%w: %d samples for %d channels", ErrTruncatedPCM, len(buf.Data), numChans)
	}
	frames := len(buf.Data) / numChans
	return e.writeSamples(func(bb *bytes.Buffer) error {
		return e.addIntSamples(bb, buf, numChans)
	}, frames, e.BitDepth/8)
}

// WriteFloat adds a float64 PCM buffer to the file using the configured
// floating point encoding (fl32 or FL32). Samples are converted to 32 bit
// IEEE 754 big endian values. Values outside of the [-1, 1] range are allowed,
// but NaN, infinities and values that don't fit in a float32 are rejected with
// ErrInvalidSample.
func (e *Encoder) WriteFloat(buf *audio.FloatBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}
	if !e.isFloat() {
		return fmt.Errorf("%w: encoder isn't configured for floating point samples", ErrBufferMismatch)
	}
	if e.BitDepth != 32 {
		return fmt.Errorf("%w: %q requires 32 bit samples, got %d", ErrBufferMismatch, e.Encoding, e.BitDepth)
	}
	numChans, err := e.checkBufferFormat(buf.Format)
	if err != nil {
		return err
	}
	if len(buf.Data)%numChans != 0 {
		return fmt.Errorf("%w: %d samples for %d channels", ErrTruncatedPCM, len(buf.Data), numChans)
	}
	frames := len(buf.Data) / numChans
	return e.writeSamples(func(bb *bytes.Buffer) error {
		return addFloatSamples(bb, buf.Data, numChans)
	}, frames, 4)
}

// WriteFloat32 adds a float32 PCM buffer to the file using the configured
// floating point encoding. The samples are written verbatim (big endian) and
// no scaling is applied.
func (e *Encoder) WriteFloat32(buf *audio.Float32Buffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}
	if !e.isFloat() {
		return fmt.Errorf("%w: encoder isn't configured for floating point samples", ErrBufferMismatch)
	}
	if e.BitDepth != 32 {
		return fmt.Errorf("%w: %q requires 32 bit samples, got %d", ErrBufferMismatch, e.Encoding, e.BitDepth)
	}
	if buf.SourceBitDepth != 0 && buf.SourceBitDepth != 32 {
		return fmt.Errorf("%w: buffer bit depth %d, encoder bit depth %d", ErrBufferMismatch, buf.SourceBitDepth, e.BitDepth)
	}
	numChans, err := e.checkBufferFormat(buf.Format)
	if err != nil {
		return err
	}
	if len(buf.Data)%numChans != 0 {
		return fmt.Errorf("%w: %d samples for %d channels", ErrTruncatedPCM, len(buf.Data), numChans)
	}
	frames := len(buf.Data) / numChans
	return e.writeSamples(func(dst *bytes.Buffer) error {
		for i, v := range buf.Data {
			if err := validateFloatSample(float64(v)); err != nil {
				return fmt.Errorf("sample %d: %w", i, err)
			}
			if err := binary.Write(dst, binary.BigEndian, math.Float32bits(v)); err != nil {
				return err
			}
		}
		return nil
	}, frames, 4)
}

// checkBufferFormat verifies that the buffer format is compatible with the
// encoder configuration and returns the number of channels to use when
// serializing the samples. A nil format is tolerated (all the samples are then
// considered mono) to stay compatible with the historical integer behavior.
func (e *Encoder) checkBufferFormat(format *audio.Format) (int, error) {
	if e.NumChans < 1 {
		return 0, fmt.Errorf("%w: invalid number of channels %d", ErrBufferMismatch, e.NumChans)
	}
	if format == nil {
		if e.NumChans > 1 {
			return 0, fmt.Errorf("%w: nil buffer format with %d output channels", ErrBufferMismatch, e.NumChans)
		}
		return 1, nil
	}
	if format.NumChannels == 0 {
		// an unset channel count is treated as mono.
		if e.NumChans > 1 {
			return 0, fmt.Errorf("%w: buffer has no channel count, encoder has %d", ErrBufferMismatch, e.NumChans)
		}
		return 1, nil
	}
	if format.NumChannels != e.NumChans {
		return 0, fmt.Errorf("%w: buffer has %d channels, encoder has %d", ErrBufferMismatch, format.NumChannels, e.NumChans)
	}
	if format.SampleRate != 0 && e.SampleRate != 0 && format.SampleRate != e.SampleRate {
		return 0, fmt.Errorf("%w: buffer sample rate %d, encoder sample rate %d", ErrBufferMismatch, format.SampleRate, e.SampleRate)
	}
	return e.NumChans, nil
}

// writeSamples writes the header if needed, the SSND chunk header if needed
// and then the serialized samples. The serialization function is invoked only
// after the buffer was validated, so a failed call never writes a partial
// frame. Once a write fails, the error is latched and all subsequent writes
// fail with it.
func (e *Encoder) writeSamples(serialize func(*bytes.Buffer) error, frames, bytesPerSample int) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	if e.closed {
		return fmt.Errorf("can't write to a closed encoder")
	}
	bb := bytes.NewBuffer(nil)
	if err := serialize(bb); err != nil {
		return err
	}
	frameSize := e.NumChans * bytesPerSample
	expected := frames * frameSize
	if bb.Len() != expected {
		return fmt.Errorf("%w: expected %d bytes of samples, got %d", ErrTruncatedPCM, expected, bb.Len())
	}

	if err := e.writeHeader(); err != nil {
		e.writeErr = err
		return err
	}
	if !e.pcmChunkStarted {
		// other chunks
		// audio frames
		if err := e.AddBE([]byte("SSND")); err != nil {
			return e.latch(fmt.Errorf("%v when writing SSND chunk ID header", err))
		}

		e.pcmChunkSizePos = e.WrittenBytes
		e.pcmChunkStarted = true

		// temporary blocksize uint32
		if err := e.AddBE(uint32(8)); err != nil {
			return e.latch(fmt.Errorf("%v when writing SSND chunk size header", err))
		}

		if err := e.AddBE(uint32(0)); err != nil {
			return e.latch(fmt.Errorf("%v when writing SSND offset", err))
		}
		if err := e.AddBE(uint32(0)); err != nil {
			return e.latch(fmt.Errorf("%v when writing SSND block size", err))
		}
	}

	n, err := e.w.Write(bb.Bytes())
	// only whole frames are accounted for, a short write never registers a
	// partial frame.
	written := n / frameSize * frameSize
	e.WrittenBytes += n
	e.sampleBytes += written
	e.frames += written / frameSize
	if err != nil {
		return e.latch(fmt.Errorf("failed to write samples: %w", err))
	}
	if n != bb.Len() {
		return e.latch(fmt.Errorf("failed to write samples: %w", io.ErrShortWrite))
	}
	return nil
}

// latch records the first write error so that a caller can't keep writing to a
// partially written file.
func (e *Encoder) latch(err error) error {
	if e.writeErr == nil {
		e.writeErr = err
	}
	return e.writeErr
}

// addFloatSamples validates and serializes float samples as big endian
// 32 bit IEEE 754 values.
func addFloatSamples(bb *bytes.Buffer, data []float64, numChans int) error {
	if numChans < 1 {
		return fmt.Errorf("%w: invalid number of channels %d", ErrBufferMismatch, numChans)
	}
	if len(data)%numChans != 0 {
		return fmt.Errorf("%w: %d samples for %d channels", ErrTruncatedPCM, len(data), numChans)
	}
	for i, v := range data {
		if err := validateFloatSample(v); err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
		if err := binary.Write(bb, binary.BigEndian, math.Float32bits(float32(v))); err != nil {
			return err
		}
	}
	return nil
}

// validateFloatSample makes sure a sample can be represented as a finite
// 32 bit IEEE 754 value.
func validateFloatSample(v float64) error {
	if math.IsNaN(v) {
		return fmt.Errorf("%w: NaN", ErrInvalidSample)
	}
	if math.IsInf(v, 0) {
		return fmt.Errorf("%w: infinity", ErrInvalidSample)
	}
	if v > math.MaxFloat32 || v < -math.MaxFloat32 {
		return fmt.Errorf("%w: %v overflows a float32", ErrInvalidSample, v)
	}
	return nil
}

func (e *Encoder) writeHeader() error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.w == nil {
		return fmt.Errorf("can't write to a nil writer")
	}
	if e.headerWritten || e.WrittenBytes > 0 {
		return nil
	}
	if e.NumChans < 1 {
		return fmt.Errorf("%w: invalid number of channels %d", ErrBufferMismatch, e.NumChans)
	}
	if e.isFloat() && e.BitDepth != 32 {
		return fmt.Errorf("%w: %q requires 32 bit samples, got %d", ErrBufferMismatch, e.Encoding, e.BitDepth)
	}
	if e.Encoding != encNotSet && !e.isFloat() {
		return fmt.Errorf("%w: can't write the %q AIFC encoding", ErrFmtNotSupported, e.Encoding)
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
	format := aiffID
	if e.Encoding != encNotSet {
		format = aifcID
	}
	if err := e.AddBE(format); err != nil {
		return fmt.Errorf("%v when writing format header", err)
	}

	if e.Encoding != encNotSet {
		// The FVER chunk is mandatory in AIFC files.
		if err := e.AddBE([]byte("FVER")); err != nil {
			return fmt.Errorf("%v when writing FVER chunk ID header", err)
		}
		if err := e.AddBE(uint32(4)); err != nil {
			return fmt.Errorf("%v when writing FVER chunk size", err)
		}
		if err := e.AddBE(aifcVersion1); err != nil {
			return fmt.Errorf("%v when writing FVER version", err)
		}
	}

	// comm chunk
	if err := e.AddBE(COMMID); err != nil {
		return fmt.Errorf("%v when writing comm chunk ID header", err)
	}

	// The COMM chunk of an AIFC file carries the compression type and a
	// pascal style compression name.
	var encName []byte
	if e.Encoding != encNotSet {
		encName = []byte(encodingName(e.Encoding))
	}
	commSize := 18
	if e.Encoding != encNotSet {
		commSize += 4 + 1 + len(encName)
	}
	if err := e.AddBE(uint32(commSize)); err != nil {
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
	if err := e.AddBE(uint16(e.BitDepth)); err != nil {
		return fmt.Errorf("%v when writing comm chan numbers", err)
	}
	// sample rate in IeeeFloat (10 bytes)
	if err := e.AddBE(audio.IntToIEEEFloat(int(e.SampleRate))); err != nil {
		return fmt.Errorf("%v when writing comm sample rate", err)
	}
	if e.Encoding != encNotSet {
		if err := e.AddBE(e.Encoding); err != nil {
			return fmt.Errorf("%v when writing comm encoding", err)
		}
		if err := e.AddBE(uint8(len(encName))); err != nil {
			return fmt.Errorf("%v when writing comm encoding name size", err)
		}
		if err := e.AddBE(encName); err != nil {
			return fmt.Errorf("%v when writing comm encoding name", err)
		}
		// chunks are padded to an even number of bytes.
		if commSize%2 != 0 {
			if err := e.AddBE(uint8(0)); err != nil {
				return fmt.Errorf("%v when writing comm padding", err)
			}
		}
	}
	e.headerWritten = true
	return nil
}

// Close flushes the content to disk, make sure the headers are up to date
// Note that the underlying writter is NOT being closed.
func (e *Encoder) Close() error {
	if e == nil || e.w == nil {
		return fmt.Errorf("can't close a nil encoder")
	}
	if e.closed {
		return e.writeErr
	}
	e.closed = true
	// nothing was ever written, don't create a partial/invalid file.
	if !e.headerWritten {
		return e.writeErr
	}
	// the sample count and chunk sizes are only updated when every sample
	// made it to the writer.
	totalSize := e.WrittenBytes
	// go back and write total size
	if _, err := e.w.Seek(int64(e.formSizePos), 0); err != nil {
		return err
	}
	if err := e.AddBE(uint32(totalSize) - 8); err != nil {
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
		chunksize := uint32(e.sampleBytes + 8)
		if err := e.AddBE(uint32(chunksize)); err != nil {
			return fmt.Errorf("%v when writing wav data chunk size header", err)
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
