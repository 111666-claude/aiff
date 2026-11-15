package aiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/go-audio/audio"
)

// Encoder encodes LPCM data into an aiff content.
type Encoder struct {
	w          io.WriteSeeker
	SampleRate int
	BitDepth   int
	NumChans   int

	WrittenBytes    int
	frames          int
	pcmChunkStarted bool
	pcmChunkSizePos int
	wroteHeader     bool

	// aifc enables AIFF-C output with little-endian ("sowt") PCM samples.
	aifc bool
	// closed prevents writes after the AIFF-C container was flushed.
	closed bool
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

// NewAIFCEncoder creates a new encoder writing an AIFF-C container with
// little-endian ("sowt") integer PCM samples.
// Only 16 and 24 bit samples with one or two channels are supported.
// Invalid configurations and buffers are reported by Write and Close instead
// of being written as a damaged file.
// Don't forget to add Frames to the encoder before writing.
func NewAIFCEncoder(w io.WriteSeeker, sampleRate, bitDepth, numChans int) *Encoder {
	return &Encoder{
		w:          w,
		SampleRate: sampleRate,
		BitDepth:   bitDepth,
		NumChans:   numChans,
		aifc:       true,
	}
}

// sowtCompressionName is the AIFF-C compression name written for sowt PCM.
const sowtCompressionName = "not compressed"

// validateAIFC checks that the encoder configuration can be represented as
// AIFF-C sowt PCM. It returns an error for any unsupported bit depth or
// channel count.
func (e *Encoder) validateAIFC() error {
	if e.NumChans != 1 && e.NumChans != 2 {
		return fmt.Errorf("aiff-c sowt: unsupported channel count %d, only mono and stereo are supported", e.NumChans)
	}
	switch e.BitDepth {
	case 16, 24:
	default:
		return fmt.Errorf("aiff-c sowt: unsupported bit depth %d, only 16 and 24 bit PCM are supported", e.BitDepth)
	}
	return nil
}

// validateAIFCBuffer checks that the buffer matches the encoder configuration.
func (e *Encoder) validateAIFCBuffer(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}
	if buf.Format == nil {
		return fmt.Errorf("aiff-c sowt: can't add a buffer without a format")
	}
	if buf.Format.NumChannels != e.NumChans {
		return fmt.Errorf("aiff-c sowt: buffer has %d channels but the encoder was created with %d", buf.Format.NumChannels, e.NumChans)
	}
	if buf.Format.SampleRate != 0 && buf.Format.SampleRate != e.SampleRate {
		return fmt.Errorf("aiff-c sowt: buffer has a sample rate of %d but the encoder was created with %d", buf.Format.SampleRate, e.SampleRate)
	}
	if buf.SourceBitDepth != 0 && buf.SourceBitDepth != e.BitDepth {
		return fmt.Errorf("aiff-c sowt: buffer holds %d bit samples but the encoder writes %d bit samples", buf.SourceBitDepth, e.BitDepth)
	}
	if len(buf.Data)%e.NumChans != 0 {
		return fmt.Errorf("aiff-c sowt: buffer holds %d samples which isn't a whole number of frames for %d channels", len(buf.Data), e.NumChans)
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

func (e *Encoder) addBuffer(buf *audio.IntBuffer) error {
	if buf == nil {
		return fmt.Errorf("can't add a nil buffer")
	}

	var byteOrder binary.ByteOrder = binary.BigEndian
	if e.aifc {
		byteOrder = binary.LittleEndian
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
				if err = binary.Write(bb, byteOrder, uint8(v)); err != nil {
					return err
				}
			case 16:
				if err = binary.Write(bb, byteOrder, int16(v)); err != nil {
					return err
				}
			case 24:
				if e.aifc {
					if err = binary.Write(bb, byteOrder, audio.Int32toInt24LEBytes(int32(v))); err != nil {
						return err
					}
				} else if err = binary.Write(bb, byteOrder, audio.Int32toInt24BEBytes(int32(v))); err != nil {
					return err
				}
			case 32:
				if err = binary.Write(bb, byteOrder, int32(v)); err != nil {
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
	return err
}

func (e *Encoder) writeHeader() error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.w == nil {
		return fmt.Errorf("can't write to a nil writer")
	}

	if e.WrittenBytes > 0 {
		return nil
	}

	if e.aifc {
		if err := e.validateAIFC(); err != nil {
			return err
		}
	}

	form := aiffID
	if e.aifc {
		form = aifcID
	}

	// ID
	if err := e.AddBE(formID); err != nil {
		return fmt.Errorf("%v when writing FORM header", err)
	}
	// size, will need to be updated later on (total size - 8)
	if err := e.AddBE(uint32(0)); err != nil {
		return fmt.Errorf("%v when writing size header", err)
	}
	// Format
	if err := e.AddBE(form); err != nil {
		return fmt.Errorf("%v when writing format header", err)
	}
	// comm chunk
	if err := e.AddBE(COMMID); err != nil {
		return fmt.Errorf("%v when writing comm chunk ID header", err)
	}
	// blocksize uint32
	commSize := uint32(18)
	commPadding := false
	if e.aifc {
		// 18 bytes of common data, 4 bytes of compression type and a
		// pascal style compression name. Chunk data is padded to an even
		// byte count and the pad byte is part of the declared chunk size.
		commSize = uint32(18 + 4 + 1 + len(sowtCompressionName))
		if commSize%2 != 0 {
			commSize++
			commPadding = true
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
	if err := e.AddBE(uint32(42)); err != nil {
		return fmt.Errorf("%v when writing comm num sample frames", err)
	}
	if err := e.AddBE(uint16(e.BitDepth)); err != nil {
		return fmt.Errorf("%v when writing comm chan numbers", err)
	}
	// sample rate in IeeeFloat (10 bytes)
	if err := e.AddBE(audio.IntToIEEEFloat(int(e.SampleRate))); err != nil {
		return fmt.Errorf("%v when writing comm sample rate", err)
	}
	if e.aifc {
		if err := e.AddBE(encSowt); err != nil {
			return fmt.Errorf("%v when writing comm compression type", err)
		}
		if err := e.AddBE(uint8(len(sowtCompressionName))); err != nil {
			return fmt.Errorf("%v when writing comm compression name length", err)
		}
		if err := e.AddBE([]byte(sowtCompressionName)); err != nil {
			return fmt.Errorf("%v when writing comm compression name", err)
		}
		if commPadding {
			if err := e.AddBE(uint8(0)); err != nil {
				return fmt.Errorf("%v when writing comm chunk padding", err)
			}
		}
	}
	e.wroteHeader = true
	return nil
}

func (e *Encoder) Write(buf *audio.IntBuffer) error {
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.aifc && e.closed {
		return fmt.Errorf("aiff-c sowt: can't write to a closed encoder")
	}
	if e.aifc {
		if err := e.validateAIFC(); err != nil {
			return err
		}
		if err := e.validateAIFCBuffer(buf); err != nil {
			return err
		}
	}

	if err := e.writeHeader(); err != nil {
		return err
	}

	if !e.pcmChunkStarted {
		// other chunks
		// audio frames
		if err := e.AddBE([]byte("SSND")); err != nil {
			return fmt.Errorf("%v when writing SSND chunk ID header", err)
		}

		e.pcmChunkSizePos = e.WrittenBytes
		e.pcmChunkStarted = true

		// temporary blocksize uint32
		//chunksize := uint32((int(e.BitDepth)/8)*int(e.NumChans)*len(e.Frames) + 8)
		if err := e.AddBE(uint32(84)); err != nil {
			return fmt.Errorf("%v when writing SSND chunk size header", err)
		}

		if err := e.AddBE(uint32(0)); err != nil {
			return fmt.Errorf("%v when writing SSND offset", err)
		}
		if err := e.AddBE(uint32(0)); err != nil {
			return fmt.Errorf("%v when writing SSND block size", err)
		}
	}

	return e.addBuffer(buf)
}

// Close flushes the content to disk, make sure the headers are up to date
// Note that the underlying writter is NOT being closed.
func (e *Encoder) Close() error {
	if e == nil {
		return fmt.Errorf("can't close a nil encoder")
	}
	if e.aifc {
		if err := e.validateAIFC(); err != nil {
			return err
		}
		if !e.wroteHeader {
			return fmt.Errorf("aiff-c sowt: can't close the encoder before writing sample data")
		}
		if e.closed {
			return nil
		}
		e.closed = true
		// AIFF chunks hold an even number of bytes, pad odd sized sample data.
		if (e.frames*e.NumChans*(e.BitDepth/8))%2 != 0 {
			n, err := e.w.Write([]byte{0})
			e.WrittenBytes += n
			if err != nil {
				return fmt.Errorf("%v when writing the SSND chunk padding", err)
			}
		}
	}

	// go back and write total size
	if _, err := e.w.Seek(4, 0); err != nil {
		return err
	}
	if err := e.AddBE(uint32(e.WrittenBytes) - 8); err != nil {
		return fmt.Errorf("%v when writing the total written bytes", err)
	}
	if _, err := e.w.Seek(22, 0); err != nil {
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
		// the chunk size covers the offset and block size fields plus the
		// sample bytes, the pad byte of an odd sized chunk isn't included.
		chunksize := uint32((int(e.BitDepth)/8)*int(e.NumChans)*e.frames + 8)
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
	return nil
}
