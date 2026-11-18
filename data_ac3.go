package astits

import (
	"errors"
)

// AC3 syncword
// ATSC A/52, section 5.3.1.1
const ac3SyncWord = 0x0b77

// AC3SamplesPerFrame is the number of samples covered by one AC3 frame
// ATSC A/52, section 5.4.1.3
const AC3SamplesPerFrame = 1536

// ac3PTSWrap is the modulus of the 33 bits PTS
const ac3PTSWrap = 1 << 33

// defaultAC3MaxBufferSize is the default maximum number of bytes the extractor
// buffers while waiting for a frame to complete
const defaultAC3MaxBufferSize = 1 << 16

// AC3 channel modes (acmod)
// ATSC A/52, table 5.8
const (
	AC3ChannelModeDualMono = 0 // 1+1: Ch1, Ch2
	AC3ChannelModeMono     = 1 // 1/0: C
	AC3ChannelModeStereo   = 2 // 2/0: L, R
	AC3ChannelMode3F       = 3 // 3/0: L, C, R
	AC3ChannelMode2F1R     = 4 // 2/1: L, R, S
	AC3ChannelMode3F1R     = 5 // 3/1: L, C, R, S
	AC3ChannelMode2F2R     = 6 // 2/2: L, R, SL, SR
	AC3ChannelMode3F2R     = 7 // 3/2: L, C, R, SL, SR
)

// AC3 errors
var (
	errAC3InvalidSyncword = errors.New("astits: invalid AC3 syncword")
	errAC3NotEnoughData   = errors.New("astits: not enough AC3 data")
	errAC3ReservedCode    = errors.New("astits: reserved AC3 code")
)

// ac3SampleRates indexes sample rates by fscod
// ATSC A/52, table 5.6
var ac3SampleRates = [3]int{48000, 44100, 32000}

// ac3FrameSizes indexes frame sizes in 16 bits words by frmsizecod then fscod
// ATSC A/52, table 5.13
var ac3FrameSizes = [38][3]int{
	{64, 69, 96},
	{64, 70, 96},
	{80, 87, 120},
	{80, 88, 120},
	{96, 104, 144},
	{96, 105, 144},
	{112, 121, 168},
	{112, 122, 168},
	{128, 139, 192},
	{128, 140, 192},
	{160, 174, 240},
	{160, 175, 240},
	{192, 208, 288},
	{192, 209, 288},
	{224, 243, 336},
	{224, 244, 336},
	{256, 278, 384},
	{256, 279, 384},
	{320, 348, 480},
	{320, 349, 480},
	{384, 417, 576},
	{384, 418, 576},
	{448, 487, 672},
	{448, 488, 672},
	{512, 557, 768},
	{512, 558, 768},
	{576, 626, 864},
	{576, 627, 864},
	{640, 696, 960},
	{640, 697, 960},
	{768, 835, 1152},
	{768, 836, 1152},
	{896, 975, 1344},
	{896, 976, 1344},
	{1024, 1114, 1536},
	{1024, 1115, 1536},
	{1152, 1253, 1728},
	{1152, 1254, 1728},
}

// AC3Frame represents an AC3 frame
// https://www.atsc.org/wp-content/uploads/2015/03/A52-201212-17.pdf
type AC3Frame struct {
	ChannelMode uint8 // acmod, see AC3ChannelMode* constants
	Data        []byte
	FrameSize   int // Size of the frame in bytes, syncword included
	PTS         *ClockReference
	SampleRate  int
}

// ac3FrameHeader represents the parsed fields of an AC3 syncframe header
type ac3FrameHeader struct {
	channelMode uint8
	frameSize   int
	sampleRate  int
}

