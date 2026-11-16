package astits

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func patExpectedBytes(versionNumber uint8, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(PIDPAT, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint16(0))       // Table ID
	w.Write("1011")          // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(13), 12) // Section length

	w.Write(uint16(PSITableIDPAT))
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write(programNumberStart)
	w.Write("111") // reserved
	w.WriteN(pmtStartPID, 13)

	// CRC32
	if versionNumber == 0 {
		w.Write([]byte{0x71, 0x10, 0xd8, 0x78})
	} else {
		w.Write([]byte{0xef, 0xbe, 0x08, 0x5a})
	}

	w.Write(bytes.Repeat([]byte{0xff}, 167))

	return buf.Bytes()
}

func TestMuxer_generatePAT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)

	err := muxer.generatePAT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.patBytes.Len())
	assert.Equal(t, patExpectedBytes(0, 0), muxer.patBytes.Bytes())

	// Version number shouldn't change
	err = muxer.generatePAT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.patBytes.Len())
	assert.Equal(t, patExpectedBytes(0, 1), muxer.patBytes.Bytes())

	// Version number should change
	muxer.pmUpdated = true
	err = muxer.generatePAT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.patBytes.Len())
	assert.Equal(t, patExpectedBytes(1, 2), muxer.patBytes.Bytes())
}

func pmtExpectedBytesVideoOnly(versionNumber, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(pmtStartPID, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint16(PSITableIDPMT)) // Table ID
	w.Write("1011")                // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(18), 12)       // Section length

	w.Write(programNumberStart)
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write("111")               // reserved
	w.WriteN(uint16(0x1234), 13) // PCR PID

	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // program info length

	w.Write(uint8(StreamTypeH264Video))
	w.Write("111") // reserved
	w.WriteN(uint16(0x1234), 13)

	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // es info length

	w.Write([]byte{0x31, 0x48, 0x5b, 0xa2}) // CRC32

	w.Write(bytes.Repeat([]byte{0xff}, 162))

	return buf.Bytes()
}

func pmtExpectedBytesVideoAndAudio(versionNumber uint8, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(pmtStartPID, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint16(PSITableIDPMT)) // Table ID
	w.Write("1011")                // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(23), 12)       // Section length

	w.Write(programNumberStart)
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write("111")               // reserved
	w.WriteN(uint16(0x1234), 13) // PCR PID

	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // program info length

	w.Write(uint8(StreamTypeH264Video))
	w.Write("111") // reserved
	w.WriteN(uint16(0x1234), 13)
	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // es info length

	w.Write(uint8(StreamTypeADTS))
	w.Write("111") // reserved
	w.WriteN(uint16(0x0234), 13)
	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // es info length

	// CRC32
	if versionNumber == 0 {
		w.Write([]byte{0x29, 0x52, 0xc4, 0x50})
	} else {
		w.Write([]byte{0x06, 0xf4, 0xa6, 0xea})
	}

	w.Write(bytes.Repeat([]byte{0xff}, 157))

	return buf.Bytes()
}

func TestMuxer_generatePMT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	muxer.SetPCRPID(0x1234)
	assert.NoError(t, err)

	err = muxer.generatePMT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.pmtBytes.Len())
	assert.Equal(t, pmtExpectedBytesVideoOnly(0, 0), muxer.pmtBytes.Bytes())

	// Version number shouldn't change
	err = muxer.generatePMT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.pmtBytes.Len())
	assert.Equal(t, pmtExpectedBytesVideoOnly(0, 1), muxer.pmtBytes.Bytes())

	err = muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x0234,
		StreamType:    StreamTypeAACAudio,
	})
	assert.NoError(t, err)

	// Version number should change
	err = muxer.generatePMT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.pmtBytes.Len())
	assert.Equal(t, pmtExpectedBytesVideoAndAudio(1, 2), muxer.pmtBytes.Bytes())
}

func TestMuxer_WriteTables(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	muxer.SetPCRPID(0x1234)
	assert.NoError(t, err)

	n, err := muxer.WriteTables()
	assert.NoError(t, err)
	assert.Equal(t, 2*MpegTsPacketSize, n)
	assert.Equal(t, n, buf.Len())

	expectedBytes := append(patExpectedBytes(0, 0), pmtExpectedBytesVideoOnly(0, 0)...)
	assert.Equal(t, expectedBytes, buf.Bytes())
}

