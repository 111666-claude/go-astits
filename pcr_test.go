package astits

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pcrTestPacket builds a hand-made 188 bytes TS packet
func pcrTestPacket(t *testing.T, p *Packet) []byte {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	n, err := writePacket(w, p, MpegTsPacketSize)
	require.NoError(t, err)
	require.Equal(t, MpegTsPacketSize, n)
	return buf.Bytes()
}

// pcrTestPCRPacket builds a hand-made TS packet carrying a PCR
func pcrTestPCRPacket(t *testing.T, pid uint16, base, ext int64, discontinuity bool) []byte {
	return pcrTestPacket(t, &Packet{
		Header: PacketHeader{PID: pid, HasAdaptationField: true, HasPayload: true},
		AdaptationField: &PacketAdaptationField{
			DiscontinuityIndicator: discontinuity,
			HasPCR:                 true,
			PCR:                    &ClockReference{Base: base, Extension: ext},
		},
		Payload: []byte{0x1, 0x2, 0x3, 0x4},
	})
}

// pcrTestPayloadPacket builds a hand-made TS packet whose payload length is
// exact, the rest of the packet being stuffed through the adaptation field
func pcrTestPayloadPacket(t *testing.T, pid uint16, pusi bool, payload []byte) []byte {
	return pcrTestPacket(t, &Packet{
		Header: PacketHeader{
			PID:                       pid,
			PayloadUnitStartIndicator: pusi,
			HasAdaptationField:        true,
			HasPayload:                true,
		},
		AdaptationField: &PacketAdaptationField{StuffingLength: MpegTsPacketSize - 6 - len(payload)},
		Payload:         payload,
	})
}

// pcrTestPSIPayload serializes PSI data
func pcrTestPSIPayload(t *testing.T, d *PSIData) []byte {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	_, err := writePSIData(w, d)
	require.NoError(t, err)
	return buf.Bytes()
}

// pcrTestPSIPacket builds a hand-made TS packet carrying PSI data
func pcrTestPSIPacket(t *testing.T, pid uint16, d *PSIData) []byte {
	return pcrTestPacket(t, &Packet{
		Header:  PacketHeader{PID: pid, PayloadUnitStartIndicator: true, HasPayload: true},
		Payload: pcrTestPSIPayload(t, d),
	})
}

// pcrTestPSIData builds PSI data with a computed section length
func pcrTestPSIData(tableID PSITableID, tableIDExtension uint16, d *PSISectionSyntaxData) *PSIData {
	s := &PSISection{
		Header: &PSISectionHeader{TableID: tableID, SectionSyntaxIndicator: true},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{TableIDExtension: tableIDExtension, CurrentNextIndicator: true},
			Data:   d,
		},
	}
	s.Header.SectionLength = calcPSISectionLength(s)
	return &PSIData{Sections: []*PSISection{s}}
}

// pcrTestPAT builds PAT PSI data
func pcrTestPAT(programs ...*PATProgram) *PSIData {
	return pcrTestPSIData(PSITableIDPAT, 1, &PSISectionSyntaxData{
		PAT: &PATData{TransportStreamID: 1, Programs: programs},
	})
}

// pcrTestPMT builds PMT PSI data
func pcrTestPMT(programNumber, pcrPID uint16) *PSIData {
	return pcrTestPSIData(PSITableIDPMT, programNumber, &PSISectionSyntaxData{
		PMT: &PMTData{
			ProgramNumber: programNumber,
			PCRPID:        pcrPID,
			ElementaryStreams: []*PMTElementaryStream{{
				StreamType:    StreamTypeH264Video,
				ElementaryPID: pcrPID,
			}},
		},
	})
}

// pcrTestFeed runs packets through a Demuxer and feeds the packet sequence to the extractor
func pcrTestFeed(t *testing.T, e *PCRExtractor, packets ...[]byte) (ds []*PCRData) {
	dmx := NewDemuxer(context.Background(), bytes.NewReader(bytes.Join(packets, nil)), DemuxerOptPacketSize(MpegTsPacketSize))
	for {
		p, err := dmx.NextPacket()
		if err == ErrNoMorePackets {
			break
		}
		require.NoError(t, err)
		if d := e.ProcessPacket(p); d != nil {
			ds = append(ds, d)
		}
	}
	return
}

