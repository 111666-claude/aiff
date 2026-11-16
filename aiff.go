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

	// encAble
	encAble = [4]byte{'a', 'b', 'l', 'e'}

	// ErrFmtNotSupported is a generic error reporting an unknown format.
	ErrFmtNotSupported = errors.New("format not supported")
	// ErrUnexpectedData is a generic error reporting that the parser encountered unexpected data.
	ErrUnexpectedData = errors.New("unexpected data content")
	// ErrInvalidSample reports a sample that can't be represented as a finite
	// 32 bit IEEE 754 float (NaN, infinity or an overflowing value).
	ErrInvalidSample = errors.New("invalid sample value")
	// ErrBufferMismatch reports a buffer whose format doesn't match the encoder
	// configuration (bit depth or number of channels).
	ErrBufferMismatch = errors.New("buffer format doesn't match the encoder configuration")
	// ErrTruncatedPCM reports PCM data that ends in the middle of a sample or frame.
	ErrTruncatedPCM = errors.New("truncated PCM data")
	// ErrFloatPCMIntBuffer reports that fl32/FL32 PCM can't be decoded in an
	// integer buffer without an explicit conversion.
	ErrFloatPCMIntBuffer = errors.New("floating point PCM must be decoded with a float buffer")

	// Exported AIFC encodings. They mirror the compression IDs stored in the
	// COMM chunk.
	EncodingNone = encNone
	EncodingSowt = encSowt
	EncodingTwos = encTwos
	EncodingRaw  = encRaw
	EncodingFl32 = encFl32
	EncodingFL32 = encFL32
	EncodingFl64 = encFl64
	EncodingFL64 = encFL64

	// Debug is a flag that can be turned on to see more logs
	Debug = false
)

// aifcVersion1 is the mandatory FVER timestamp of AIFC files.
var aifcVersion1 = [4]byte{0xA2, 0x80, 0x51, 0x40}

// isFloatEncoding reports whether enc is one of the 32 bit float AIFC
// compression types. Both tags store big endian IEEE 754 samples.
func isFloatEncoding(enc [4]byte) bool {
	return enc == encFl32 || enc == encFL32
}

// encodingName returns the pascal string description stored in the COMM chunk
// for the given compression type.
func encodingName(enc [4]byte) string {
	if isFloatEncoding(enc) {
		// 21 characters, the same description used by libsndfile and
		// Pure Data. With the 5 bytes of AIFC encoding info added to the 18
		// bytes of common fields the COMM chunk size stays even.
		return "32-bit floating point"
	}
	return ""
}

func (d *Decoder) isFloatEncoding() bool {
	return isFloatEncoding(d.Encoding)
}

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
