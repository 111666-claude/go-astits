package astits

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ac3TestFrame builds a valid AC3 frame with the given header codes, filled
// with the given byte after the header
func ac3TestFrame(fscod, frmsizecod, bsid, acmod uint8, fill byte) []byte {
	b := make([]byte, 2*ac3FrameSizes[frmsizecod][fscod])
	b[0] = 0x0b
	b[1] = 0x77
	b[2] = 0x12
	b[3] = 0x34
	b[4] = fscod<<6 | frmsizecod
	b[5] = bsid << 3
	b[6] = acmod << 5
	for i := 7; i < len(b); i++ {
		b[i] = fill
	}
	return b
}

func ac3TestPESData(pts *ClockReference, data ...[]byte) *PESData {
	d := &PESData{Header: &PESHeader{}}
	if pts != nil {
		d.Header.OptionalHeader = &PESOptionalHeader{PTS: pts}
	}
	for _, b := range data {
		d.Data = append(d.Data, b...)
	}
	return d
}

func TestParseAC3FrameHeader(t *testing.T) {
	// Valid header: 48 kHz, frmsizecod 4 => 96 words => 192 bytes, stereo
	h, err := parseAC3FrameHeader(ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0))
	require.NoError(t, err)
	assert.Equal(t, 192, h.frameSize)
	assert.Equal(t, 48000, h.sampleRate)
	assert.Equal(t, uint8(AC3ChannelModeStereo), h.channelMode)

	// Not enough data
	_, err = parseAC3FrameHeader([]byte{0x0b, 0x77, 0, 0, 0})
	assert.Equal(t, errAC3NotEnoughData, err)

	// Invalid syncword
	f := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0)
	f[1] = 0x78
	_, err = parseAC3FrameHeader(f)
	assert.Equal(t, errAC3InvalidSyncword, err)

	// Reserved sample rate code
	f = ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0)
	f[4] |= 0xc0
	_, err = parseAC3FrameHeader(f)
	assert.Equal(t, errAC3ReservedCode, err)

	// Reserved frame size code
	f = ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0)
	f[4] = f[4]&0xc0 | 38
	_, err = parseAC3FrameHeader(f)
	assert.Equal(t, errAC3ReservedCode, err)

	// Reserved bit stream identification (E-AC3)
	f = ac3TestFrame(0, 4, 16, AC3ChannelModeStereo, 0)
	_, err = parseAC3FrameHeader(f)
	assert.Equal(t, errAC3ReservedCode, err)
}

// TestAC3FramesExtractor checks frame extraction over two PES packets holding
// three frames, the second one spanning the PES boundary
func TestAC3FramesExtractor(t *testing.T) {
	// Build 3 frames of 192 bytes each (48 kHz, stereo)
	f1 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x11)
	f2 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x22)
	f3 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x33)

	e := NewAC3FramesExtractor(0)

	// First PES: PTS + frame 1 + first 100 bytes of frame 2
	frames := e.Extract(ac3TestPESData(newClockReference(90000, 0), f1, f2[:100]))
	require.Len(t, frames, 1)
	assert.Equal(t, f1, frames[0].Data)
	assert.Equal(t, 192, frames[0].FrameSize)
	assert.Equal(t, 48000, frames[0].SampleRate)
	assert.Equal(t, uint8(AC3ChannelModeStereo), frames[0].ChannelMode)
	require.NotNil(t, frames[0].PTS)
	assert.Equal(t, int64(90000), frames[0].PTS.Base)

	// Second PES: no PTS + rest of frame 2 + frame 3
	frames = e.Extract(ac3TestPESData(nil, f2[100:], f3))
	require.Len(t, frames, 2)
	assert.Equal(t, f2, frames[0].Data)
	assert.Equal(t, f3, frames[1].Data)

	// PTS is derived by adding 1536 samples per frame: 1536 * 90000 / 48000 = 2880
	require.NotNil(t, frames[0].PTS)
	assert.Equal(t, int64(90000+2880), frames[0].PTS.Base)
	require.NotNil(t, frames[1].PTS)
	assert.Equal(t, int64(90000+2*2880), frames[1].PTS.Base)
}

