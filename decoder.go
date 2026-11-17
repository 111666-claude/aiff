package aiff

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"time"

	"bytes"

	"github.com/go-audio/audio"
)

// Decoder is the wrapper structure for the AIFF container
type Decoder struct {
	r io.ReadSeeker

	// ID is always 'FORM'. This indicates that this is a FORM chunk
	ID [4]byte
	// Size contains the size of data portion of the 'FORM' chunk.
	// Note that the data portion has been
	// broken into two parts, formType and chunks
	Size uint32
	// Form describes what's in the 'FORM' chunk. For Audio IFF files,
	// formType (aka Format) is always 'AIFF'.
	// This indicates that the chunks within the FORM pertain to sampled sound.
	Form [4]byte

	// Data coming from the COMM chunk
	commSize        uint32
	NumChans        uint16
	NumSampleFrames uint32
	BitDepth        uint16
	SampleRate      int
	//
	PCMSize  uint32
	PCMChunk *Chunk
	//
	Comments []string

	// AIFC data
	Encoding     [4]byte
	EncodingName string

	// SSND chunk header values. The offset is the number of bytes of
	// non-audio data stored before the samples and the block size is the
	// alignment of the samples (usually 0).
	SSNDOffset    uint32
	SSNDBlockSize uint32

	// Apple specific
	HasAppleInfo bool
	AppleInfo    AppleMetadata

	err             error
	pcmDataAccessed bool

	byteOrder binary.ByteOrder

	// read the file information to setup the audio clip
	// find the beginning of the SSND chunk and set the clip reader to it.
	rewindBytes int64
}

// NewDecoder creates a new reader reading the given reader and pushing audio data to the given channel.
// It is the caller's responsibility to call Close on the reader when done.
func NewDecoder(r io.ReadSeeker) *Decoder {
	return &Decoder{r: r, byteOrder: binary.BigEndian}
}

// SampleBitDepth returns the bit depth encoding of each sample.
func (d *Decoder) SampleBitDepth() int32 {
	if d == nil {
		return 0
	}
	return int32(d.BitDepth)
}

// DecodedBitDepth returns the bit depth of the linear PCM values returned by
// the decoder. It is the COMM chunk bit depth except for compressed encodings
// (G.711 μ-law decodes to 16 bit linear PCM).
func (d *Decoder) DecodedBitDepth() int32 {
	if d == nil {
		return 0
	}
	return int32(d.decodedBitDepth())
}

// IsULaw reports whether the decoded content uses AIFC G.711 μ-law
// compression.
func (d *Decoder) IsULaw() bool {
	if d == nil {
		return false
	}
	return isULawEncoding(d.Encoding)
}

// PCMLen returns the total number of bytes in the PCM data chunk
func (d *Decoder) PCMLen() int64 {
	if d == nil {
		return 0
	}
	return int64(d.PCMSize)
}

// Err returns the first non-EOF error that was encountered by the Decoder.
func (d *Decoder) Err() error {
	if d.err == io.EOF {
		return nil
	}
	return d.err
}

// EOF returns positively if the underlying reader reached the end of file.
func (d *Decoder) EOF() bool {
	if d == nil || d.err == io.EOF {
		return true
	}
	return false
}

// WasPCMAccessed returns positively if the PCM data was previously accessed.
func (d *Decoder) WasPCMAccessed() bool {
	if d == nil {
		return false
	}
	return d.pcmDataAccessed
}

// Format returns the audio format of the decoded content.
func (d *Decoder) Format() *audio.Format {
	if d == nil {
		return nil
	}
	return &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}
}

// NextChunk returns the next available chunk
func (d *Decoder) NextChunk() (*Chunk, error) {
	// we need to read the info so we have access to the encoding.
	if d.ReadInfo(); d.err != nil {
		d.err = fmt.Errorf("failed to read info - %w", d.err)
		return nil, d.err
	}

	var (
		id   [4]byte
		size uint32
	)

	id, size, d.err = d.iDnSize()
	declaredSize := size
	if size%2 != 0 {
		// realign, the encoder lied about the size of the chunk header :(
		size++
	}
	if d.err != nil {
		if d.err == io.EOF || d.err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("error reading chunk header - %v", d.err)
	}

	c := &Chunk{
		ID:           id,
		Size:         int(size),
		DeclaredSize: int(declaredSize),
		R:            io.LimitReader(d.r, int64(size)),
	}

	return c, d.err
}

