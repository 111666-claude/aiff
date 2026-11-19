package aiff

import "errors"

var (
	formID = [4]byte{'F', 'O', 'R', 'M'}
	aiffID = [4]byte{'A', 'I', 'F', 'F'}
	aifcID = [4]byte{'A', 'I', 'F', 'C'}
	// COMMID is the common chunk ID
	COMMID = [4]byte{'C', 'O', 'M', 'M'}
	COMTID = [4]byte{'C', 'O', 'M', 'T'}
	SSNDID = [4]byte{'S', 'S', 'N', 'D'}

	// Apple stuff
	chanID = [4]byte{'C', 'H', 'A', 'N'}
	bascID = [4]byte{'b', 'a', 's', 'c'}
	trnsID = [4]byte{'t', 'r', 'n', 's'}
	cateID = [4]byte{'c', 'a', 't', 'e'}

	// AIFC encodings
	encNotSet = [4]byte{}
	encNone   = [4]byte{'N', 'O', 'N', 'E'}
	// inverted byte order LE instead of BE (not really compression)
	encSowt = [4]byte{'s', 'o', 'w', 't'}
	// inverted byte order LE instead of BE (not really compression)
	encTwos = [4]byte{'t', 'w', 'o', 's'}
	encRaw  = [4]byte{'r', 'a', 'w', ' '}
	encIn24 = [4]byte{'i', 'n', '2', '4'}
	enc42n1 = [4]byte{'4', '2', 'n', '1'}
	encIn32 = [4]byte{'i', 'n', '3', '2'}
	enc23ni = [4]byte{'2', '3', 'n', 'i'}

	encFl32 = [4]byte{'f', 'l', '3', '2'}
	encFL32 = [4]byte{'F', 'L', '3', '2'}
	encFl64 = [4]byte{'f', 'l', '6', '4'}
	encFL64 = [4]byte{'F', 'L', '6', '4'}

	envUlaw = [4]byte{'u', 'l', 'a', 'w'}
	encULAW = [4]byte{'U', 'L', 'A', 'W'}
	encAlaw = [4]byte{'a', 'l', 'a', 'w'}
	encALAW = [4]byte{'A', 'L', 'A', 'W'}

	encDwvw = [4]byte{'D', 'W', 'V', 'W'}
	encGsm  = [4]byte{'G', 'S', 'M', ' '}
	encIma4 = [4]byte{'i', 'm', 'a', '4'}
	encIMA4 = [4]byte{'I', 'M', 'A', '4'}

	// IMA ADPCM step and index tables shared by the ima4 encoding.
	ima4StepTable = [89]int32{
		7, 8, 9, 10, 11, 12, 13, 14, 16, 17,
		19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
		50, 55, 60, 66, 73, 80, 88, 97, 107, 118,
		130, 143, 157, 173, 190, 209, 230, 253, 279, 307,
		337, 371, 408, 449, 494, 544, 598, 658, 724, 796,
		876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066,
		2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358,
		5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899,
		15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
	}
	ima4IndexTable = [16]int32{
		-1, -1, -1, -1, 2, 4, 6, 8,
		-1, -1, -1, -1, 2, 4, 6, 8,
	}

	// encAble
	encAble = [4]byte{'a', 'b', 'l', 'e'}

	// ErrFmtNotSupported is a generic error reporting an unknown format.
	ErrFmtNotSupported = errors.New("format not supported")
	// ErrUnexpectedData is a generic error reporting that the parser encountered unexpected data.
	ErrUnexpectedData = errors.New("unexpected data content")
	// ErrInvalidCOMM reports a COMM chunk which is truncated or contains
	// values incompatible with its declared encoding.
	ErrInvalidCOMM = errors.New("invalid COMM chunk")
	// ErrInvalidChannelCount reports a channel count the encoding can't represent.
	ErrInvalidChannelCount = errors.New("invalid channel count")
	// ErrTruncatedPacket reports IMA4 data which ends partway through a packet.
	ErrTruncatedPacket = errors.New("truncated IMA4 packet")
	// ErrSampleCountMismatch reports a COMM sample frame count which doesn't
	// match the amount of audio actually stored in the SSND chunk.
	ErrSampleCountMismatch = errors.New("IMA4 sample count mismatch")
	// ErrTrailingPadding reports IMA4 packets stored past the frame count
	// declared by COMM.
	ErrTrailingPadding = errors.New("unexpected IMA4 trailing data")

	// Debug is a flag that can be turned on to see more logs
	Debug = false
)

// IMA4 (Apple QuickTime IMA ADPCM) packet layout: a 2 byte preamble followed
// by 32 bytes of nibbles, so a 34 byte packet yields 64 samples.
const (
	ima4PacketSize       = 34
	ima4PacketSamples    = 64
	ima4PacketHeaderSize = 2
	ima4MaxStepIndex     = 88
	// ima4MaxChans is the widest layout this decoder supports: one 34 byte
	// packet per channel, interleaved by frame within a 64 frame block.
	ima4MaxChans = 2
	// ima4BitDepth is the width of the linear samples ima4 decodes to.
	ima4BitDepth = 16
)

func round(v float64, decimals int) float64 {
	var pow float64 = 1
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	return float64(int((v*pow)+0.5)) / pow
}

func nullTermStr(b []byte) string {
	return string(b[:clen(b)])
}

func clen(n []byte) int {
	for i := 0; i < len(n); i++ {
		if n[i] == 0 {
			return i
		}
	}
	return len(n)
}
