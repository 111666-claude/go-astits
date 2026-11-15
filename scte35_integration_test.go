package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scte35TestPID is the SCTE-35 elementary PID used by integration tests.
const scte35TestPID uint16 = 0x01f0

// videoTestPID is a normal H.264 elementary PID used alongside the cue.
const videoTestPID uint16 = 0x0101

// buildMuxerWithSCTE35 creates a muxer advertising a video stream and an
// SCTE-35 stream, and writes its PAT/PMT.
func buildMuxerWithSCTE35(t *testing.T, w *bytes.Buffer) *Muxer {
	m := NewMuxer(context.Background(), w,
		WithTransportStreamID(0x04d2),
		WithPMTPID(pmtStartPID),
	)
	require.NoError(t, m.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: videoTestPID,
		StreamType:    StreamTypeH264Video,
	}))
	require.NoError(t, m.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: scte35TestPID,
		StreamType:    StreamTypeSCTE35,
	}))
	m.SetPCRPID(videoTestPID)
	_, err := m.WriteTables()
	require.NoError(t, err)
	return m
}

// countPackets returns how many 188-byte TS packets carry a given PID.
func countPackets(t *testing.T, raw []byte, pid uint16) int {
	t.Helper()
	require.Equal(t, 0, len(raw)%MpegTsPacketSize)
	n := 0
	for off := 0; off+MpegTsPacketSize <= len(raw); off += MpegTsPacketSize {
		p := binaryPID(raw[off:])
		if p == pid {
			n++
		}
	}
	return n
}

func binaryPID(pkt []byte) uint16 {
	return uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
}

func TestMuxerDemuxer_SCTE35_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	m := buildMuxerWithSCTE35(t, &buf)

	cue := buildSpliceInsertSection(t)
	// Use a recognizable tier/adjustment. Add a large unknown descriptor so
	// the PES packet is forced to span several TS packets.
	cue.Tier = 0xabc
	cue.PTSAdjustment = 0x11223344
	cue.Descriptors = append(cue.Descriptors, &SCTE35SpliceDescriptor{
		Tag: 0x70,
		Raw: bytes.Repeat([]byte{0x5a}, 200),
	})
	n, err := m.WriteData(&MuxerData{PID: scte35TestPID, SCTE35: cue})
	require.NoError(t, err)
	assert.Greater(t, n, MpegTsPacketSize)

	// A normal video PES packet must coexist on its own PID.
	videoPES := &PESData{
		Data: bytes.Repeat([]byte{0xaa}, 300),
		Header: &PESHeader{
			OptionalHeader: &PESOptionalHeader{
				DataAlignmentIndicator: true,
				PTSDTSIndicator:        PTSDTSIndicatorOnlyPTS,
				PTS:                    &ClockReference{Base: 90000 * 10},
			},
		},
	}
	_, err = m.WriteData(&MuxerData{PID: videoTestPID, PES: videoPES})
	require.NoError(t, err)

	raw := buf.Bytes()

	// The cue section is large enough to span more than one TS packet.
	assert.Greater(t, countPackets(t, raw, scte35TestPID), 1)

	// Demux the whole stream.
	dmx := NewDemuxer(context.Background(), bytes.NewReader(raw), DemuxerOptPacketSize(MpegTsPacketSize))
	var (
		gotPAT    *PATData
		gotPMT    *PMTData
		gotCue    *SCTE35SpliceInfoSection
		gotCuePID uint16
		videoCnt  int
	)
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		switch {
		case d.PAT != nil:
			gotPAT = d.PAT
		case d.PMT != nil:
			gotPMT = d.PMT
		case d.SCTE35 != nil:
			gotCue = d.SCTE35
			gotCuePID = d.PID
		case d.PES != nil && d.PID == videoTestPID:
			videoCnt++
		}
	}

	require.NotNil(t, gotPAT)
	require.NotNil(t, gotPMT)
	require.NotNil(t, gotCue)
	assert.Equal(t, scte35TestPID, gotCuePID)
	assert.Equal(t, 1, videoCnt, "video PES must survive alongside the cue")

	// The PMT must still advertise the SCTE-35 stream type.
	var foundSCTE35 bool
	for _, es := range gotPMT.ElementaryStreams {
		if es.ElementaryPID == scte35TestPID {
			assert.Equal(t, StreamTypeSCTE35, es.StreamType)
			foundSCTE35 = true
		}
	}
	assert.True(t, foundSCTE35)

	// The structured cue must survive the full mux/demux round-trip.
	assert.Equal(t, uint16(0xabc), gotCue.Tier)
	assert.Equal(t, uint64(0x11223344), gotCue.PTSAdjustment)
	require.NotNil(t, gotCue.SpliceCommand)
	require.NotNil(t, gotCue.SpliceCommand.SpliceInsert)
	si := gotCue.SpliceCommand.SpliceInsert
	assert.Equal(t, uint32(0x11223344), si.SpliceEventID)
	require.NotNil(t, si.SpliceTime)
	assert.Equal(t, uint64(0x180000123), si.SpliceTime.PTS)
	require.NotNil(t, si.BreakDuration)
	assert.Equal(t, uint64(90000*30), si.BreakDuration.Duration)
	require.GreaterOrEqual(t, len(gotCue.Descriptors), 1)
	require.NotNil(t, gotCue.Descriptors[0].Segmentation)
	assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, gotCue.Descriptors[0].Segmentation.SegmentationUPID)
	// The padding descriptor round-trips losslessly across packet boundaries.
	require.Len(t, gotCue.Descriptors, 2)
	assert.Equal(t, uint8(0x70), gotCue.Descriptors[1].Tag)
	assert.Equal(t, bytes.Repeat([]byte{0x5a}, 200), gotCue.Descriptors[1].Raw)
}