// parseAC3FrameHeader parses an AC3 syncframe header
// b must start at the syncword and hold at least the 7 first bytes of the frame
func parseAC3FrameHeader(b []byte) (h ac3FrameHeader, err error) {
	// Not enough data
	if len(b) < 7 {
		err = errAC3NotEnoughData
		return
	}

	// Syncword
	if uint16(b[0])<<8|uint16(b[1]) != ac3SyncWord {
		err = errAC3InvalidSyncword
		return
	}

	// CRC1 is skipped: b[2], b[3]

	// Sample rate code
	fscod := b[4] >> 6
	if fscod == 3 {
		err = errAC3ReservedCode
		return
	}
	h.sampleRate = ac3SampleRates[fscod]

	// Frame size code
	frmsizecod := b[4] & 0x3f
	if frmsizecod > 37 {
		err = errAC3ReservedCode
		return
	}
	h.frameSize = 2 * ac3FrameSizes[frmsizecod][fscod]

	// Bit stream identification: values above 10 are reserved (E-AC3 uses 11 to 16)
	if bsid := b[5] >> 3; bsid > 10 {
		err = errAC3ReservedCode
		return
	}

	// Bit stream mode is skipped: b[5] & 0x7

	// Channel mode
	h.channelMode = b[6] >> 5
	return
}

// ac3QueuedPTS represents a PES PTS waiting for the frame it applies to
type ac3QueuedPTS struct {
	offset int64 // Absolute offset of the first byte of the PES payload carrying this PTS
	pts    int64 // 90 kHz base
}

// AC3FramesExtractor extracts complete AC3 frames from the payloads of
// consecutive PES packets of an AC3 elementary stream (e.g. PMT stream type
// StreamTypeAC3Audio). Frames spanning over several PES packets are stitched
// back together. Other PIDs and the rest of the TS parsing are unaffected:
// the caller simply feeds the PES data of the AC3 PID to the extractor.
//
// PTS handling: the PTS of a PES packet is assigned to the first frame
// starting in that packet. The PTS of the following frames is derived by
// adding AC3SamplesPerFrame samples per frame, wrapping on 33 bits.
//
// Error handling:
//   - invalid syncword or reserved codes (fscod == 3, frmsizecod > 37,
//     bsid > 10): bytes are skipped until the next valid syncword
//   - truncated frame: bytes are buffered until the frame completes
//   - stream interruption: call Flush to drop the buffered partial frame
//   - buffer growing past the max buffer size: buffered data is dropped up to
//     the last syncword and PTS state is reset
type AC3FramesExtractor struct {
	buf           []byte
	hasPTS        bool
	maxBufferSize int
	offset        int64 // Absolute offset of buf[0]
	ptsBase       int64
	ptsFrames     int64 // Number of frames emitted since ptsBase
	ptsQueue      []ac3QueuedPTS
	ptsSampleRate int
}

// NewAC3FramesExtractor creates a new AC3 frames extractor. maxBufferSize caps
// the number of buffered bytes waiting for a frame to complete; a value <= 0
// uses a sane default. It must be bigger than the biggest expected frame.
func NewAC3FramesExtractor(maxBufferSize int) *AC3FramesExtractor {
	if maxBufferSize <= 0 {
		maxBufferSize = defaultAC3MaxBufferSize
	}
	return &AC3FramesExtractor{
		maxBufferSize: maxBufferSize,
	}
}

// Extract processes a PES payload and returns the frames it completes
func (e *AC3FramesExtractor) Extract(d *PESData) (frames []*AC3Frame) {
	// Nothing to do
	if d == nil || len(d.Data) == 0 {
		return
	}

	// Queue PTS: it applies to the first frame starting in this payload
	if d.Header != nil && d.Header.OptionalHeader != nil && d.Header.OptionalHeader.PTS != nil {
		e.ptsQueue = append(e.ptsQueue, ac3QueuedPTS{
			offset: e.offset + int64(len(e.buf)),
			pts:    d.Header.OptionalHeader.PTS.Base,
		})
	}

	// Append payload to buffer
	e.buf = append(e.buf, d.Data...)

	// Enforce buffer limit
	if len(e.buf) > e.maxBufferSize {
		e.resync()
	}

	// Extract frames
	for {
		// Look for syncword
		idx := ac3SyncwordIndex(e.buf)
		if idx < 0 {
			// No syncword: drop everything but a trailing potential syncword start
			if l := len(e.buf); l > 0 && e.buf[l-1] == byte(ac3SyncWord>>8) {
				e.drop(l - 1)
			} else {
				e.drop(l)
			}
			return
		}
		e.drop(idx)

		// Parse header
		h, err := parseAC3FrameHeader(e.buf)
		if err == errAC3NotEnoughData {
			// Wait for more data
			return
		} else if err != nil {
			// Invalid header: skip the syncword and resync
			e.drop(2)
			continue
		}

		// Frame is truncated: wait for more data
		if len(e.buf) < h.frameSize {
			return
		}

		// Build frame
		f := &AC3Frame{
			ChannelMode: h.channelMode,
			Data:        make([]byte, h.frameSize),
			FrameSize:   h.frameSize,
			PTS:         e.framePTS(h.sampleRate),
			SampleRate:  h.sampleRate,
		}
		copy(f.Data, e.buf[:h.frameSize])
		frames = append(frames, f)
		e.drop(h.frameSize)
	}
}