// IsValidFile verifies that the file is valid/readable.
func (d *Decoder) IsValidFile() bool {
	d.ReadInfo()
	if d.err != nil {
		return false
	}
	if d.NumChans < 1 {
		return false
	}
	if d.BitDepth < 8 {
		return false
	}
	if d, err := d.Duration(); err != nil || d <= 0 {
		return false
	}
	switch d.Encoding {
	case encSowt, encNone, encNotSet, envUlaw, encULAW:
	default:
		return false
	}
	if isULawEncoding(d.Encoding) && d.BitDepth != 8 {
		return false
	}
	// Validate that the SSND chunk agrees with the COMM chunk without
	// consuming the reader so the caller can keep decoding the file.
	if err := d.validateSSND(); err != nil {
		return false
	}

	return true
}

// validateSSND walks the chunk headers to locate the SSND chunk and checks its
// header and sample data length against the COMM chunk. The reader position is
// restored before returning, so the decoder can still be used to read the
// samples. Files with a layout that can't be walked are left to FwdToPCM to
// report the error when the samples are actually read.
func (d *Decoder) validateSSND() error {
	if d.PCMChunk != nil || d.r == nil {
		// the SSND chunk was already validated when it was located.
		return nil
	}
	pos, err := d.r.Seek(0, io.SeekCurrent)
	if err != nil {
		// the reader can't be rewound, skip the check.
		return nil
	}
	defer d.r.Seek(pos, io.SeekStart)
	end, err := d.r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil
	}
	if _, err := d.r.Seek(12, io.SeekStart); err != nil {
		return nil
	}
	for {
		var (
			id   [4]byte
			size uint32
		)
		if err := binary.Read(d.r, binary.BigEndian, &id); err != nil {
			// the chunk list couldn't be walked, let FwdToPCM report the
			// problem if the samples are actually read.
			return nil
		}
		if err := binary.Read(d.r, binary.BigEndian, &size); err != nil {
			return nil
		}
		if id == SSNDID {
			if size < 8 {
				return fmt.Errorf("%w: SSND chunk of %d bytes can't hold the offset and block size fields", ErrUnexpectedData, size)
			}
			var offset, blockSize uint32
			if err := binary.Read(d.r, binary.BigEndian, &offset); err != nil {
				return fmt.Errorf("PCM offset failed to parse - %s", err)
			}
			if err := binary.Read(d.r, binary.BigEndian, &blockSize); err != nil {
				return fmt.Errorf("PCM block size failed to parse - %s", err)
			}
			dataLen := int64(size) - 8
			if int64(offset) > dataLen {
				return fmt.Errorf("%w: SSND offset %d exceeds the %d bytes of sample data", ErrUnexpectedData, offset, dataLen)
			}
			dataLen -= int64(offset)
			if err := d.checkPCMFrames(uint64(dataLen)); err != nil {
				return err
			}
			// Reject a file that doesn't actually hold the declared
			// sample data. The reader is at the start of the offsetted
			// comment bytes.
			if start, err := d.r.Seek(0, io.SeekCurrent); err == nil {
				if start+int64(offset)+dataLen > end {
					return fmt.Errorf("%w: SSND declares %d sample byte(s) but only %d remain in the file",
						ErrTruncatedPCM, dataLen, end-start-int64(offset))
				}
			}
			return nil
		}
		if _, err := d.r.Seek(int64(size)+int64(size%2), io.SeekCurrent); err != nil {
			return nil
		}
	}
}

// Duration returns the time duration for the current AIFF container
func (d *Decoder) Duration() (time.Duration, error) {
	if d == nil {
		return 0, errors.New("can't calculate the duration of a nil pointer")
	}
	d.ReadInfo()
	if err := d.Err(); err != nil {
		return 0, err
	}
	duration := time.Duration(float64(d.NumSampleFrames) / float64(d.SampleRate) * float64(time.Second))
	return duration, nil
}

// Tempo returns a tempo when available, otherwise -1
func (d *Decoder) Tempo() float64 {
	if d == nil || !d.HasAppleInfo || d.AppleInfo.Beats < 1 {
		return -1
	}
	duration, err := d.Duration()
	if err != nil {
		return -1
	}
	return round(float64(d.AppleInfo.Beats)/(duration.Seconds()/60.0), 2)
}