func TestMuxer_WriteTables_Error(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)

	_, err = muxer.WriteTables()
	assert.Equal(t, ErrPCRPIDInvalid, err)
}

func TestMuxer_AddElementaryStream(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)

	err = muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.Equal(t, ErrPIDAlreadyExists, err)
}

func TestMuxer_RemoveElementaryStream(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)

	err = muxer.RemoveElementaryStream(0x1234)
	assert.NoError(t, err)

	err = muxer.RemoveElementaryStream(0x1234)
	assert.Equal(t, ErrPIDNotFound, err)
}

func testPayload() []byte {
	ret := make([]byte, 0xff+1)
	for i := 0; i <= 0xff; i++ {
		ret[i] = byte(i)
	}
	return ret
}

func TestMuxer_WritePayload(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)

	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	muxer.SetPCRPID(0x1234)
	assert.NoError(t, err)

	err = muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x0234,
		StreamType:    StreamTypeAACAudio,
	})
	assert.NoError(t, err)

	payload := testPayload()
	pcr := ClockReference{
		Base:      5726623061,
		Extension: 341,
	}
	pts := ClockReference{Base: 5726623060}

	n, err := muxer.WriteData(&MuxerData{
		PID: 0x1234,
		AdaptationField: &PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &pcr,
			RandomAccessIndicator: true,
		},
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					DTS:             &pts,
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorBothPresent,
				},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, buf.Len(), n)

	bytesTotal := n

	n, err = muxer.WriteData(&MuxerData{
		PID: 0x0234,
		AdaptationField: &PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &pcr,
			RandomAccessIndicator: true,
		},
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					DTS:             &pts,
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorBothPresent,
				},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, buf.Len(), bytesTotal+n)
	assert.Equal(t, 0, buf.Len()%MpegTsPacketSize)

	bs := buf.Bytes()
	assert.Equal(t, patExpectedBytes(0, 0), bs[:MpegTsPacketSize])
	assert.Equal(t, pmtExpectedBytesVideoAndAudio(0, 0), bs[MpegTsPacketSize:MpegTsPacketSize*2])
}

var (
	eitStartTime    = time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	eitCurrentEvent = EITEvent{
		EventID:   100,
		StartTime: eitStartTime,
		Duration:  90 * time.Minute,
		Language:  "eng",
		Title:     "Current",
	}
	eitNextEvent = EITEvent{
		EventID:   101,
		StartTime: eitStartTime.Add(90 * time.Minute),
		Duration:  45 * time.Minute,
		Language:  "eng",
		Title:     "Next",
	}
)