func TestMuxerDemuxer_SCTE35_TimeSignalCrossPacket(t *testing.T) {
	var buf bytes.Buffer
	m := buildMuxerWithSCTE35(t, &buf)

	// A time_signal carrying several descriptors makes the section spill
	// across several TS packets.
	cue := &SCTE35SpliceInfoSection{
		Tier:          0x777,
		PTSAdjustment: 0x000000005,
		SpliceCommand: &SCTE35SpliceCommand{
			Type:       SCTE35SpliceCommandTypeTimeSignal,
			TimeSignal: &SCTE35SpliceTime{TimeSpecified: true, PTS: 0x1ffffffff},
		},
		Descriptors: []*SCTE35SpliceDescriptor{
			{Tag: SCTE35DescriptorTagSegmentation, Segmentation: &SCTE35SegmentationDescriptor{
				SegmentationEventID:       1,
				ProgramSegmentationFlag:   true,
				DeliveryNotRestrictedFlag: true,
				SegmentationTypeID:        SCTE35SegmentationTypeProgramStart,
				SegmentNum:                1,
				SegmentsExpected:          1,
			}},
			{Tag: 0x60, Raw: bytes.Repeat([]byte{0x06}, 200)},
		},
	}
	_, err := m.WriteData(&MuxerData{PID: scte35TestPID, SCTE35: cue})
	require.NoError(t, err)

	raw := buf.Bytes()
	assert.Greater(t, countPackets(t, raw, scte35TestPID), 1)

	dmx := NewDemuxer(context.Background(), bytes.NewReader(raw), DemuxerOptPacketSize(MpegTsPacketSize))
	var got *SCTE35SpliceInfoSection
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		if d.SCTE35 != nil {
			got = d.SCTE35
		}
	}
	require.NotNil(t, got)
	require.NotNil(t, got.SpliceCommand.TimeSignal)
	assert.Equal(t, uint64(0x1ffffffff), got.SpliceCommand.TimeSignal.PTS)
	require.Len(t, got.Descriptors, 2)
	assert.Equal(t, uint8(0x60), got.Descriptors[1].Tag)
	assert.Equal(t, bytes.Repeat([]byte{0x06}, 200), got.Descriptors[1].Raw)
}

func TestMuxer_SCTE35_RejectsWrongStreamType(t *testing.T) {
	var buf bytes.Buffer
	m := buildMuxerWithSCTE35(t, &buf)

	// videoTestPID is H.264, not SCTE-35.
	_, err := m.WriteData(&MuxerData{
		PID:    videoTestPID,
		SCTE35: &SCTE35SpliceInfoSection{SpliceCommand: &SCTE35SpliceCommand{Type: SCTE35SpliceCommandTypeSpliceNull}},
	})
	require.Error(t, err)
}

func TestDemuxer_SCTE35_CorruptSectionReturnsError(t *testing.T) {
	var buf bytes.Buffer
	m := buildMuxerWithSCTE35(t, &buf)
	cue := buildSpliceInsertSection(t)
	_, err := m.WriteData(&MuxerData{PID: scte35TestPID, SCTE35: cue})
	require.NoError(t, err)

	raw := buf.Bytes()

	// Locate the first SCTE-35 packet, find the 0xFC table id inside its
	// payload and flip the following byte so the section CRC fails.
	corrupted := append([]byte(nil), raw...)
	for off := 0; off+MpegTsPacketSize <= len(corrupted); off += MpegTsPacketSize {
		if binaryPID(corrupted[off:]) != scte35TestPID {
			continue
		}
		pkt := corrupted[off : off+MpegTsPacketSize]
		for j := 4; j < len(pkt); j++ {
			if pkt[j] == scte35TableID {
				pkt[j+5] ^= 0xff // corrupt a body byte well inside the section
				goto corrupted
			}
		}
	}
corrupted:
	dmx := NewDemuxer(context.Background(), bytes.NewReader(corrupted), DemuxerOptPacketSize(MpegTsPacketSize))
	var sawSCTE35 bool
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		if err != nil {
			// The demuxer logs parse errors internally and continues, but it
			// must never emit a corrupted cue.
			continue
		}
		if d != nil && d.SCTE35 != nil {
			sawSCTE35 = true
		}
	}
	assert.False(t, sawSCTE35, "a corrupted section must not be delivered as a cue")
}

func TestDemuxer_SCTE35_ManualPacketReassembly(t *testing.T) {
	// Verify the parseData entry point directly, feeding a section split
	// across two arbitrary packet fragments.
	section, err := WriteSCTE35SpliceInfoSection(buildSpliceInsertSection(t))
	require.NoError(t, err)

	header := &PESHeader{
		StreamID: StreamIDSCTE35,
		OptionalHeader: &PESOptionalHeader{
			HasOptionalFields: true,
		},
	}
	var pesBuf bytes.Buffer
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &pesBuf})
	_, _, err = writePESData(pw, header, section, true, len(section)+64)
	require.NoError(t, err)
	full := pesBuf.Bytes()
	split := 40

	esm := newElementaryStreamMap()
	esm.setStreamUnlocked(scte35TestPID, 1, StreamTypeSCTE35)
	ps := []*Packet{
		{Header: PacketHeader{PID: scte35TestPID, PayloadUnitStartIndicator: true}, Payload: full[:split]},
		{Header: PacketHeader{PID: scte35TestPID}, Payload: full[split:]},
	}
	ds, err := parseData(ps, nil, newProgramMap(), esm)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	require.NotNil(t, ds[0].SCTE35)
	assert.Equal(t, buildSpliceInsertSection(t).SpliceCommand.SpliceInsert.SpliceEventID,
		ds[0].SCTE35.SpliceCommand.SpliceInsert.SpliceEventID)
}
