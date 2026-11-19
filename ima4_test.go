package aiff

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-audio/audio"
)

// The IMA4 fixtures are small, hand-built AIFC files. Their expected PCM was
// derived from an independent IMA ADPCM decoder and cross-checked against
// FFmpeg's adpcm_ima_qt decoder, so these values are not produced by the code
// under test.
var (
	ima4MonoSamples = []int{
		0, 11, 41, 104, 240, 143, -52, -182, -347, -497, -400, -240, -90, 46, 206, 356,
		492, 332, 182, 46, -114, -264, -400, -560, -710, -534, -416, -266, -90, 28, 178, 354,
		-1, -256, -118, 92, 283, 525, 305, 105, -25, -2, 19, 0, 17, 1, -13, 0,
		12, 1, -9, 0, 8, 1, -5, 1, -4, 1, -3, 1, -2, 1, -2, 0,
	}
	ima4StereoSamples = []int{
		-11, -11, -41, 19, -104, 40, -186, -16, -21, -139, 334, 127, 589, 89, 912, -293,
		281, -752, -533, 173, -205, 305, -106, -296, 165, -624, 576, 669, 799, 141, -221, -19,
		-657, -455, -525, 737, -165, 257, 163, -179, 461, -576, 732, 747, 978, 571, -142, 91,
		-622, -345, -186, -1007, -54, 557, 306, -82, 634, -276, 932, -804, -425, 638, -619, 56,
		-443, -120, 37, -600, 182, 711, 579, 183, 699, -297, -943, -733, -709, 724, -496, 142,
		-302, -34, 226, -514, 386, 797, 531, 269, 928, -211, -876, -356, -618, 836, -384, 356,
		-171, -80, 411, -477, 587, -837, 747, 586, -855, 4, -642, -172, -448, -652, 80, 659,
		240, 131, 385, -349, 782, -785, 902, 672, -740, 90, -506, -86, -293, -566, -99, 745,
	}
)

func openTestFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestIMA4FullPCMBuffer(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		numChans    int
		sampleRate  int
		frames      int
		wantSamples []int
	}{
		{"mono", "fixtures/ima4_mono.aifc", 1, 22050, 64, ima4MonoSamples},
		// A 7 byte SSND offset must be skipped before the packet data, and the
		// resulting odd declared chunk size yields a trailing pad byte.
		{"SSND offset and pad", "fixtures/ima4_mono_offset.aifc", 1, 22050, 64, ima4MonoSamples},
		{"upper case encoding", "fixtures/ima4_mono_upper.aifc", 1, 22050, 64, ima4MonoSamples},
		{"stereo interleaved", "fixtures/ima4_stereo.aifc", 2, 44100, 64, ima4StereoSamples},
		// COMM declares 40 frames even though the single packet holds 64.
		{"trimmed to declared frames", "fixtures/ima4_mono_short.aifc", 1, 22050, 40, ima4MonoSamples[:40]},
		{
			"two packets", "fixtures/ima4_mono_2packets.aifc", 1, 8000, 100,
			[]int{
				-11, -41, -104, -240, -259, -99, 8, 144, 304, 454, 551, 711, 388, -306, -405, -315,
				-233, -10, 58, 242, 298, 451, 589, 715, 141, -599, -500, -229, -147, -73, 131, 192,
				360, 513, 651, 777, 203, -537, -438, -167, -85, -11, 193, 254, 422, 575, 713, 82,
				-551, -469, -395, -191, -130, 38, 191, 329, 455, 646, 749, 276, -608, -488, -379, -81,
				// Second packet starts from its own preamble (predictor -128,
				// step index 48), not from the first packet's final state.
				-38, 44, 267, 335, 519, 687, 840, 146, -550, -460, -214, -140, 64, 125, 293, 446,
				584, 710, 136, -604, -505, -415, -169, -95, 109, 170, 338, 491, 629, 755, 181, -559,
				-460, -370, -124, -50,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder(openTestFile(t, tc.input))
			buf, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatal(err)
			}
			if len(buf.Data) != len(tc.wantSamples) {
				t.Fatalf("expected %d samples, got %d", len(tc.wantSamples), len(buf.Data))
			}
			for i, want := range tc.wantSamples {
				if buf.Data[i] != want {
					t.Fatalf("sample %d: expected %d, got %d", i, want, buf.Data[i])
				}
			}
			if buf.Format.NumChannels != tc.numChans {
				t.Fatalf("expected %d channels, got %d", tc.numChans, buf.Format.NumChannels)
			}
			if buf.Format.SampleRate != tc.sampleRate {
				t.Fatalf("expected sample rate %d, got %d", tc.sampleRate, buf.Format.SampleRate)
			}
			if buf.SourceBitDepth != ima4BitDepth {
				t.Fatalf("expected source bit depth %d, got %d", ima4BitDepth, buf.SourceBitDepth)
			}
			if frames := buf.NumFrames(); frames != tc.frames {
				t.Fatalf("expected %d frames, got %d", tc.frames, frames)
			}
			if d.SampleBitDepth() != ima4BitDepth {
				t.Fatalf("expected SampleBitDepth %d, got %d", ima4BitDepth, d.SampleBitDepth())
			}
		})
	}
}