// eitExpectedBytes builds the expected EIT present/following TS packet without relying on
// the library's writing helpers: event data is laid out by hand so that byte offsets,
// section length and CRC are checked against an independent implementation.
func eitExpectedBytes(versionNumber, cc uint8, currentRunningStatus, nextRunningStatus uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("01") // no transport error, payload start
	w.Write("0")  // no priority
	w.WriteN(PIDEIT, 13)
	w.Write("00") // no scrambling
	w.Write("01") // no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint8(0)) // pointer field

	w.Write(uint8(PSITableIDEITPresentFollowing)) // Table ID
	w.Write("1")                                  // Syntax section indicator
	w.Write("1")                                  // Private bit
	w.Write("11")                                 // Reserved
	w.WriteN(uint16(64), 12)                      // Section length

	w.Write(uint16(programNumberStart)) // Service ID
	w.Write("11")                       // Reserved bits
	w.WriteN(versionNumber, 5)          // Version number
	w.Write("1")                        // Current/next indicator
	w.Write(uint8(0))                   // Section number
	w.Write(uint8(0))                   // Last section number

	w.Write(uint16(0))   // Transport stream ID
	w.Write(uint16(0))   // Original network ID
	w.Write(uint8(0))    // Segment last section number
	w.Write(uint8(0x4e)) // Last table ID

	// Current event
	w.Write(uint16(100))
	w.Write([]byte{0xeb, 0x97, 0x03, 0x04, 0x05}) // Start time: 2024-01-02 03:04:05 UTC
	w.Write([]byte{0x01, 0x30, 0x00})             // Duration: 01:30:00
	w.WriteN(currentRunningStatus, 3)
	w.Write("0") // Free CA mode
	w.WriteN(uint16(14), 12)

	w.Write(uint8(DescriptorTagShortEvent))
	w.Write(uint8(12)) // Descriptor length
	w.Write([]byte("eng"))
	w.Write(uint8(7))
	w.Write([]byte("Current"))
	w.Write(uint8(0)) // Text length

	// Next event
	w.Write(uint16(101))
	w.Write([]byte{0xeb, 0x97, 0x04, 0x34, 0x05}) // Start time: 2024-01-02 04:34:05 UTC
	w.Write([]byte{0x00, 0x45, 0x00})             // Duration: 00:45:00
	w.WriteN(nextRunningStatus, 3)
	w.Write("0") // Free CA mode
	w.WriteN(uint16(11), 12)

	w.Write(uint8(DescriptorTagShortEvent))
	w.Write(uint8(9)) // Descriptor length
	w.Write([]byte("eng"))
	w.Write(uint8(4))
	w.Write([]byte("Next"))
	w.Write(uint8(0)) // Text length

	// CRC32
	if versionNumber == 0 {
		w.Write([]byte{0x89, 0xa9, 0xb4, 0x8c})
	} else {
		w.Write([]byte{0x10, 0x84, 0xe0, 0xec})
	}

	w.Write(bytes.Repeat([]byte{0xff}, 116))

	return buf.Bytes()
}

func TestMuxer_generateEIT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))

	err := muxer.generateEIT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.eitBytes.Len())
	assert.Equal(t, eitExpectedBytes(0, 0, RunningStatusRunning, RunningStatusNotRunning), muxer.eitBytes.Bytes())

	// Version number shouldn't change on retransmission, but CC should
	err = muxer.generateEIT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.eitBytes.Len())
	assert.Equal(t, eitExpectedBytes(0, 1, RunningStatusRunning, RunningStatusNotRunning), muxer.eitBytes.Bytes())

	// Re-setting the same events is idempotent: the version number doesn't change
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))
	err = muxer.generateEIT()
	assert.NoError(t, err)
	assert.Equal(t, eitExpectedBytes(0, 2, RunningStatusRunning, RunningStatusNotRunning), muxer.eitBytes.Bytes())

	// Version number should change after an actual update
	updatedNext := eitNextEvent
	updatedNext.Title = "Next Updated"
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, updatedNext))
	err = muxer.generateEIT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.eitBytes.Len())
	got := muxer.eitBytes.Bytes()
	assert.Equal(t, uint8(1), got[10]>>1&0x1f, "version number should have been bumped")
	assert.Contains(t, string(got), "Next Updated")
}

func TestMuxer_WriteTables_EIT(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))

	n, err := muxer.WriteTables()
	assert.NoError(t, err)
	assert.Equal(t, 3*MpegTsPacketSize, n)
	assert.Equal(t, n, buf.Len())

	expectedBytes := append(patExpectedBytes(0, 0), pmtExpectedBytesVideoOnly(0, 0)...)
	expectedBytes = append(expectedBytes, eitExpectedBytes(0, 0, RunningStatusRunning, RunningStatusNotRunning)...)
	assert.Equal(t, expectedBytes, buf.Bytes())
}

// TestMuxer_EITDisabledByDefault makes sure a muxer that hasn't been configured with EIT keeps
// producing the exact same bytes as before the EIT support was added.
func TestMuxer_EITDisabledByDefault(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf, MuxerOptTablesRetransmitPeriod(2))
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)

	payload := testPayload()
	for i := 0; i < 3; i++ {
		_, err := muxer.WriteData(&MuxerData{
			PID: 0x1234,
			PES: &PESData{Data: payload, Header: &PESHeader{}},
		})
		require.NoError(t, err)
	}

	// Only PAT and PMT are retransmitted, hence no packet is sent on the EIT PID.
	bs := buf.Bytes()
	require.Equal(t, 0, len(bs)%MpegTsPacketSize)
	for i := 0; i < len(bs); i += MpegTsPacketSize {
		assert.NotEqual(t, PIDEIT, uint16(bs[i+1]&0x1f)<<8|uint16(bs[i+2]),
			"no EIT packet expected at offset %d", i)
	}
}