func pcrTimestamp(base, ext int64) time.Duration {
	return ClockReference{Base: base, Extension: ext}.Duration()
}

func TestPCRExtractorNew(t *testing.T) {
	e := NewPCRExtractor(PCRExtractorOptPCRPIDs(1, 2), PCRExtractorOptJitterTolerance(time.Second))
	assert.True(t, e.optPCRPIDs[1])
	assert.True(t, e.optPCRPIDs[2])
	assert.Equal(t, int64(90000), e.optJitterTolerance)
	e = NewPCRExtractor()
	assert.Equal(t, int64(3600), e.optJitterTolerance)
}

func TestPCRExtractorWrap(t *testing.T) {
	e := NewPCRExtractor(PCRExtractorOptPCRPIDs(100))
	maxBase := pcrBaseModulo - 1
	ds := pcrTestFeed(t, e,
		pcrTestPCRPacket(t, 100, maxBase-1, 0, false),
		pcrTestPCRPacket(t, 100, maxBase, 10, false),
		pcrTestPCRPacket(t, 100, 1, 20, false), // base wrap: +2 ticks
		pcrTestPCRPacket(t, 100, 5, 0, false),
	)
	require.Len(t, ds, 4)

	// PCR values, packet indexes and unwrapped timestamps
	assert.Equal(t, ClockReference{Base: maxBase - 1}, ds[0].PCR)
	assert.Equal(t, uint64(0), ds[0].PacketIndex)
	assert.Equal(t, pcrTimestamp(maxBase-1, 0), ds[0].Timestamp)
	assert.False(t, ds[0].Discontinuity)

	assert.Equal(t, pcrTimestamp(maxBase, 10), ds[1].Timestamp)
	assert.Equal(t, uint64(1), ds[1].PacketIndex)

	// The wrap is resolved: the unwrapped base keeps moving forward
	assert.Equal(t, pcrTimestamp(pcrBaseModulo+1, 20), ds[2].Timestamp)
	assert.Equal(t, pcrTimestamp(pcrBaseModulo+5, 0), ds[3].Timestamp)
	for _, d := range ds {
		assert.Equal(t, uint16(100), d.PID)
		assert.Equal(t, uint16(0), d.ProgramNumber)
		assert.False(t, d.Discontinuity)
	}
}

func TestPCRExtractorPCRPIDSwitch(t *testing.T) {
	e := NewPCRExtractor()
	pat := pcrTestPSIPacket(t, PIDPAT, pcrTestPAT(&PATProgram{ProgramNumber: 1, ProgramMapID: 0x1000}))
	pmt1 := pcrTestPSIPacket(t, 0x1000, pcrTestPMT(1, 200))
	pmt2 := pcrTestPSIPacket(t, 0x1000, pcrTestPMT(1, 300))
	ds := pcrTestFeed(t, e,
		pat,
		pmt1,
		pcrTestPCRPacket(t, 200, 90000, 0, false),
		pcrTestPCRPacket(t, 200, 180000, 0, false),
		pmt2,
		pcrTestPCRPacket(t, 300, 45000, 0, false),  // PCR PID switch: rebased on 180000
		pcrTestPCRPacket(t, 200, 270000, 0, false), // old PID is not tracked anymore
		pcrTestPCRPacket(t, 300, 90000, 0, false),  // +45000 ticks
	)
	require.Len(t, ds, 4)

	assert.Equal(t, uint16(1), ds[0].ProgramNumber)
	assert.Equal(t, uint16(200), ds[0].PID)
	assert.Equal(t, pcrTimestamp(90000, 0), ds[0].Timestamp)
	assert.False(t, ds[0].Discontinuity)

	assert.Equal(t, pcrTimestamp(180000, 0), ds[1].Timestamp)

	// The track switches to the new PID and the discontinuity is flagged
	assert.Equal(t, uint16(300), ds[2].PID)
	assert.Equal(t, uint16(1), ds[2].ProgramNumber)
	assert.True(t, ds[2].Discontinuity)
	assert.Equal(t, pcrTimestamp(180000, 0), ds[2].Timestamp)
	assert.Equal(t, uint64(5), ds[2].PacketIndex)

	// The timeline carries over to the new PID
	assert.Equal(t, pcrTimestamp(225000, 0), ds[3].Timestamp)
	assert.False(t, ds[3].Discontinuity)
}