func TestIMA4PCMBufferStreaming(t *testing.T) {
	// Buffer sizes are chosen to slice through packet groups and frames so the
	// decoder's internal per-packet cache is exercised across calls.
	for _, size := range []int{1, 3, 7, 16, 64, 127} {
		size := size
		t.Run(fmt.Sprintf("buffer_%d", size), func(t *testing.T) {
			d := NewDecoder(openTestFile(t, "fixtures/ima4_stereo.aifc"))
			buf := &audio.IntBuffer{Data: make([]int, size)}
			var got []int
			for {
				n, err := d.PCMBuffer(buf)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
				got = append(got, buf.Data[:n]...)
			}
			if len(got) != len(ima4StereoSamples) {
				t.Fatalf("buffer size %d: expected %d samples, got %d", size, len(ima4StereoSamples), len(got))
			}
			for i, want := range ima4StereoSamples {
				if got[i] != want {
					t.Fatalf("buffer size %d, sample %d: expected %d, got %d", size, i, want, got[i])
				}
			}
			if buf.Format == nil || buf.Format.NumChannels != 2 {
				t.Fatalf("buffer format was not populated: %#v", buf.Format)
			}
		})
	}
}

func TestIMA4Duration(t *testing.T) {
	d := NewDecoder(openTestFile(t, "fixtures/ima4_stereo.aifc"))
	got, err := d.Duration()
	if err != nil {
		t.Fatal(err)
	}
	seconds := 64.0 / 44100.0
	want := time.Duration(seconds * float64(time.Second))
	if got != want {
		t.Fatalf("expected duration %v, got %v", want, got)
	}
}

func TestIMA4Rewind(t *testing.T) {
	d := NewDecoder(openTestFile(t, "fixtures/ima4_mono.aifc"))
	first, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Rewind(); err != nil {
		t.Fatal(err)
	}
	second, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Data) != len(second.Data) {
		t.Fatalf("expected %d samples after rewind, got %d", len(first.Data), len(second.Data))
	}
	for i := range first.Data {
		if first.Data[i] != second.Data[i] {
			t.Fatalf("sample %d changed after rewind: %d then %d", i, first.Data[i], second.Data[i])
		}
	}
}

func TestIMA4IsValidFile(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"fixtures/ima4_mono.aifc", true},
		{"fixtures/ima4_stereo.aifc", true},
		{"fixtures/ima4_bad_channels.aifc", false},
		{"fixtures/ima4_bad_bits.aifc", false},
		{"fixtures/ima4_bad_comm.aifc", false},
	}
	for _, tc := range tests {
		d := NewDecoder(openTestFile(t, tc.input))
		if got := d.IsValidFile(); got != tc.valid {
			t.Errorf("%s: expected valid=%t, got %t (%v)", tc.input, tc.valid, got, d.Err())
		}
	}
}