func TestMuxer_WriteTables_NoEITDoesNotChangeOutput(t *testing.T) {
	writeTables := func(withEIT bool) []byte {
		buf := bytes.Buffer{}
		muxer := NewMuxer(context.Background(), &buf)
		require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
			ElementaryPID: 0x1234,
			StreamType:    StreamTypeH264Video,
		}))
		muxer.SetPCRPID(0x1234)
		if withEIT {
			require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))
		}
		_, err := muxer.WriteTables()
		require.NoError(t, err)
		return buf.Bytes()
	}

	withoutEIT := writeTables(false)
	withEIT := writeTables(true)

	// Enabling EIT must not alter PAT or PMT output: the EIT packet is appended after them.
	require.Equal(t, 2*MpegTsPacketSize, len(withoutEIT))
	require.Equal(t, 3*MpegTsPacketSize, len(withEIT))
	assert.Equal(t, withoutEIT, withEIT[:len(withoutEIT)])
	assert.Equal(t, eitExpectedBytes(0, 0, RunningStatusRunning, RunningStatusNotRunning), withEIT[len(withoutEIT):])
}

func TestMuxer_SetEIT_Errors(t *testing.T) {
	longestTitle := strings.Repeat("a", maxEITEventNameLength)

	testCases := []struct {
		name    string
		current EITEvent
		next    EITEvent
		err     error
	}{
		{
			name:    "duplicate event ID",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: eitCurrentEvent.EventID, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "eng"},
			err:     ErrEITDuplicateEventID,
		},
		{
			name:    "next starts before current",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(-time.Second), Duration: time.Hour, Language: "eng"},
			err:     ErrEITEventsOutOfOrder,
		},
		{
			name:    "next starts at the same time as current",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime, Duration: time.Hour, Language: "eng"},
			err:     ErrEITEventsOutOfOrder,
		},
		{
			name:    "zero duration",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Language: "eng"},
			err:     ErrEITEventDurationInvalid,
		},
		{
			name:    "negative duration",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: -time.Minute, Language: "eng"},
			err:     ErrEITEventDurationInvalid,
		},
		{
			name:    "duration longer than BCD field",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: eitMaxDuration + time.Second, Language: "eng"},
			err:     ErrEITEventDurationInvalid,
		},
		{
			name:    "sub-second duration",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour + time.Millisecond, Language: "eng"},
			err:     ErrEITEventDurationInvalid,
		},
		{
			name:    "title too long",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "eng", Title: longestTitle + "a"},
			err:     ErrEITEventTitleTooLong,
		},
		{
			name:    "language too short",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "en"},
			err:     ErrEITEventLanguageInvalid,
		},
		{
			name:    "language too long",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "engg"},
			err:     ErrEITEventLanguageInvalid,
		},
		{
			name:    "language not lowercase ASCII",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "ENG"},
			err:     ErrEITEventLanguageInvalid,
		},
		{
			name:    "zero start time",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, Duration: time.Hour, Language: "eng"},
			err:     ErrEITStartTimeInvalid,
		},
		{
			name:    "start time before MJD range",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: time.Date(1858, time.November, 17, 0, 0, 0, 0, time.UTC), Duration: time.Hour, Language: "eng"},
			err:     ErrEITStartTimeInvalid,
		},
		{
			name:    "start time after MJD range",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: time.Date(2038, time.April, 23, 0, 0, 0, 0, time.UTC), Duration: time.Hour, Language: "eng"},
			err:     ErrEITStartTimeInvalid,
		},
		{
			name:    "sub-second start time",
			current: eitCurrentEvent,
			next:    EITEvent{EventID: 101, StartTime: eitStartTime.Add(time.Hour + time.Nanosecond), Duration: time.Hour, Language: "eng"},
			err:     ErrEITStartTimeInvalid,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			muxer := NewMuxer(context.Background(), nil)
			err := muxer.SetEIT(tc.current, tc.next)
			assert.True(t, errors.Is(err, tc.err), "expected %v, got %v", tc.err, err)
			// Failed attempts must not enable EIT
			assert.False(t, muxer.eitEnabled)
		})
	}
}