// Flush drops the buffered partial frame and resets the PTS state. It should
// be called when the stream is interrupted (e.g. TS discontinuity) since the
// buffered bytes will never complete.
func (e *AC3FramesExtractor) Flush() {
	e.buf = e.buf[:0]
	e.ptsQueue = e.ptsQueue[:0]
	e.hasPTS = false
}

// drop removes the n first bytes from the buffer
func (e *AC3FramesExtractor) drop(n int) {
	if n <= 0 {
		return
	}
	if n >= len(e.buf) {
		e.buf = e.buf[:0]
	} else {
		e.buf = e.buf[n:]
	}
	e.offset += int64(n)
}

// framePTS computes the PTS of a frame starting at the current buffer offset
func (e *AC3FramesExtractor) framePTS(sampleRate int) (pts *ClockReference) {
	// Consume queued PES PTS applying to a frame starting at this offset:
	// the last matching one wins
	for len(e.ptsQueue) > 0 && e.ptsQueue[0].offset <= e.offset {
		e.ptsBase = e.ptsQueue[0].pts
		e.ptsFrames = 0
		e.ptsSampleRate = sampleRate
		e.hasPTS = true
		e.ptsQueue = e.ptsQueue[1:]
	}

	// No PTS reference yet
	if !e.hasPTS {
		return
	}

	// Sample rate changed: re-anchor the PTS at the current frame
	if sampleRate != e.ptsSampleRate {
		e.ptsBase += e.ptsFrames * AC3SamplesPerFrame * 90000 / int64(e.ptsSampleRate)
		e.ptsFrames = 0
		e.ptsSampleRate = sampleRate
	}

	// Each frame advances the clock by AC3SamplesPerFrame samples, PTS wraps on 33 bits
	pts = newClockReference((e.ptsBase+e.ptsFrames*AC3SamplesPerFrame*90000/int64(e.ptsSampleRate))%ac3PTSWrap, 0)
	e.ptsFrames++
	return
}

// resync drops buffered data up to the last syncword and resets the PTS state
func (e *AC3FramesExtractor) resync() {
	if idx := ac3LastSyncwordIndex(e.buf); idx >= 0 {
		e.drop(idx)
	} else if l := len(e.buf); l > 0 && e.buf[l-1] == byte(ac3SyncWord>>8) {
		e.drop(l - 1)
	} else {
		e.drop(l)
	}
	e.ptsQueue = e.ptsQueue[:0]
	e.hasPTS = false
}

// ac3SyncwordIndex returns the index of the first AC3 syncword in b, or -1
func ac3SyncwordIndex(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == byte(ac3SyncWord>>8) && b[i+1] == byte(ac3SyncWord&0xff) {
			return i
		}
	}
	return -1
}

// ac3LastSyncwordIndex returns the index of the last AC3 syncword in b, or -1
func ac3LastSyncwordIndex(b []byte) int {
	for i := len(b) - 2; i >= 0; i-- {
		if b[i] == byte(ac3SyncWord>>8) && b[i+1] == byte(ac3SyncWord&0xff) {
			return i
		}
	}
	return -1
}