// Drain parses the remaining chunks
func (d *Decoder) Drain() error {
	var chunk *Chunk
	for d.err == nil {
		chunk, d.err = d.NextChunk()
		if d.err != nil {
			if d.err == io.EOF {
				return nil
			}
			return d.err
		}
		if err := d.parseChunk(chunk); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
	return d.err
}

// FwdToPCM forwards the underlying reader until the start of the PCM chunk.
// If the PCM chunk was already read, no data will be found (you need to rewind).
func (d *Decoder) FwdToPCM() error {
	if d.err = d.readHeaders(); d.err != nil {
		d.err = fmt.Errorf("failed to read header - %w", d.err)
		return nil
	}

	var chunk *Chunk
	for d.err == nil {
		chunk, d.err = d.NextChunk()
		if d.err != nil {
			d.err = fmt.Errorf("failed to read next chunk: %w", d.err)
			return d.err
		}

		if chunk.ID == SSNDID {
			if err := d.parseSSNDChunk(chunk); err != nil {
				d.err = err
				return err
			}
			d.PCMChunk = chunk
			d.pcmDataAccessed = true
			return nil
		}

		if err := d.parseChunk(chunk); err != nil {
			return fmt.Errorf("failed to parse the chunk - %w", err)
		}
	}
	return d.err
}

// parseSSNDChunk reads the SSND chunk header (offset and block size fields),
// skips the offsetted comment bytes and validates the amount of sample data
// against the COMM chunk. The chunk reader is left at the start of the sample
// data and must not be used to read the samples after this call.
//
//	 0      4 bytes  "SSND"
//	 4      4 bytes  <Chunk size(x)>
//	 8      4 bytes  <Offset(n)>
//	12      4 bytes  <block size>
//	16     (n)bytes  Comment
//	16+(n) (s)bytes  <Sample data>
func (d *Decoder) parseSSNDChunk(chunk *Chunk) error {
	if chunk.ID != SSNDID {
		return fmt.Errorf("unexpected SSND chunk ID: %q", chunk.ID)
	}
	if chunk.declaredSize() < 8 {
		return fmt.Errorf("%w: SSND chunk of %d bytes can't hold the offset and block size fields", ErrUnexpectedData, chunk.declaredSize())
	}
	var offset, blockSize uint32
	if err := chunk.ReadBE(&offset); err != nil {
		return fmt.Errorf("PCM offset failed to parse - %s", err)
	}
	if err := chunk.ReadBE(&blockSize); err != nil {
		return fmt.Errorf("PCM block size failed to parse - %s", err)
	}
	d.SSNDOffset = offset
	d.SSNDBlockSize = blockSize
	// The chunk size covers the offset, the block size and the sample data.
	// The optional pad byte that keeps the chunk even sized isn't audio
	// data, so the declared size is used when known.
	dataLen := int64(chunk.declaredSize()) - 8
	if int64(offset) > dataLen {
		return fmt.Errorf("%w: SSND offset %d exceeds the %d bytes of sample data", ErrUnexpectedData, offset, dataLen)
	}
	dataLen -= int64(offset)
	d.PCMSize = uint32(dataLen)
	if d.SampleRate <= 0 || d.NumChans < 1 {
		return fmt.Errorf("%w: no valid COMM chunk was found before the audio data", ErrUnexpectedData)
	}
	if err := d.validatePCMFrames(); err != nil {
		return err
	}
	// Make sure the file actually holds the declared sample data: a
	// truncated file otherwise decodes to fewer samples without any error.
	// The reader is still before the offsetted comment bytes.
	required := int64(offset) + int64(d.PCMSize)
	if remaining, err := d.remainingBytes(); err == nil && remaining < required {
		return fmt.Errorf("%w: SSND declares %d sample byte(s) but only %d remain in the file",
			ErrTruncatedPCM, d.PCMSize, remaining-int64(offset))
	}
	if offset > 0 {
		// skip the offsetted (comment) bytes, those aren't sample data.
		buf := make([]byte, offset)
		if _, err := io.ReadFull(chunk, buf); err != nil {
			return fmt.Errorf("failed to read the offsetted buffer - %v", err)
		}
	}
	return nil
}

// Reset resets the decoder (and rewind the underlying reader)
func (d *Decoder) Reset() {
	d.ID = [4]byte{}
	d.Size = 0
	d.Form = [4]byte{}
	d.commSize = 0
	d.NumChans = 0
	d.NumSampleFrames = 0
	d.BitDepth = 0
	d.SampleRate = 0
	d.Encoding = [4]byte{}
	d.EncodingName = ""
	d.SSNDOffset = 0
	d.SSNDBlockSize = 0
	d.PCMSize = 0
	d.PCMChunk = nil
	d.byteOrder = binary.BigEndian
	d.rewindBytes = 0
	d.err = nil
	d.pcmDataAccessed = false
	d.r.Seek(0, 0)
}

// Seek provides access to the cursor position in the PCM data
func (d *Decoder) Seek(offset int64, whence int) (int64, error) {
	return d.r.Seek(offset, whence)
}

// Rewind allows the decoder to be rewound to the beginning of the PCM data.
// This is useful if you want to keep on decoding the same file in a loop.
func (d *Decoder) Rewind() error {
	d.Reset()
	return nil
}

// FullPCMBuffer is an inneficient way to access all the PCM data contained in the
// audio container. The entire PCM data is held in memory.
// Consider using Buffer() instead.
func (d *Decoder) FullPCMBuffer() (*audio.IntBuffer, error) {
	if !d.WasPCMAccessed() {
		err := d.FwdToPCM()
		if err != nil {
			return nil, fmt.Errorf("failed to forward to PCM: %w", err)
		}
	}
	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	decodeF, _, err := d.decodeFunc()
	if err != nil {
		return nil, fmt.Errorf("could not get sample decode func %v", err)
	}
	sourceBitDepth := d.decodedBitDepth()
	bPerSample := d.encodedBytesPerSample()

	chunkSize := 4096
	buf := &audio.IntBuffer{Data: make([]int, chunkSize),
		Format:         format,
		SourceBitDepth: sourceBitDepth,
	}

	sampleBuf := make([]byte, bPerSample, bPerSample)
	i := 0
	var n int
	chunkSize = 2048 * bPerSample
	sizeToRead := chunkSize
	// PCMSize is the number of sample bytes in the chunk, the chunk reader
	// may also expose a pad byte that must not be decoded as audio.
	r := d.pcmReader()
	var innerErr error
	for err == nil {
		// to avoid doing too many small reads (bad performance)
		// we are loading part of the chunk in memory and reading from there
		sizeToRead = chunkSize
		if leftOverSize := int(d.PCMSize) - d.pcmBytesRead(); leftOverSize < chunkSize {
			sizeToRead = leftOverSize
		}
		if sizeToRead < 1 {
			break
		}
		optBuf := make([]byte, sizeToRead)
		n, err = r.Read(optBuf)
		if err != nil {
			// fmt.Println("-->", sizeToRead, err)
			break
		}
		if n == 0 {
			break
		}
		smallRead := n != sizeToRead
		optBuf = optBuf[:n]

		bufReader := bytes.NewReader(optBuf)
		for innerErr == nil {
			buf.Data[i], innerErr = decodeF(bufReader, sampleBuf)
			if innerErr != nil {
				if innerErr == io.EOF {
					innerErr = nil
				}
				break
			}
			i++
			// grow the underlying slice if needed
			if i >= len(buf.Data) {
				buf.Data = append(buf.Data, make([]int, chunkSize)...)
			}
		}
		if smallRead {
			// a single Read can return fewer bytes than requested even
			// when more data is available, keep reading.
			continue
		}
	}
	buf.Data = buf.Data[:i]

	if err == io.EOF {
		err = nil
	}

	return buf, err
}

// PCMBuffer populates the passed PCM buffer and returns the number of samples
// read and a potential error. If the reader reaches EOF, an io.EOF error will be returned.
func (d *Decoder) PCMBuffer(buf *audio.IntBuffer) (n int, err error) {
	if buf == nil {
		return 0, nil
	}

	if !d.WasPCMAccessed() {
		err = d.FwdToPCM()
		if err != nil {
			return 0, err
		}
	}

	// TODO: avoid a potentially unecessary allocation
	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	buf.SourceBitDepth = d.decodedBitDepth()
	decodeF, _, err := d.decodeFunc()
	if err != nil {
		return 0, fmt.Errorf("could not get sample decode func %v", err)
	}

	bPerSample := d.encodedBytesPerSample()
	// populate a file buffer to avoid multiple very small reads
	// we need to cap the buffer size to not be bigger than the pcm chunk.
	size := len(buf.Data) * bPerSample
	remaining := int(d.PCMSize) - d.pcmBytesRead()
	if remaining < 0 {
		remaining = 0
	}
	if size > remaining {
		size = remaining
	}
	if size < 1 {
		return 0, nil
	}
	tmpBuf := make([]byte, size)
	var m int
	m, err = d.pcmReader().Read(tmpBuf)
	if err != nil {
		if err == io.EOF {
			return m, nil
		}
		return m, err
	}
	if m == 0 {
		return m, nil
	}
	bufR := bytes.NewReader(tmpBuf[:m])
	sampleBuf := make([]byte, bPerSample, bPerSample)
	var misaligned bool
	if m%bPerSample > 0 {
		misaligned = true
	}

	// Note that we populate the buffer even if the
	// size of the buffer doesn't fit an even number of frames.
	for n = 0; n < len(buf.Data); n++ {
		buf.Data[n], err = decodeF(bufR, sampleBuf)
		if err != nil {
			// the last sample isn't a full sample but just padding.
			if misaligned {
				n--
			}
			break
		}
	}
	buf.Format = format
	if err == io.EOF {
		err = nil
	}

	return n, err
}

// String implements the Stringer interface.
func (d *Decoder) String() string {
	out := fmt.Sprintf("Format: %s - ", d.Form)
	if d.Form == aifcID {
		out += fmt.Sprintf("%s - ", string(d.Encoding[:]))
	}
	if d.SampleRate != 0 {
		out += fmt.Sprintf("%d channels @ %d / %d bits - ", d.NumChans, d.SampleRate, d.BitDepth)
		if isULawEncoding(d.Encoding) {
			out += "(G.711 μ-law, decodes to 16 bits) - "
		}
		dur, _ := d.Duration()
		out += fmt.Sprintf("Duration: %f seconds\n", dur.Seconds())
	}
	if len(d.Comments) > 0 {
		for _, comment := range d.Comments {
			out += fmt.Sprintln(comment)
		}
	}
	if d.HasAppleInfo {
		out += fmt.Sprintln("Key note:", AppleNoteToPitch(d.AppleInfo.Note))
		out += fmt.Sprintln("Scale:", AppleScaleToString(d.AppleInfo.Scale))
		out += fmt.Sprintf("Tempo: %.2f BPM\n", d.Tempo())
		out += fmt.Sprintf("Number of beats: %d\n", d.AppleInfo.Beats)
		out += fmt.Sprintf("Time signature: %d/%d\n", d.AppleInfo.Numerator, d.AppleInfo.Denominator)
		var format string
		if d.AppleInfo.IsLooping {
			format = "loop"
		} else {
			format = "one-shot"
		}
		out += fmt.Sprintln("Sample format:", format)
		if len(d.AppleInfo.Tags) > 0 {
			out += "Tags:\n"
			for _, tag := range d.AppleInfo.Tags {
				out += fmt.Sprintln("\t" + tag)
			}
		}
	}
	return out
}

// iDnSize returns the next ID + block size
func (d *Decoder) iDnSize() ([4]byte, uint32, error) {
	var ID [4]byte
	var blockSize uint32
	if d.err = binary.Read(d.r, binary.BigEndian, &ID); d.err != nil {
		return ID, blockSize, d.err
	}
	if d.err = binary.Read(d.r, binary.BigEndian, &blockSize); d.err != nil {
		return ID, blockSize, d.err
	}
	return ID, blockSize, nil
}

// readHeaders is safe to call multiple times
// byte size of the header: 12
func (d *Decoder) readHeaders() error {
	// prevent the headers to be re-read
	if d.Size > 0 {
		return nil
	}
	var n int64
	size := 12 // 4 + 4 + 4
	src := bytes.NewBuffer(make([]byte, 0, size))
	n, d.err = io.CopyN(src, d.r, int64(size))
	if n < int64(size) {
		src.Truncate(int(n))
	}

	if d.err = binary.Read(src, binary.BigEndian, &d.ID); d.err != nil {
		return d.err
	}
	// Must start by a FORM header/ID
	if d.ID != formID {
		d.err = fmt.Errorf("%s - %#v", ErrFmtNotSupported, d.ID)
		return d.err
	}

	if d.err = binary.Read(src, binary.BigEndian, &d.Size); d.err != nil {
		return d.err
	}
	if d.err = binary.Read(src, binary.BigEndian, &d.Form); d.err != nil {
		return d.err
	}

	// Must be a AIFF or AIFC form type
	if d.Form != aiffID && d.Form != aifcID {
		d.err = fmt.Errorf("%s - %#v", ErrFmtNotSupported, d.Form)
		return d.err
	}

	return d.err
}

// ReadInfo reads the underlying reader to extract information.
// This method is safe to call multiple times.
func (d *Decoder) ReadInfo() {
	if d == nil || d.SampleRate > 0 {
		return
	}
	if d.err = d.readHeaders(); d.err != nil {
		d.err = fmt.Errorf("failed to read header - %w", d.err)
		return
	}

	var (
		id          [4]byte
		size        uint32
		rewindBytes int64
	)
	for d.err != io.EOF {
		id, size, d.err = d.iDnSize()
		if d.err != nil {
			d.err = fmt.Errorf("error reading chunk header - %v", d.err)
			break
		}
		switch id {
		case COMMID:
			d.parseCommChunk(size)
			// if we found other chunks before the COMM,
			// we need to rewind the reader so we can properly
			// read the rest later.
			if rewindBytes > 0 {
				// we need to rewind rewindBytes+size of chunk ID and size
				d.r.Seek(-(rewindBytes + int64(size) + 8), io.SeekCurrent)
				rewindBytes = 0
			}
			return
		case COMTID:
			chunk := &Chunk{
				ID:   id,
				Size: int(size),
				R:    io.LimitReader(d.r, int64(size)),
			}
			if err := d.parseCommentsChunk(chunk); err != nil {
				fmt.Fprintf(os.Stderr, "failed to read comments (ignored) - %v", err)
			}
			// skip the pad byte that keeps odd sized chunks even.
			if _, err := d.r.Seek(int64(size%2), io.SeekCurrent); err != nil {
				return
			}
		default:
			// we haven't read the COMM chunk yet, we need to track location to rewind
			if d.SampleRate == 0 {
				// we add 8 for the ID and size of this chunk and, when
				// needed, the pad byte that keeps the chunk even sized.
				rewindBytes += int64(size) + 8 + int64(size%2)
			}
			if d.err = d.jumpTo(int(size) + int(size%2)); d.err != nil {
				return
			}
		}
	}
}

func (d *Decoder) parseCommChunk(size uint32) error {
	// don't re-parse the comm chunk
	if d.NumChans > 0 {
		return nil
	}
	if size < 18 {
		d.err = fmt.Errorf("%w: COMM chunk of %d bytes, at least 18 are required", ErrUnexpectedData, size)
		return d.err
	}
	if size > 1<<16 {
		d.err = fmt.Errorf("%w: COMM chunk size %d is unreasonable", ErrUnexpectedData, size)
		return d.err
	}
	d.commSize = size

	var n int64
	src := bytes.NewBuffer(make([]byte, 0, size))
	n, d.err = io.CopyN(src, d.r, int64(size))
	if n < int64(size) {
		src.Truncate(int(n))
	}
	if d.err != nil && d.err != io.EOF {
		return d.err
	}
	if n < 18 {
		d.err = fmt.Errorf("%w: COMM chunk holds %d bytes, at least 18 are required", ErrUnexpectedData, n)
		return d.err
	}
	d.err = nil

	if d.err = binary.Read(src, binary.BigEndian, &d.NumChans); d.err != nil {
		d.err = fmt.Errorf("num of channels failed to parse - %s", d.err)
		return d.err
	}
	if d.NumChans < 1 {
		d.err = fmt.Errorf("%w: invalid number of channels %d", ErrInvalidParams, d.NumChans)
		return d.err
	}
	if d.err = binary.Read(src, binary.BigEndian, &d.NumSampleFrames); d.err != nil {
		d.err = fmt.Errorf("num of sample frames failed to parse - %s", d.err)
		return d.err
	}
	if d.err = binary.Read(src, binary.BigEndian, &d.BitDepth); d.err != nil {
		d.err = fmt.Errorf("sample size failed to parse - %s", d.err)
		return d.err
	}
	var srBytes [10]byte
	if d.err = binary.Read(src, binary.BigEndian, &srBytes); d.err != nil {
		d.err = fmt.Errorf("sample rate failed to parse - %s", d.err)
		return d.err
	}
	d.SampleRate = audio.IEEEFloatToInt(srBytes)
	if d.SampleRate <= 0 {
		d.err = fmt.Errorf("%w: invalid sample rate %d", ErrInvalidParams, d.SampleRate)
		return d.err
	}

	read := 18

	if d.Form == aifcID {
		if size < 22 {
			d.err = fmt.Errorf("%w: AIFC COMM chunk of %d bytes doesn't hold a compression type", ErrUnexpectedData, size)
			return d.err
		}
		if d.err = binary.Read(src, binary.BigEndian, &d.Encoding); d.err != nil {
			d.err = fmt.Errorf("AIFC encoding failed to parse - %s", d.err)
			return d.err
		}
		if isULawEncoding(d.Encoding) {
			// μ-law decodes to 16 bit linear PCM but stores 8 bit code
			// words. Some files incorrectly declare 16 bit.
			if d.BitDepth != 8 && d.BitDepth != 16 {
				d.err = fmt.Errorf("%w: %q requires 8 bit samples, got %d", ErrInvalidParams, d.Encoding, d.BitDepth)
				return d.err
			}
		} else if d.Encoding == encSowt {
			d.byteOrder = binary.LittleEndian
		}
		// Pascal style string with the description of the encoding. It is
		// optional: some encoders stop right after the compression type,
		// while others store a length byte without any name.
		read = 22
		if size > 22 {
			var encNameSize uint8
			if d.err = binary.Read(src, binary.BigEndian, &encNameSize); d.err != nil {
				if d.err == io.EOF {
					d.err = nil
				} else {
					d.err = fmt.Errorf("AIFC encoding name failed to parse - %s", d.err)
					return d.err
				}
			} else {
				read = 23
				if int(encNameSize) > int(size)-23 {
					d.err = fmt.Errorf("%w: AIFC compression name of %d bytes exceeds the COMM chunk", ErrUnexpectedData, encNameSize)
					return d.err
				}
				if encNameSize > 0 {
					desc := make([]byte, encNameSize)
					if _, err := io.ReadFull(src, desc); err != nil {
						d.err = fmt.Errorf("AIFC encoding name failed to parse - %s", err)
						return d.err
					}
					// the trailing pad byte isn't part of the name.
					d.EncodingName = string(desc)
					read += int(encNameSize)
				}
			}
		}
	}
	if read < int(size) {
		io.CopyN(ioutil.Discard, src, int64(int(size)-read))
	}

	return d.err
}

// jumpTo advances the reader to the amount of bytes provided
func (d *Decoder) jumpTo(bytesAhead int) error {
	var err error
	if bytesAhead > 0 {
		_, err = d.r.Seek(int64(bytesAhead), io.SeekCurrent)
		// TODO: benchmark against
		// _, err = io.CopyN(ioutil.Discard, d.r, int64(bytesAhead))
	}
	return err
}

func bytesPerSample(bitDepth int) int {
	return bitDepth / 8
}

// encodedBytesPerSample returns the number of bytes used by a single stored
// sample in the PCM chunk. G.711 μ-law stores one byte per sample. It returns
// 0 for bit depths that aren't a whole number of bytes since such samples
// can't be decoded.
func (d *Decoder) encodedBytesPerSample() int {
	if isULawEncoding(d.Encoding) {
		return 1
	}
	bitDepth := int(d.BitDepth)
	if bitDepth < 8 || bitDepth%8 != 0 {
		return 0
	}
	return bitDepth / 8
}

// decodedBitDepth returns the bit depth of the linear PCM samples returned by
// the decoder. Compressed encodings decode to 16 bit linear PCM.
func (d *Decoder) decodedBitDepth() int {
	if isULawEncoding(d.Encoding) {
		return 16
	}
	return int(d.BitDepth)
}

// validatePCMFrames checks that the number of sample frames declared by the
// COMM chunk matches the amount of audio data stored in the SSND chunk. Some
// encoders (Python's aifc module for instance) count the pad byte that keeps
// the SSND chunk even sized in the declared size; such a trailing byte is
// trimmed from PCMSize.
func (d *Decoder) validatePCMFrames() error {
	expected, err := d.expectedPCMSize()
	if err != nil {
		return err
	}
	actual := uint64(d.PCMSize)
	if expected == actual {
		return nil
	}
	// Tolerate a single pad byte counted in the declared chunk size: it can
	// only be a pad byte when the sample data itself is odd sized.
	if expected%2 == 1 && actual == expected+1 {
		d.PCMSize = uint32(expected)
		return nil
	}
	return fmt.Errorf("%w: COMM declares %d frame(s) of %d channel(s) x %d byte(s) = %d byte(s) but SSND holds %d byte(s)",
		ErrPCMFrameMismatch, d.NumSampleFrames, d.NumChans, d.encodedBytesPerSample(), expected, actual)
}

// expectedPCMSize returns the number of sample bytes declared by the COMM
// chunk.
func (d *Decoder) expectedPCMSize() (uint64, error) {
	if d.Form == aifcID && !supportedAIFCEncoding(d.Encoding) {
		return 0, fmt.Errorf("%w: AIFC compression type %q", ErrFmtNotSupported, d.Encoding)
	}
	if d.NumChans < 1 {
		return 0, fmt.Errorf("%w: invalid number of channels %d", ErrInvalidParams, d.NumChans)
	}
	bPerSample := d.encodedBytesPerSample()
	if bPerSample < 1 {
		return 0, fmt.Errorf("%w: unsupported bit depth %d", ErrInvalidParams, d.BitDepth)
	}
	return uint64(d.NumSampleFrames) * uint64(d.NumChans) * uint64(bPerSample), nil
}

// checkPCMFrames checks the declared number of sample frames against the
// given number of sample bytes, tolerating a pad byte counted in the chunk
// size.
func (d *Decoder) checkPCMFrames(actual uint64) error {
	if d.NumChans < 1 || d.BitDepth < 1 {
		return nil
	}
	expected, err := d.expectedPCMSize()
	if err != nil {
		return err
	}
	if expected == actual || (expected%2 == 1 && actual == expected+1) {
		return nil
	}
	return fmt.Errorf("%w: COMM declares %d frame(s) of %d channel(s) x %d byte(s) = %d byte(s) but SSND holds %d byte(s)",
		ErrPCMFrameMismatch, d.NumSampleFrames, d.NumChans, d.encodedBytesPerSample(), expected, actual)
}

// supportedAIFCEncoding reports whether the decoder knows how to turn the
// given AIFC compression type into linear PCM data.
func supportedAIFCEncoding(enc [4]byte) bool {
	switch enc {
	case encNotSet, encNone, encSowt, encTwos, encRaw, encAble:
		return true
	case envUlaw, encULAW:
		return true
	default:
		return false
	}
}

// decodeFunc returns a function decoding one stored sample along with the
// number of bytes consumed by that sample.
func (d *Decoder) decodeFunc() (func(io.Reader, []byte) (int, error), int, error) {
	if d.Form == aifcID && !supportedAIFCEncoding(d.Encoding) {
		return nil, 0, fmt.Errorf("%w: AIFC compression type %q", ErrFmtNotSupported, d.Encoding)
	}
	if isULawEncoding(d.Encoding) {
		return func(r io.Reader, buf []byte) (int, error) {
			if _, err := io.ReadFull(r, buf[:1]); err != nil {
				return 0, err
			}
			return ulawDecodeSample(buf[0]), nil
		}, 1, nil
	}
	f, err := sampleDecodeFunc(int(d.BitDepth), d.byteOrder)
	return f, bytesPerSample(int(d.BitDepth)), err
}

// pcmBytesRead returns the number of sample bytes already consumed from the
// SSND chunk. The chunk position also accounts for the SSND header (offset and
// block size fields) and any skipped offsetted data.
func (d *Decoder) pcmBytesRead() int {
	if d.PCMChunk == nil {
		return 0
	}
	read := d.PCMChunk.Pos - 8 - int(d.SSNDOffset)
	if read < 0 {
		return 0
	}
	if uint32(read) > d.PCMSize {
		return int(d.PCMSize)
	}
	return read
}

// pcmReader returns a reader limited to the remaining sample bytes of the PCM
// chunk, excluding the chunk header and the optional pad byte.
func (d *Decoder) pcmReader() io.Reader {
	remaining := int64(d.PCMSize) - int64(d.pcmBytesRead())
	if remaining < 0 {
		remaining = 0
	}
	return io.LimitReader(d.PCMChunk, remaining)
}

// remainingBytes returns the number of bytes left in the underlying reader
// (restoring its position) or an error if the reader can't be seeked.
func (d *Decoder) remainingBytes() (int64, error) {
	if d.r == nil {
		return 0, errors.New("nil reader")
	}
	pos, err := d.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := d.r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := d.r.Seek(pos, io.SeekStart); err != nil {
		return 0, err
	}
	return end - pos, nil
}

func sampleDecodeFunc(bitDepth int, byteOrder binary.ByteOrder) (func(io.Reader, []byte) (int, error), error) {
	switch bitDepth {
	case 8:
		// 8bit values are unsigned
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:1])
			return int(buf[0]), err
		}, nil
	case 16:
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:2])
			return int(int16(byteOrder.Uint16(buf[:2]))), err
		}, nil
	case 24:
		if byteOrder == binary.BigEndian {
			return func(r io.Reader, buf []byte) (int, error) {
				_, err := r.Read(buf[:3])
				if err != nil {
					return 0, err
				}
				return int(audio.Int24BETo32(buf[:3])), nil
			}, nil
		}
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:3])
			if err != nil {
				return 0, err
			}
			return int(audio.Int24LETo32(buf[:3])), nil
		}, nil
	case 32:
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:4])
			return int(int32(byteOrder.Uint32(buf[:4]))), err
		}, nil
	default:
		return nil, fmt.Errorf("%v bit depth not supported", bitDepth)
	}
}

func sampleFloat64DecodeFunc(bitDepth int, byteOrder binary.ByteOrder) (func(io.Reader) (float64, error), error) {
	switch bitDepth {
	case 8:
		// 8bit values are unsigned
		return func(r io.Reader) (float64, error) {
			var v uint8
			err := binary.Read(r, byteOrder, &v)
			return float64(v), err
		}, nil
	case 16:
		return func(r io.Reader) (float64, error) {
			var v int16
			err := binary.Read(r, byteOrder, &v)
			return float64(v), err
		}, nil
	case 24:
		return func(r io.Reader) (float64, error) {
			// TODO: check if the conversion might not be inversed depending on
			// the encoding (BE vs LE)
			var output int32
			d := make([]byte, 3)
			_, err := r.Read(d)
			if err != nil {
				return 0, err
			}
			output |= int32(d[2]) << 0
			output |= int32(d[1]) << 8
			output |= int32(d[0]) << 16
			return float64(output), nil
		}, nil
	case 32:
		return func(r io.Reader) (float64, error) {
			var v float32
			err := binary.Read(r, byteOrder, &v)
			return float64(v), err
		}, nil
	default:
		return nil, fmt.Errorf("%v bit depth not supported", bitDepth)
	}
}
