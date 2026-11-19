package aiff

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/go-audio/audio"
)

// Apple's IMA4 ("ima4") is the QuickTime IMA ADPCM variant used by many AIFC
// files. The SSND payload is a sequence of packet groups; one 34 byte packet
// per channel is stored for every 64 decoded sample frames:
//
//	bytes 0..1   big endian preamble:
//	               bits 15..7  signed 9 bit initial predictor (low 7 bits zero)
//	               bits  6..0  initial step index
//	bytes 2..33  64 4-bit nibbles, low nibble of each byte first
//
// The predictor and step index are reset at the start of every packet, and
// the packets of a group are interleaved sample by sample (left packet data
// precedes right packet data, per Apple's IMA technote TN1081).

// isIMA4 reports whether the file is an AIFC container using Apple's IMA4
// (QuickTime IMA ADPCM) encoding.
func (d *Decoder) isIMA4() bool {
	return d.Form == aifcID && (d.Encoding == encIma4 || d.Encoding == encIMA4)
}

// validateIMA4Comm checks the COMM values the IMA4 layout depends on.
func (d *Decoder) validateIMA4Comm() error {
	// A packet group holds one packet per channel and Apple only defines the
	// mono and stereo layouts for this codec.
	if d.NumChans < 1 || d.NumChans > ima4MaxChans {
		return fmt.Errorf("%w: ima4 supports 1 or 2 channels, COMM declares %d",
			ErrInvalidChannelCount, d.NumChans)
	}
	// COMM's sampleSize is the bit depth of the sound before compression, so
	// it must be a representable PCM width. ima4 always decodes to 16 bit.
	if d.BitDepth < 1 || d.BitDepth > 32 {
		return fmt.Errorf("%w: invalid sample size %d", ErrInvalidCOMM, d.BitDepth)
	}
	if d.SampleRate <= 0 {
		return fmt.Errorf("%w: invalid sample rate %d", ErrInvalidCOMM, d.SampleRate)
	}
	return nil
}

// validateIMA4Payload checks the SSND packet data against the frame count
// declared by COMM. The decoder delivers exactly NumSampleFrames frames: a
// final packet may hold samples past that count, but the payload must not
// store fewer packets, extra whole packets, or a partial packet.
func (d *Decoder) validateIMA4Payload() error {
	if d.ima4DataSize < 0 {
		return fmt.Errorf("%w: SSND offset %d exceeds the chunk data",
			ErrInvalidCOMM, -d.ima4DataSize)
	}

	group := int64(ima4PacketSize * int(d.NumChans))
	packets := d.ima4DataSize / group
	remainder := d.ima4DataSize % group

	if d.NumSampleFrames == 0 {
		if d.ima4DataSize == 0 {
			return nil
		}
		return fmt.Errorf("%w: COMM declares 0 sample frames but SSND holds %d packet byte(s)",
			ErrSampleCountMismatch, d.ima4DataSize)
	}

	needed := int64((uint64(d.NumSampleFrames) + ima4PacketSamples - 1) / ima4PacketSamples)

	if remainder != 0 {
		return fmt.Errorf("%w: SSND holds %d whole packet group(s) plus %d trailing byte(s), not a whole %d byte group",
			ErrTruncatedPacket, packets, remainder, group)
	}
	if packets < needed {
		return fmt.Errorf("%w: COMM declares %d sample frame(s) needing %d packet group(s) but SSND only holds %d",
			ErrSampleCountMismatch, d.NumSampleFrames, needed, packets)
	}
	if packets > needed {
		return fmt.Errorf("%w: %d extra packet group(s) after the %d needed for COMM's %d sample frame(s)",
			ErrTrailingPadding, packets-needed, needed, d.NumSampleFrames)
	}
	return nil
}

// ima4TotalSamples returns the number of interleaved samples the decoder will
// deliver, honoring the effective frame count declared by COMM.
func (d *Decoder) ima4TotalSamples() int {
	return int(d.NumSampleFrames) * int(d.NumChans)
}

// readIMA4 fills buf with decoded IMA4 samples and returns the number of
// samples written. Like the linear PCM path it returns (0, nil) once the
// stream is exhausted, so callers detect the end via a zero count.
func (d *Decoder) readIMA4(buf *audio.IntBuffer) (int, error) {
	if len(buf.Data) == 0 {
		return 0, nil
	}
	total := d.ima4TotalSamples()
	if d.ima4Finished || d.ima4Produced >= total {
		d.ima4Finished = true
		return 0, nil
	}

	needed := len(buf.Data)
	if left := total - d.ima4Produced; left < needed {
		needed = left
	}

	n := 0
	for n < needed {
		if d.ima4PendingOff >= len(d.ima4Pending) {
			if err := d.decodeIMA4Packet(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return n, err
			}
			continue
		}
		copied := copy(buf.Data[n:needed], d.ima4Pending[d.ima4PendingOff:])
		n += copied
		d.ima4PendingOff += copied
	}

	buf.Format = d.Format()
	buf.SourceBitDepth = ima4BitDepth
	d.ima4Produced += n

	if d.ima4Produced >= total {
		d.ima4Finished = true
	}
	return n, nil
}