func TestPCRExtractorDiscontinuityIndicator(t *testing.T) {
	e := NewPCRExtractor(PCRExtractorOptPCRPIDs(100))
	ds := pcrTestFeed(t, e,
		pcrTestPCRPacket(t, 100, 90000, 0, false),
		pcrTestPCRPacket(t, 100, 180000, 0, false),
		pcrTestPCRPacket(t, 100, 1000, 0, true), // discontinuity indicator: rebase
		pcrTestPCRPacket(t, 100, 46000, 0, false),
	)
	require.Len(t, ds, 4)
	assert.False(t, ds[0].Discontinuity)
	assert.False(t, ds[1].Discontinuity)
	assert.True(t, ds[2].Discontinuity)
	assert.Equal(t, pcrTimestamp(180000, 0), ds[2].Timestamp)
	assert.Equal(t, pcrTimestamp(225000, 0), ds[3].Timestamp)
}

func TestPCRExtractorInvalidExtension(t *testing.T) {
	e := NewPCRExtractor(PCRExtractorOptPCRPIDs(100))
	ds := pcrTestFeed(t, e,
		pcrTestPCRPacket(t, 100, 90000, 0, false),
		pcrTestPCRPacket(t, 100, 180000, 300, false), // invalid extension: dropped
		pcrTestPCRPacket(t, 100, 270000, 0, false),   // track was reset: discontinuity
		pcrTestPCRPacket(t, 100, 360000, 299, false),
	)
	require.Len(t, ds, 3)
	assert.Equal(t, pcrTimestamp(90000, 0), ds[0].Timestamp)
	assert.True(t, ds[1].Discontinuity)
	assert.Equal(t, pcrTimestamp(90000, 0), ds[1].Timestamp)
	assert.Equal(t, pcrTimestamp(180000, 299), ds[2].Timestamp)
	assert.False(t, ds[2].Discontinuity)
}

func TestPCRExtractorBackwardJitter(t *testing.T) {
	// Backward jumps within the jitter tolerance are emitted as is
	e := NewPCRExtractor(PCRExtractorOptPCRPIDs(100))
	ds := pcrTestFeed(t, e,
		pcrTestPCRPacket(t, 100, 90000, 0, false),
		pcrTestPCRPacket(t, 100, 180000, 0, false),
		pcrTestPCRPacket(t, 100, 179000, 0, false), // 1000 ticks backward: jitter
		pcrTestPCRPacket(t, 100, 270000, 0, false),
	)
	require.Len(t, ds, 4)
	assert.Equal(t, pcrTimestamp(179000, 0), ds[2].Timestamp)
	assert.False(t, ds[2].Discontinuity)
	assert.Equal(t, pcrTimestamp(270000, 0), ds[3].Timestamp)

	// Backward jumps beyond the jitter tolerance are discontinuities
	e = NewPCRExtractor(PCRExtractorOptPCRPIDs(100))
	ds = pcrTestFeed(t, e,
		pcrTestPCRPacket(t, 100, 90000, 0, false),
		pcrTestPCRPacket(t, 100, 180000, 0, false),
		pcrTestPCRPacket(t, 100, 10000, 0, false), // 170000 ticks backward: discontinuity
		pcrTestPCRPacket(t, 100, 55000, 0, false),
	)
	require.Len(t, ds, 4)
	assert.True(t, ds[2].Discontinuity)
	assert.Equal(t, pcrTimestamp(180000, 0), ds[2].Timestamp)
	assert.Equal(t, pcrTimestamp(225000, 0), ds[3].Timestamp)
}