func TestMuxer_SetEIT_ErrorsDoNotAffectExistingConfiguration(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))
	require.True(t, muxer.eitEnabled)

	before := muxer.eit
	err := muxer.SetEIT(eitCurrentEvent, EITEvent{
		EventID:   eitCurrentEvent.EventID,
		StartTime: eitStartTime.Add(time.Hour),
		Duration:  time.Hour,
		Language:  "eng",
	})
	assert.True(t, errors.Is(err, ErrEITDuplicateEventID), "expected %v, got %v", ErrEITDuplicateEventID, err)
	assert.Equal(t, before, muxer.eit)
	assert.True(t, muxer.eitEnabled)
}

func TestMuxer_SetEIT_LongestTitleThatFits(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)

	// A title longer than 250 bytes satisfies neither event_name_length nor the descriptor
	// length and must be rejected.
	err := muxer.SetEIT(
		EITEvent{EventID: 1, StartTime: eitStartTime, Duration: time.Hour, Language: "eng", Title: strings.Repeat("a", maxEITEventNameLength+1)},
		EITEvent{EventID: 2, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "eng"},
	)
	assert.True(t, errors.Is(err, ErrEITEventTitleTooLong), "expected %v, got %v", ErrEITEventTitleTooLong, err)
	assert.False(t, muxer.eitEnabled)

	// The maximum title length fits in the 12-bit section length and is accepted.
	err = muxer.SetEIT(
		EITEvent{EventID: 1, StartTime: eitStartTime, Duration: time.Hour, Language: "eng", Title: strings.Repeat("a", maxEITEventNameLength)},
		EITEvent{EventID: 2, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "eng", Title: strings.Repeat("a", maxEITEventNameLength)},
	)
	assert.NoError(t, err)
	assert.True(t, muxer.eitEnabled)
}

func TestMuxer_EITRoundTrip(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf,
		MuxerOptTablesRetransmitPeriod(2),
		WithTransportStreamID(7),
		WithOriginalNetworkID(9),
	)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))

	payload := testPayload()
	for i := 0; i < 5; i++ {
		_, err := muxer.WriteData(&MuxerData{
			PID: 0x1234,
			PES: &PESData{Data: payload, Header: &PESHeader{}},
		})
		require.NoError(t, err)
	}

	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))
	var eits []*EITData
	var eitPIDs []uint16
	pesCount := 0
	var pat *PATData
	var pmt *PMTData
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)

		if d.PAT != nil {
			pat = d.PAT
		}
		if d.PMT != nil {
			pmt = d.PMT
		}
		if d.EIT != nil {
			eits = append(eits, d.EIT)
			eitPIDs = append(eitPIDs, d.PID)
		}
		if d.PES != nil {
			pesCount++
		}
	}

	require.NotNil(t, pat)
	require.NotNil(t, pmt)
	assert.Equal(t, uint16(7), pat.TransportStreamID)
	assert.Equal(t, programNumberStart, pmt.ProgramNumber)

	// EIT was received at least twice (first transmission + retransmission)
	require.GreaterOrEqual(t, len(eits), 2)

	for i, eit := range eits {
		assert.Equal(t, PIDEIT, eitPIDs[i])
		assert.Equal(t, uint16(7), eit.TransportStreamID)
		assert.Equal(t, uint16(9), eit.OriginalNetworkID)
		assert.Equal(t, programNumberStart, eit.ServiceID)
		assert.Equal(t, uint8(0), eit.SegmentLastSectionNumber)
		assert.Equal(t, uint8(0x4e), eit.LastTableID)
		require.Len(t, eit.Events, 2)

		current := eit.Events[0]
		assert.Equal(t, uint16(100), current.EventID)
		assert.True(t, eitStartTime.Equal(current.StartTime))
		assert.Equal(t, 90*time.Minute, current.Duration)
		assert.Equal(t, uint8(RunningStatusRunning), current.RunningStatus)
		assert.False(t, current.HasFreeCSAMode)
		require.Len(t, current.Descriptors, 1)
		assert.Equal(t, uint8(DescriptorTagShortEvent), current.Descriptors[0].Tag)
		require.NotNil(t, current.Descriptors[0].ShortEvent)
		assert.Equal(t, []byte("eng"), current.Descriptors[0].ShortEvent.Language)
		assert.Equal(t, []byte("Current"), current.Descriptors[0].ShortEvent.EventName)

		next := eit.Events[1]
		assert.Equal(t, uint16(101), next.EventID)
		assert.True(t, eitStartTime.Add(90*time.Minute).Equal(next.StartTime))
		assert.Equal(t, 45*time.Minute, next.Duration)
		assert.Equal(t, uint8(RunningStatusNotRunning), next.RunningStatus)
		require.Len(t, next.Descriptors, 1)
		require.NotNil(t, next.Descriptors[0].ShortEvent)
		assert.Equal(t, []byte("Next"), next.Descriptors[0].ShortEvent.EventName)
	}

	// PES packets still come through
	assert.Equal(t, 5, pesCount)
}