// decodeIMA4Packet reads and decodes the next packet group into ima4Pending.
func (d *Decoder) decodeIMA4Packet() error {
	if d.ima4Finished {
		return io.EOF
	}
	samples, err := d.readIMA4Packet()
	if err != nil {
		if errors.Is(err, io.EOF) {
			d.ima4Finished = true
			if d.ima4Produced < d.ima4TotalSamples() {
				return fmt.Errorf("%w: SSND ended after %d of %d samples",
					ErrSampleCountMismatch, d.ima4Produced, d.ima4TotalSamples())
			}
		}
		return err
	}
	d.ima4Pending = samples
	d.ima4PendingOff = 0
	return nil
}

// readIMA4Packet reads one packet group (one packet per channel) and returns
// its samples interleaved by frame.
func (d *Decoder) readIMA4Packet() ([]int, error) {
	chans := int(d.NumChans)
	raw := make([]byte, ima4PacketSize*chans)
	if _, err := io.ReadFull(d.PCMChunk, raw); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: %v", ErrTruncatedPacket, err)
	}

	out := make([]int, ima4PacketSamples*chans)
	for c := 0; c < chans; c++ {
		decodeIMA4Block(raw[c*ima4PacketSize:(c+1)*ima4PacketSize], out[c:], chans)
	}
	return out, nil
}

// decodeIMA4Block decodes one 34 byte packet, writing a value every stride
// entries of dst so that the per-channel packets of a group end up
// interleaved by frame. The predictor state is local to the packet, which
// implements the per-packet reset required by the format.
func decodeIMA4Block(packet []byte, dst []int, stride int) {
	preamble := int16(binary.BigEndian.Uint16(packet[:2]))
	predictor := int32(preamble) &^ int32(0x7f)
	stepIndex := int32(preamble & 0x7f)
	// The spec allows step indices up to 88. Saturate rather than index past
	// the step table if an encoder stored a larger value.
	if stepIndex > ima4MaxStepIndex {
		stepIndex = ima4MaxStepIndex
	}

	n := 0
	for _, b := range packet[ima4PacketHeaderSize:] {
		// The low nibble comes first, then the high nibble.
		dst[n*stride] = int(ima4ExpandNibble(&predictor, &stepIndex, b&0x0f))
		n++
		dst[n*stride] = int(ima4ExpandNibble(&predictor, &stepIndex, b>>4))
		n++
	}
}

// fullIMA4Buffer decodes the whole SSND payload into a single buffer honoring
// COMM's frame count. It backs FullPCMBuffer.
func (d *Decoder) fullIMA4Buffer() (*audio.IntBuffer, error) {
	total := d.ima4TotalSamples()
	if d.ima4Produced >= total {
		// Mirror the linear PCM path: once the chunk has been consumed a
		// second call yields an empty buffer without an error.
		return &audio.IntBuffer{
			Data:           []int{},
			Format:         d.Format(),
			SourceBitDepth: ima4BitDepth,
		}, nil
	}
	data := make([]int, 0, total)
	for len(data) < total {
		samples, err := d.readIMA4Packet()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: SSND ended after %d of %d samples",
					ErrSampleCountMismatch, len(data), total)
			}
			return nil, err
		}
		if left := total - len(data); left < len(samples) {
			samples = samples[:left]
		}
		data = append(data, samples...)
	}
	d.ima4Produced = len(data)
	d.ima4Finished = true
	return &audio.IntBuffer{
		Data:           data,
		Format:         d.Format(),
		SourceBitDepth: ima4BitDepth,
	}, nil
}

// ima4ExpandNibble applies one ADPCM nibble to the predictor, clipping both
// the sample and the step index as mandated by the IMA specification.
func ima4ExpandNibble(predictor *int32, stepIndex *int32, nibble byte) int32 {
	step := ima4StepTable[*stepIndex]

	diff := step >> 3
	if nibble&4 != 0 {
		diff += step
	}
	if nibble&2 != 0 {
		diff += step >> 1
	}
	if nibble&1 != 0 {
		diff += step >> 2
	}
	if nibble&8 != 0 {
		*predictor -= diff
	} else {
		*predictor += diff
	}
	if *predictor > 32767 {
		*predictor = 32767
	} else if *predictor < -32768 {
		*predictor = -32768
	}

	*stepIndex += ima4IndexTable[nibble]
	if *stepIndex < 0 {
		*stepIndex = 0
	} else if *stepIndex > ima4MaxStepIndex {
		*stepIndex = ima4MaxStepIndex
	}
	return *predictor
}