func TestIMA4Errors(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		target error
	}{
		{"truncated packet", "fixtures/ima4_truncated_packet.aifc", ErrTruncatedPacket},
		{"truncated chunk header", "fixtures/ima4_bad_ssnd_size.aifc", ErrTruncatedPacket},
		{"frames exceed packets", "fixtures/ima4_frames_too_many.aifc", ErrSampleCountMismatch},
		{"zero frames with data", "fixtures/ima4_zero_frames.aifc", ErrSampleCountMismatch},
		{"extra packet", "fixtures/ima4_trailing_packet.aifc", ErrTrailingPadding},
		{"invalid channels", "fixtures/ima4_bad_channels.aifc", ErrInvalidChannelCount},
		{"invalid sample size", "fixtures/ima4_bad_bits.aifc", ErrInvalidCOMM},
		{"truncated COMM", "fixtures/ima4_bad_comm.aifc", ErrInvalidCOMM},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder(openTestFile(t, tc.input))
			_, err := d.FullPCMBuffer()
			if err == nil {
				t.Fatalf("expected %v, got no error", tc.target)
			}
			if !errors.Is(err, tc.target) {
				t.Fatalf("expected error matching %v, got %v", tc.target, err)
			}
		})
	}
}

func TestIMA4DecodeBlock(t *testing.T) {
	// A hand-built packet: predictor 0x1000 (4096) and step index 0 in the
	// preamble, then one byte carrying nibble 0xF low and 0x0 high. The low
	// nibble must be decoded first, the sign bit must subtract, and the
	// remaining bytes (all zero nibbles) continue from the packet state.
	packet := make([]byte, ima4PacketSize)
	packet[0] = 0x10 // predictor 0x1000, step index 0
	packet[1] = 0x00
	packet[2] = 0x0F // low nibble 0xF (sign + magnitude 7), high nibble 0x0

	dst := make([]int, ima4PacketSamples)
	decodeIMA4Block(packet, dst, 1)

	// step 0 is 7: diff = 7>>3 + 7 + 7>>1 + 7>>2 = 0 + 7 + 3 + 1 = 11.
	if want := 4096 - 11; dst[0] != want {
		t.Fatalf("first (low) nibble: expected %d, got %d", want, dst[0])
	}
	// The next nibble is a zero: predictor moves up by the new step's 1/8.
	if dst[1] <= dst[0] {
		t.Fatalf("high nibble wasn't decoded second: %d then %d", dst[0], dst[1])
	}
	if len(dst) != ima4PacketSamples {
		t.Fatalf("expected %d samples, got %d", ima4PacketSamples, len(dst))
	}
}

func TestIMA4ExpandNibbleClips(t *testing.T) {
	// Drive the predictor past the positive limit and confirm it saturates.
	predictor := int32(32760)
	stepIndex := int32(ima4MaxStepIndex)
	for i := 0; i < 4; i++ {
		if got := ima4ExpandNibble(&predictor, &stepIndex, 0x7); got != 32767 {
			t.Fatalf("expected clipping to 32767, got %d", got)
		}
	}
	if stepIndex > ima4MaxStepIndex {
		t.Fatalf("step index wasn't clipped: %d", stepIndex)
	}

	predictor = -32760
	stepIndex = int32(ima4MaxStepIndex)
	for i := 0; i < 4; i++ {
		if got := ima4ExpandNibble(&predictor, &stepIndex, 0xF); got != -32768 {
			t.Fatalf("expected clipping to -32768, got %d", got)
		}
	}
}

func TestIMA4DoesNotAffectLinearPCM(t *testing.T) {
	// Guard against the IMA4 path leaking into regular AIFF decoding.
	for _, path := range []string{"fixtures/kick.aif", "fixtures/sowt.aif", "fixtures/kick8b.aiff"} {
		d := NewDecoder(openTestFile(t, path))
		buf, err := d.FullPCMBuffer()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(buf.Data) == 0 {
			t.Fatalf("%s: expected PCM data", path)
		}
		if d.isIMA4() {
			t.Fatalf("%s: wrongly identified as IMA4", path)
		}
	}
}