func TestMuxer_EITMultiPacketSection(t *testing.T) {
	// Two maximum-length titles make the section span several TS packets.
	longTitle := strings.Repeat("a", maxEITEventNameLength)
	current := EITEvent{EventID: 1, StartTime: eitStartTime, Duration: time.Hour, Language: "eng", Title: longTitle}
	next := EITEvent{EventID: 2, StartTime: eitStartTime.Add(time.Hour), Duration: time.Hour, Language: "eng", Title: longTitle}

	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(current, next))
	_, err := muxer.WriteTables()
	require.NoError(t, err)

	bs := buf.Bytes()
	require.Equal(t, 0, len(bs)%MpegTsPacketSize)

	// Count EIT packets and check the section layout: only the first packet sets the payload
	// unit start indicator, the section length spans the remaining packets, and continuity
	// counters increment for each packet.
	var eitPackets [][]byte
	for i := 0; i < len(bs); i += MpegTsPacketSize {
		pkt := bs[i : i+MpegTsPacketSize]
		if uint16(pkt[1]&0x1f)<<8|uint16(pkt[2]) == PIDEIT {
			eitPackets = append(eitPackets, pkt)
		}
	}
	require.Greater(t, len(eitPackets), 2, "expected a multi-packet EIT section")

	// section_length = syntax header (5) + EIT section data + CRC32 (4), where each event
	// costs its 12-byte header plus a short event descriptor (tag + length + language +
	// name length + name + text length).
	wantSectionLength := 5 + 6 + 2*(12+2+3+1+maxEITEventNameLength+1) + 4
	require.Equal(t, wantSectionLength, 9+int(calcEITSectionLength(&muxer.eit)))
	require.Greater(t, 1+3+wantSectionLength, MpegTsPacketSize, "section must span multiple packets")

	for i, pkt := range eitPackets {
		assert.Equal(t, i == 0, pkt[1]&0x40 != 0, "PUSI only expected on the first packet")
		assert.Equal(t, uint8(i&0x0f), pkt[3]&0x0f, "unexpected CC at EIT packet %d", i)
	}

	// The demuxer must reassemble the section and read back both events.
	dmx := NewDemuxer(context.Background(), bytes.NewReader(bs), DemuxerOptPacketSize(MpegTsPacketSize))
	var eit *EITData
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		if d.EIT != nil {
			eit = d.EIT
		}
	}
	require.NotNil(t, eit)
	require.Len(t, eit.Events, 2)
	assert.Equal(t, longTitle, string(eit.Events[0].Descriptors[0].ShortEvent.EventName))
	assert.Equal(t, longTitle, string(eit.Events[1].Descriptors[0].ShortEvent.EventName))
	assert.Equal(t, eitStartTime, eit.Events[0].StartTime)
	assert.Equal(t, eitStartTime.Add(time.Hour), eit.Events[1].StartTime)
}