func TestAC3FramesExtractorPTSWrap(t *testing.T) {
	f1 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x11)
	f2 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x22)
	f3 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x33)

	e := NewAC3FramesExtractor(0)

	// PTS one frame before the 33 bits wrap
	frames := e.Extract(ac3TestPESData(newClockReference(ac3PTSWrap-2880, 0), f1, f2, f3))
	require.Len(t, frames, 3)
	assert.Equal(t, int64(ac3PTSWrap-2880), frames[0].PTS.Base)
	assert.Equal(t, int64(0), frames[1].PTS.Base)
	assert.Equal(t, int64(2880), frames[2].PTS.Base)
}

func TestAC3FramesExtractorNoPTS(t *testing.T) {
	e := NewAC3FramesExtractor(0)
	frames := e.Extract(ac3TestPESData(nil, ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x11)))
	require.Len(t, frames, 1)
	assert.Nil(t, frames[0].PTS)
}

func TestAC3FramesExtractorResync(t *testing.T) {
	valid := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x42)

	// Invalid frame: reserved frame size code
	invalid := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x99)
	invalid[4] = 38

	e := NewAC3FramesExtractor(0)

	// Garbage, then an invalid frame, then a valid one
	frames := e.Extract(ac3TestPESData(nil, []byte{0x00, 0x01, 0x0b}, invalid, valid))
	require.Len(t, frames, 1)
	assert.Equal(t, valid, frames[0].Data)
}

func TestAC3FramesExtractorTruncated(t *testing.T) {
	f := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x42)

	e := NewAC3FramesExtractor(0)

	// Feed the frame byte per byte: only the last chunk completes it
	for i := 0; i < len(f); i++ {
		frames := e.Extract(ac3TestPESData(nil, f[i:i+1]))
		if i < len(f)-1 {
			assert.Empty(t, frames)
		} else {
			require.Len(t, frames, 1)
			assert.Equal(t, f, frames[0].Data)
		}
	}
}

func TestAC3FramesExtractorFlush(t *testing.T) {
	f1 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x11)
	f2 := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x22)

	e := NewAC3FramesExtractor(0)

	// Partial frame, then stream interruption
	assert.Empty(t, e.Extract(ac3TestPESData(nil, f1[:100])))
	e.Flush()

	// The next frame must be extracted from scratch, without the stale bytes
	frames := e.Extract(ac3TestPESData(nil, f2))
	require.Len(t, frames, 1)
	assert.Equal(t, f2, frames[0].Data)
}

func TestAC3FramesExtractorBufferOverflow(t *testing.T) {
	valid := ac3TestFrame(0, 4, 8, AC3ChannelModeStereo, 0x42)

	e := NewAC3FramesExtractor(64)

	// Garbage overflowing the buffer is dropped
	assert.Empty(t, e.Extract(ac3TestPESData(nil, make([]byte, 128))))

	// Garbage followed by a valid frame: garbage is dropped up to the syncword
	frames := e.Extract(ac3TestPESData(nil, make([]byte, 100), valid))
	require.Len(t, frames, 1)
	assert.Equal(t, valid, frames[0].Data)

	// A frame bigger than the buffer is still extracted once complete: the cap
	// only trims data that cannot belong to the frame being assembled
	big := ac3TestFrame(0, 16, 8, AC3ChannelModeStereo, 0x00) // 512 bytes > 64 bytes buffer
	assert.Empty(t, e.Extract(ac3TestPESData(nil, big[:100])))
	frames = e.Extract(ac3TestPESData(nil, big[100:]))
	require.Len(t, frames, 1)
	assert.Equal(t, big, frames[0].Data)
}

func TestAC3FramesExtractorNilData(t *testing.T) {
	e := NewAC3FramesExtractor(0)
	assert.Empty(t, e.Extract(nil))
	assert.Empty(t, e.Extract(&PESData{}))
}