func TestPCRExtractorMissingPMT(t *testing.T) {
	// Without a PMT and without manual PIDs, nothing is produced
	e := NewPCRExtractor()
	ds := pcrTestFeed(t, e, pcrTestPCRPacket(t, 200, 90000, 0, false))
	assert.Empty(t, ds)

	// Once the PMT declares the PCR PID, samples are produced
	ds = pcrTestFeed(t, e,
		pcrTestPSIPacket(t, PIDPAT, pcrTestPAT(&PATProgram{ProgramNumber: 1, ProgramMapID: 0x1000})),
		pcrTestPSIPacket(t, 0x1000, pcrTestPMT(1, 200)),
		pcrTestPCRPacket(t, 200, 90000, 0, false),
	)
	require.Len(t, ds, 1)
	assert.Equal(t, uint16(1), ds[0].ProgramNumber)
}

func TestPCRExtractorMultiplePrograms(t *testing.T) {
	e := NewPCRExtractor()
	ds := pcrTestFeed(t, e,
		pcrTestPSIPacket(t, PIDPAT, pcrTestPAT(
			&PATProgram{ProgramNumber: 1, ProgramMapID: 0x1000},
			&PATProgram{ProgramNumber: 2, ProgramMapID: 0x1001},
		)),
		pcrTestPSIPacket(t, 0x1000, pcrTestPMT(1, 200)),
		pcrTestPSIPacket(t, 0x1001, pcrTestPMT(2, 300)),
		pcrTestPCRPacket(t, 200, 90000, 0, false),
		pcrTestPCRPacket(t, 300, 45000, 0, false),
		pcrTestPCRPacket(t, 200, 180000, 0, false),
		pcrTestPCRPacket(t, 300, 90000, 0, false),
	)
	require.Len(t, ds, 4)

	// Each program is tracked independently
	assert.Equal(t, uint16(1), ds[0].ProgramNumber)
	assert.Equal(t, pcrTimestamp(90000, 0), ds[0].Timestamp)
	assert.Equal(t, uint16(2), ds[1].ProgramNumber)
	assert.Equal(t, pcrTimestamp(45000, 0), ds[1].Timestamp)
	assert.Equal(t, pcrTimestamp(180000, 0), ds[2].Timestamp)
	assert.Equal(t, pcrTimestamp(90000, 0), ds[3].Timestamp)
}

func TestPCRExtractorNoPCR(t *testing.T) {
	// A program whose PMT declares PCRPID 0x1FFF has no PCR
	e := NewPCRExtractor()
	ds := pcrTestFeed(t, e,
		pcrTestPSIPacket(t, PIDPAT, pcrTestPAT(&PATProgram{ProgramNumber: 1, ProgramMapID: 0x1000})),
		pcrTestPSIPacket(t, 0x1000, pcrTestPMT(1, PIDNull)),
		pcrTestPCRPacket(t, 200, 90000, 0, false),
	)
	assert.Empty(t, ds)
}

func TestPCRExtractorSplitPAT(t *testing.T) {
	// A PAT section spanning over two packets is reassembled
	payload := pcrTestPSIPayload(t, pcrTestPAT(&PATProgram{ProgramNumber: 1, ProgramMapID: 0x1000}))
	e := NewPCRExtractor()
	ds := pcrTestFeed(t, e,
		pcrTestPayloadPacket(t, PIDPAT, true, payload[:5]),
		pcrTestPayloadPacket(t, PIDPAT, false, payload[5:]),
		pcrTestPSIPacket(t, 0x1000, pcrTestPMT(1, 200)),
		pcrTestPCRPacket(t, 200, 90000, 0, false),
	)
	require.Len(t, ds, 1)
	assert.Equal(t, uint16(1), ds[0].ProgramNumber)
	assert.Equal(t, pcrTimestamp(90000, 0), ds[0].Timestamp)
}