func TestMuxer_EITRetransmissionVersionAndCC(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf, MuxerOptTablesRetransmitPeriod(1))
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))

	payload := testPayload()
	writePES := func() {
		_, err := muxer.WriteData(&MuxerData{
			PID: 0x1234,
			PES: &PESData{Data: payload, Header: &PESHeader{}},
		})
		require.NoError(t, err)
	}

	// Two transmissions of the initial EIT (tables are also written at the very start)
	writePES()
	writePES()

	// Update the advertised events: the next transmission must carry a new version
	updatedNext := eitNextEvent
	updatedNext.Title = "Next Updated"
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, updatedNext))
	writePES()
	writePES()

	// Scan the TS packets directly: the demuxer doesn't expose EIT version numbers and PSI
	// packets are reported without their payload.
	type eitRecord struct {
		version uint8
		cc      uint8
		payload []byte
	}
	var records []eitRecord
	bs := buf.Bytes()
	require.Equal(t, 0, len(bs)%MpegTsPacketSize)
	for i := 0; i < len(bs); i += MpegTsPacketSize {
		pkt := bs[i : i+MpegTsPacketSize]
		if pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2]); pid != PIDEIT {
			continue
		}
		records = append(records, eitRecord{
			// sync (1) + TS header (3) + pointer field (1) + table id/section length (3)
			// + table id extension (2)
			version: pkt[10] >> 1 & 0x1f,
			cc:      pkt[3] & 0x0f,
			payload: pkt,
		})
	}

	require.GreaterOrEqual(t, len(records), 3)

	// Initial transmissions advertise version 0 with increasing continuity counters
	require.Equal(t, uint8(0), records[0].version)
	require.Equal(t, uint8(0), records[0].cc)
	require.Equal(t, uint8(0), records[1].version)
	require.Equal(t, uint8(1), records[1].cc)
	assert.Contains(t, string(records[1].payload), "Next\x00")

	// After the update, the version must bump and the new title must be advertised
	last := records[len(records)-1]
	assert.Equal(t, uint8(1), last.version)
	assert.Contains(t, string(last.payload), "Next Updated")

	// Continuity counters increment monotonically modulo 16
	for i := 1; i < len(records); i++ {
		assert.Equal(t, (records[i-1].cc+1)&0x0f, records[i].cc,
			"unexpected CC at EIT transmission %d", i)
	}

	// The updated EIT still reads back correctly
	dmx := NewDemuxer(context.Background(), bytes.NewReader(bs), DemuxerOptPacketSize(MpegTsPacketSize))
	var lastEIT *EITData
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		if d.EIT != nil {
			lastEIT = d.EIT
		}
	}
	require.NotNil(t, lastEIT)
	require.Len(t, lastEIT.Events, 2)
	assert.Equal(t, []byte("Next Updated"), lastEIT.Events[1].Descriptors[0].ShortEvent.EventName)
}

func TestMuxer_EITDeliveredWithoutWaitingForEOF(t *testing.T) {
	// The EIT is emitted before the PES packets: a demuxer reading the live stream must
	// produce it without needing the end of the stream to flush its packet pool.
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))
	_, err := muxer.WriteTables()
	require.NoError(t, err)

	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))

	// Read the PAT and PMT first
	var sawPAT, sawPMT bool
	for !sawPAT || !sawPMT {
		d, err := dmx.NextData()
		require.NoError(t, err)
		if d.PAT != nil {
			sawPAT = true
		}
		if d.PMT != nil {
			sawPMT = true
		}
	}

	// The very next data must be the EIT, without reaching EOF first.
	d, err := dmx.NextData()
	require.NoError(t, err)
	require.NotNil(t, d.EIT, "EIT should be returned before EOF")
	assert.Equal(t, PIDEIT, d.PID)
	require.Len(t, d.EIT.Events, 2)
	assert.Equal(t, uint16(100), d.EIT.Events[0].EventID)
	assert.Equal(t, uint16(101), d.EIT.Events[1].EventID)
}

func TestMuxer_EITVersionAfterRepeatedSetEITBeforeFirstWrite(t *testing.T) {
	// Version numbers are 5 bits: the first write must start at 0 and each actual content
	// change must bump the version by exactly one, even if SetEIT is called several times
	// before anything is written.
	muxer := NewMuxer(context.Background(), nil)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))

	require.NoError(t, muxer.generateEIT())
	assert.Equal(t, uint8(0), muxer.eitBytes.Bytes()[10]>>1&0x1f)

	// No content change: version stays put
	require.NoError(t, muxer.generateEIT())
	assert.Equal(t, uint8(0), muxer.eitBytes.Bytes()[10]>>1&0x1f)

	// Content change: version bumps to 1
	updatedNext := eitNextEvent
	updatedNext.Title = "Updated"
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, updatedNext))
	require.NoError(t, muxer.generateEIT())
	assert.Equal(t, uint8(1), muxer.eitBytes.Bytes()[10]>>1&0x1f)
}

func TestMuxer_EITCustomPID(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf, WithEITPID(0x1f))
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(eitCurrentEvent, eitNextEvent))

	_, err := muxer.WriteTables()
	require.NoError(t, err)

	bs := buf.Bytes()
	require.Equal(t, 3*MpegTsPacketSize, len(bs))
	eitPkt := bs[2*MpegTsPacketSize:]
	assert.Equal(t, uint16(0x1f), uint16(eitPkt[1]&0x1f)<<8|uint16(eitPkt[2]))
}

func TestMuxer_EITStartTimeBoundaries(t *testing.T) {
	testCases := []struct {
		name    string
		current time.Time
		next    time.Time
		ok      bool
	}{
		{"min representable", eitMinStartTime, eitMinStartTime.Add(time.Hour), true},
		{"max representable", eitMaxStartTime.Add(-time.Hour), eitMaxStartTime, true},
		{"just before min", eitMinStartTime.Add(-time.Second), eitMinStartTime.Add(59 * time.Minute), false},
		{"just after max", eitMaxStartTime.Add(-59 * time.Minute), eitMaxStartTime.Add(time.Second), false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := bytes.Buffer{}
			muxer := NewMuxer(context.Background(), &buf)
			require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
				ElementaryPID: 0x1234,
				StreamType:    StreamTypeH264Video,
			}))
			muxer.SetPCRPID(0x1234)

			err := muxer.SetEIT(
				EITEvent{EventID: 1, StartTime: tc.current, Duration: time.Hour, Language: "eng", Title: "A"},
				EITEvent{EventID: 2, StartTime: tc.next, Duration: time.Hour, Language: "eng", Title: "B"},
			)
			if !tc.ok {
				assert.True(t, errors.Is(err, ErrEITStartTimeInvalid), "expected %v, got %v", ErrEITStartTimeInvalid, err)
				return
			}
			require.NoError(t, err)
			_, err = muxer.WriteTables()
			require.NoError(t, err)

			dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))
			var got *EITData
			for {
				d, err := dmx.NextData()
				if errors.Is(err, ErrNoMorePackets) {
					break
				}
				require.NoError(t, err)
				if d.EIT != nil {
					got = d.EIT
				}
			}
			require.NotNil(t, got)
			require.Len(t, got.Events, 2)
			assert.Equal(t, tc.current, got.Events[0].StartTime)
			assert.Equal(t, tc.next, got.Events[1].StartTime)
		})
	}
}

func TestMuxer_EITStartTimeConvertedToUTC(t *testing.T) {
	location := time.FixedZone("UTC+2", 2*60*60)
	start := time.Date(2024, time.January, 2, 5, 4, 5, 0, location)

	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetEIT(
		EITEvent{EventID: 100, StartTime: start, Duration: 90 * time.Minute, Language: "eng", Title: "Current"},
		EITEvent{EventID: 101, StartTime: start.Add(90 * time.Minute), Duration: 45 * time.Minute, Language: "eng", Title: "Next"},
	))
	_, errWrite := muxer.WriteTables()
	require.NoError(t, errWrite)

	// The advertised start time is the UTC instant, so it must read back as 03:04:05 UTC.
	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))
	var got *EITData
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		if d.EIT != nil {
			got = d.EIT
		}
	}
	require.NotNil(t, got)
	require.Len(t, got.Events, 2)
	assert.Equal(t, eitStartTime, got.Events[0].StartTime)
	assert.Equal(t, time.UTC, got.Events[0].StartTime.Location())
	assert.Equal(t, eitStartTime.Add(90*time.Minute), got.Events[1].StartTime)
}
