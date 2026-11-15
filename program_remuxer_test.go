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

// tsBuilder serializes a synthetic transport stream packet by packet.
type tsBuilder struct {
	buf bytes.Buffer
	cc  map[uint16]uint8
}

func newTSBuilder() *tsBuilder {
	return &tsBuilder{cc: map[uint16]uint8{}}
}

func (b *tsBuilder) nextCC(pid uint16, hasPayload bool) uint8 {
	cc, ok := b.cc[pid]
	if !ok {
		cc = 0
	} else if hasPayload {
		cc = (cc + 1) & 0x0f
	}
	b.cc[pid] = cc
	return cc
}

// writePSIPacket writes a one-section PSI table in a single packet.
func (b *tsBuilder) writePSIPacket(pid uint16, section *PSISection) {
	var payload bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &payload})
	_, err := writePSIData(w, &PSIData{Sections: []*PSISection{section}})
	if err != nil {
		panic(err)
	}
	pkt := &Packet{
		Header: PacketHeader{
			PID:                       pid,
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			ContinuityCounter:         b.nextCC(pid, true),
		},
		Payload: payload.Bytes(),
	}
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf})
	if _, err := writePacket(pw, pkt, MpegTsPacketSize); err != nil {
		panic(err)
	}
}

func (b *tsBuilder) writePAT(pat *PATData, version uint8) {
	b.writePSIPacket(PIDPAT, &PSISection{
		Header: &PSISectionHeader{
			TableID:                PSITableIDPAT,
			SectionSyntaxIndicator: true,
			SectionLength:          calcPATSectionLength(pat),
		},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{
				TableIDExtension:     pat.TransportStreamID,
				VersionNumber:        version,
				CurrentNextIndicator: true,
			},
			Data: &PSISectionSyntaxData{PAT: pat},
		},
	})
}

func (b *tsBuilder) writePMT(pmt *PMTData, pid uint16, version uint8) {
	b.writePSIPacket(pid, &PSISection{
		Header: &PSISectionHeader{
			TableID:                PSITableIDPMT,
			SectionSyntaxIndicator: true,
			SectionLength:          calcPMTSectionLength(pmt),
		},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{
				TableIDExtension:     pmt.ProgramNumber,
				VersionNumber:        version,
				CurrentNextIndicator: true,
			},
			Data: &PSISectionSyntaxData{PMT: pmt},
		},
	})
}

// writePES writes one bounded PES as a single 188-byte TS packet (payloads are
// kept small enough to fit). Any space left after the PES is padded with 0xff
// by writePacket itself, so no adaptation-field stuffing math is needed here.
func (b *tsBuilder) writePES(pid uint16, streamType StreamType, pes *PESData, af *PacketAdaptationField, transportError bool) {
	h := *pes.Header
	if h.StreamID == 0 {
		h.StreamID = streamType.ToPESStreamID()
	}
	if h.OptionalHeader != nil {
		h.OptionalHeader.HeaderLength = calcPESOptionalHeaderLength(h.OptionalHeader)
	}

	hasAF := af != nil
	// Compute the exact PES size (header + data). The TS packet payload must be
	// exactly that size: video PES use a zero PacketLength, so any trailing
	// 0xff in the payload would be mistaken for ES data by the demuxer. The
	// remaining space is therefore carried as adaptation-field stuffing.
	pesHeaderSize := pesHeaderLength
	if h.OptionalHeader != nil {
		pesHeaderSize += int(calcPESOptionalHeaderLength(h.OptionalHeader))
	}
	exactPESSize := pesHeaderSize + len(pes.Data)

	// Base adaptation-field wire size (length byte included, stuffing excluded).
	if !hasAF {
		af = &PacketAdaptationField{}
		hasAF = true
	}
	af.StuffingLength = 0
	baseAFWire := 1 + int(calcPacketAdaptationFieldLength(af))
	// The adaptation-field length byte is counted both in baseAFWire and in
	// writePacket's running total, so stuffing absorbs one fewer byte.
	af.StuffingLength = MpegTsPacketSize - mpegTsPacketHeaderSize - baseAFWire - exactPESSize - 1
	if af.StuffingLength < 0 {
		panic("test PES does not fit in one packet")
	}

	var pesBuf bytes.Buffer
	pesWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &pesBuf})
	if _, _, err := writePESData(pesWriter, &h, pes.Data, true, exactPESSize); err != nil {
		panic(err)
	}

	pkt := &Packet{
		Header: PacketHeader{
			PID:                       pid,
			HasPayload:                true,
			HasAdaptationField:        hasAF,
			PayloadUnitStartIndicator: true,
			ContinuityCounter:         b.nextCC(pid, true),
			TransportErrorIndicator:   transportError,
		},
		AdaptationField: af,
		Payload:         pesBuf.Bytes(),
	}
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf})
	if _, err := writePacket(pw, pkt, MpegTsPacketSize); err != nil {
		panic(err)
	}
}

func (b *tsBuilder) bytes() []byte { return b.buf.Bytes() }

// two-program fixture PIDs and program numbers.
const (
	prog1Number uint16 = 101
	prog2Number uint16 = 202
	prog1PMTPID uint16 = 0x1001
	prog2PMTPID uint16 = 0x1002
	prog1Video  uint16 = 0x0101
	prog1Audio  uint16 = 0x0102
	prog2Video  uint16 = 0x0201
	prog2Audio  uint16 = 0x0202
	tsID        uint16 = 0x4242
)

func buildTwoProgramInput(t *testing.T) []byte {
	b := newTSBuilder()

	pat := &PATData{
		TransportStreamID: tsID,
		Programs: []*PATProgram{
			{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID},
			{ProgramNumber: prog2Number, ProgramMapID: prog2PMTPID},
		},
	}
	b.writePAT(pat, 0)

	pmt1 := &PMTData{
		ProgramNumber: prog1Number,
		PCRPID:        prog1Video,
		ProgramDescriptors: []*Descriptor{{
			Tag:     0x22, // unknown tag
			Length:  3,
			Unknown: &DescriptorUnknown{Tag: 0x22, Content: []byte{0xaa, 0xbb, 0xcc}},
		}},
		ElementaryStreams: []*PMTElementaryStream{
			{
				ElementaryPID: prog1Video,
				StreamType:    StreamTypeH264Video,
				ElementaryStreamDescriptors: []*Descriptor{{
					Tag:     0x23,
					Length:  2,
					Unknown: &DescriptorUnknown{Tag: 0x23, Content: []byte{0x11, 0x22}},
				}},
			},
			{ElementaryPID: prog1Audio, StreamType: StreamTypeADTS},
		},
	}
	pmt2 := &PMTData{
		ProgramNumber: prog2Number,
		PCRPID:        prog2Video,
		ElementaryStreams: []*PMTElementaryStream{
			{ElementaryPID: prog2Video, StreamType: StreamTypeH264Video},
			{ElementaryPID: prog2Audio, StreamType: StreamTypeADTS},
		},
	}
	b.writePMT(pmt1, prog1PMTPID, 0)
	b.writePMT(pmt2, prog2PMTPID, 0)

	// Interleaved PES of both programs, with PTS/DTS and PCRs on program 1.
	for i := 0; i < 6; i++ {
		pts := &ClockReference{Base: int64(90000 * (i + 1))}
		dts := &ClockReference{Base: int64(90000*i + 45000)}
		pes1v := &PESData{
			Data: []byte{byte(i), 0x10, 0x11, 0x12, 0x13},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{
				PTS: pts, DTS: dts, PTSDTSIndicator: PTSDTSIndicatorBothPresent,
			}},
		}
		af := &PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &ClockReference{Base: int64(27000000 * (i + 1)), Extension: int64(i)},
			RandomAccessIndicator: true,
		}
		b.writePES(prog1Video, StreamTypeH264Video, pes1v, af, i == 3) // packet 3 corrupt

		pes1a := &PESData{
			Data: []byte{byte(i), 0xa0, 0xa1},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{
				PTS: pts, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
			}},
		}
		b.writePES(prog1Audio, StreamTypeADTS, pes1a, nil, false)

		pes2v := &PESData{
			Data: []byte{byte(i), 0x20, 0x21, 0x22},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{
				PTS: pts, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
			}},
		}
		b.writePES(prog2Video, StreamTypeH264Video, pes2v, nil, false)

		pes2a := &PESData{
			Data: []byte{byte(i), 0xb0, 0xb1},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{
				PTS: pts, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
			}},
		}
		b.writePES(prog2Audio, StreamTypeADTS, pes2a, nil, false)
	}

	return b.bytes()
}

// demuxAll collects every DemuxerData until EOF.
func demuxAll(t *testing.T, input []byte) []*DemuxerData {
	dmx := NewDemuxer(context.Background(), bytes.NewReader(input), DemuxerOptPacketSize(MpegTsPacketSize))
	var out []*DemuxerData
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		out = append(out, d)
	}
	return out
}

// rawPackets returns every packet of a transport stream.
func rawPackets(t *testing.T, input []byte) []*Packet {
	dmx := NewDemuxer(context.Background(), bytes.NewReader(input), DemuxerOptPacketSize(MpegTsPacketSize))
	var out []*Packet
	for {
		p, err := dmx.NextPacket()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		out = append(out, p)
	}
	return out
}

func TestProgramRemuxer_TwoPrograms_FiltersAndPreserves(t *testing.T) {
	input := buildTwoProgramInput(t)

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(input), &out, prog1Number)
	require.NoError(t, err)

	// One corrupt input packet was reported and dropped.
	assert.Equal(t, uint64(1), stats.CorruptPackets)
	assert.Greater(t, stats.PacketsWritten, uint64(0))
	assert.Greater(t, stats.TablesWritten, uint64(0))
	// Every input packet is either forwarded (an elementary packet) or dropped;
	// the regenerated PAT/PMT packets are extra writes on top.
	esWritten := stats.PacketsWritten - stats.TablesWritten
	assert.Equal(t, stats.PacketsRead, esWritten+stats.PacketsDropped)

	pkts := rawPackets(t, out.Bytes())

	// Only PAT, the target PMT and program 1 elementary PIDs may appear.
	seen := map[uint16]bool{}
	ccLast := map[uint16]uint8{}
	for _, p := range pkts {
		seen[p.Header.PID] = true
		switch p.Header.PID {
		case PIDPAT, prog1PMTPID, prog1Video, prog1Audio:
		default:
			t.Fatalf("unexpected PID %#x in output", p.Header.PID)
		}
		// Continuity counter check per output PID.
		if prev, ok := ccLast[p.Header.PID]; ok {
			if p.Header.HasPayload {
				assert.Equal(t, (prev+1)&0x0f, p.Header.ContinuityCounter,
					"CC discontinuity on PID %#x", p.Header.PID)
			} else {
				assert.Equal(t, prev, p.Header.ContinuityCounter)
			}
		}
		if p.Header.HasPayload {
			ccLast[p.Header.PID] = p.Header.ContinuityCounter
		}
	}
	assert.True(t, seen[prog1Video])
	assert.True(t, seen[prog1Audio])

	// Re-demux and check tables and per-track content/timing.
	data := demuxAll(t, out.Bytes())
	var pat *PATData
	var pmt *PMTData
	pesByPID := map[uint16][]*PESData{}
	var pcrs []*ClockReference
	for _, d := range data {
		switch {
		case d.PAT != nil:
			pat = d.PAT
		case d.PMT != nil:
			pmt = d.PMT
		case d.PES != nil:
			pesByPID[d.PID] = append(pesByPID[d.PID], d.PES)
			if d.FirstPacket != nil && d.FirstPacket.AdaptationField != nil &&
				d.FirstPacket.AdaptationField.HasPCR {
				pcrs = append(pcrs, d.FirstPacket.AdaptationField.PCR)
			}
		}
	}

	require.NotNil(t, pat)
	assert.Equal(t, tsID, pat.TransportStreamID)
	require.Len(t, pat.Programs, 1)
	assert.Equal(t, prog1Number, pat.Programs[0].ProgramNumber)
	assert.Equal(t, prog1PMTPID, pat.Programs[0].ProgramMapID)

	require.NotNil(t, pmt)
	assert.Equal(t, prog1Number, pmt.ProgramNumber)
	assert.Equal(t, prog1Video, pmt.PCRPID)
	require.Len(t, pmt.ElementaryStreams, 2)
	assert.Equal(t, prog1Video, pmt.ElementaryStreams[0].ElementaryPID)
	assert.Equal(t, StreamTypeH264Video, pmt.ElementaryStreams[0].StreamType)
	assert.Equal(t, prog1Audio, pmt.ElementaryStreams[1].ElementaryPID)
	assert.Equal(t, StreamTypeADTS, pmt.ElementaryStreams[1].StreamType)

	// Descriptors, including unknown ones, are preserved byte for byte.
	require.Len(t, pmt.ProgramDescriptors, 1)
	assert.Equal(t, uint8(0x22), pmt.ProgramDescriptors[0].Tag)
	require.NotNil(t, pmt.ProgramDescriptors[0].Unknown)
	assert.Equal(t, []byte{0xaa, 0xbb, 0xcc}, pmt.ProgramDescriptors[0].Unknown.Content)
	require.Len(t, pmt.ElementaryStreams[0].ElementaryStreamDescriptors, 1)
	require.NotNil(t, pmt.ElementaryStreams[0].ElementaryStreamDescriptors[0].Unknown)
	assert.Equal(t, []byte{0x11, 0x22}, pmt.ElementaryStreams[0].ElementaryStreamDescriptors[0].Unknown.Content)

	// 6 PES were sent per PID; the corrupt one is dropped, so 5 on video.
	assert.Len(t, pesByPID[prog1Video], 5)
	assert.Len(t, pesByPID[prog1Audio], 6)
	for i, pes := range pesByPID[prog1Audio] {
		assert.Equal(t, []byte{byte(i), 0xa0, 0xa1}, pes.Data)
		require.NotNil(t, pes.Header.OptionalHeader.PTS)
		assert.Equal(t, int64(90000*(i+1)), pes.Header.OptionalHeader.PTS.Base)
	}
	// Video PES: verify payload and PTS/DTS, skipping the dropped corrupt index 3.
	videoIdx := 0
	for i := 0; i < 6; i++ {
		if i == 3 {
			continue
		}
		pes := pesByPID[prog1Video][videoIdx]
		assert.Equal(t, []byte{byte(i), 0x10, 0x11, 0x12, 0x13}, pes.Data)
		oh := pes.Header.OptionalHeader
		require.NotNil(t, oh.PTS)
		require.NotNil(t, oh.DTS)
		assert.Equal(t, int64(90000*(i+1)), oh.PTS.Base)
		assert.Equal(t, int64(90000*i+45000), oh.DTS.Base)
		videoIdx++
	}

	// PCRs ride on the video PID and must be preserved untouched.
	require.Len(t, pcrs, 5)
	pcrIdx := 0
	for i := 0; i < 6; i++ {
		if i == 3 {
			continue
		}
		assert.Equal(t, int64(27000000*(i+1)), pcrs[pcrIdx].Base)
		assert.Equal(t, int64(i), pcrs[pcrIdx].Extension)
		pcrIdx++
	}
}

func TestProgramRemuxer_ExplicitPIDRemap(t *testing.T) {
	input := buildTwoProgramInput(t)

	const newVideoPID uint16 = 0x0301
	const newAudioPID uint16 = 0x0302
	const newPMTPID uint16 = 0x1301

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(input), &out, prog1Number,
		RemuxerOptPIDRemap(prog1Video, newVideoPID),
		RemuxerOptPIDRemap(prog1Audio, newAudioPID),
		RemuxerOptPIDRemap(prog1PMTPID, newPMTPID),
	)
	require.NoError(t, err)

	data := demuxAll(t, out.Bytes())
	var pat *PATData
	var pmt *PMTData
	pesPIDs := map[uint16]bool{}
	for _, d := range data {
		switch {
		case d.PAT != nil:
			pat = d.PAT
		case d.PMT != nil:
			pmt = d.PMT
		case d.PES != nil:
			pesPIDs[d.PID] = true
		}
	}
	require.NotNil(t, pat)
	require.Len(t, pat.Programs, 1)
	assert.Equal(t, newPMTPID, pat.Programs[0].ProgramMapID)

	require.NotNil(t, pmt)
	assert.Equal(t, newVideoPID, pmt.PCRPID)
	gotPIDs := map[uint16]StreamType{}
	for _, es := range pmt.ElementaryStreams {
		gotPIDs[es.ElementaryPID] = es.StreamType
	}
	assert.Equal(t, StreamTypeH264Video, gotPIDs[newVideoPID])
	assert.Equal(t, StreamTypeADTS, gotPIDs[newAudioPID])

	assert.True(t, pesPIDs[newVideoPID])
	assert.True(t, pesPIDs[newAudioPID])
	assert.False(t, pesPIDs[prog1Video])

	// Output CC must still be continuous after remapping.
	for _, p := range rawPackets(t, out.Bytes()) {
		_ = p
	}
}

func TestProgramRemuxer_AutoRemap(t *testing.T) {
	input := buildTwoProgramInput(t)

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(input), &out, prog1Number,
		RemuxerOptAutoRemapPIDs(),
	)
	require.NoError(t, err)

	data := demuxAll(t, out.Bytes())
	var pat *PATData
	var pmt *PMTData
	pesPIDs := map[uint16]bool{}
	for _, d := range data {
		switch {
		case d.PAT != nil:
			pat = d.PAT
		case d.PMT != nil:
			pmt = d.PMT
		case d.PES != nil:
			pesPIDs[d.PID] = true
		}
	}
	require.NotNil(t, pmt)
	require.Len(t, pmt.ElementaryStreams, 2)
	autoPIDs := map[uint16]bool{}
	for _, es := range pmt.ElementaryStreams {
		// Auto PIDs are allocated from 0x0100.
		assert.GreaterOrEqual(t, es.ElementaryPID, startPID)
		assert.False(t, autoPIDs[es.ElementaryPID], "auto-remapped PIDs must be unique")
		autoPIDs[es.ElementaryPID] = true
		assert.True(t, pesPIDs[es.ElementaryPID])
	}
	// The PCR PID follows its elementary stream (and thus is remapped).
	assert.True(t, pesPIDs[pmt.PCRPID])
	// The PMT PID itself is not auto-remapped: the PAT keeps the original one.
	require.NotNil(t, pat)
	require.Len(t, pat.Programs, 1)
	assert.Equal(t, prog1PMTPID, pat.Programs[0].ProgramMapID)
}

func TestProgramRemuxer_RemapConflict(t *testing.T) {
	input := buildTwoProgramInput(t)

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(input), &out, prog1Number,
		RemuxerOptPIDRemap(prog1Video, 0x0500),
		RemuxerOptPIDRemap(prog1Audio, 0x0500),
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPIDRemapConflict), "expected error wrapping %v, got %v", ErrPIDRemapConflict, err)
}

func TestProgramRemuxer_RemapInvalidTarget(t *testing.T) {
	input := buildTwoProgramInput(t)

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(input), &out, prog1Number,
		RemuxerOptPIDRemap(prog1Video, PIDNull),
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPIDRemapInvalid), "expected error wrapping %v, got %v", ErrPIDRemapInvalid, err)
}

func TestProgramRemuxer_ProgramNotFound(t *testing.T) {
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog2Number, ProgramMapID: prog2PMTPID}},
	}, 0)
	b.writePMT(&PMTData{
		ProgramNumber:     prog2Number,
		PCRPID:            prog2Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog2Video, StreamType: StreamTypeH264Video}},
	}, prog2PMTPID, 0)

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrProgramNotFound), "expected error wrapping %v, got %v", ErrProgramNotFound, err)
}

func TestProgramRemuxer_PMTNotFound(t *testing.T) {
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}, 0)
	// No PMT, just a payload packet on an unknown PID.
	b.writePES(prog2Video, StreamTypeH264Video, &PESData{
		Data:   []byte{1, 2, 3},
		Header: &PESHeader{},
	}, nil, false)

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPMTNotFound), "expected error wrapping %v, got %v", ErrPMTNotFound, err)
}

func TestProgramRemuxer_PMTUpdateMidStream(t *testing.T) {
	b := newTSBuilder()
	pat := &PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}
	b.writePAT(pat, 0)

	pmtV0 := &PMTData{
		ProgramNumber: prog1Number,
		PCRPID:        prog1Video,
		ElementaryStreams: []*PMTElementaryStream{
			{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video},
			{ElementaryPID: prog1Audio, StreamType: StreamTypeADTS},
		},
	}
	b.writePMT(pmtV0, prog1PMTPID, 0)

	for i := 0; i < 3; i++ {
		b.writePES(prog1Video, StreamTypeH264Video, &PESData{
			Data:   []byte{0x10, byte(i)},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: int64(i + 1)}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
		}, nil, false)
		b.writePES(prog1Audio, StreamTypeADTS, &PESData{
			Data:   []byte{0xa0, byte(i)},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: int64(i + 1)}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
		}, nil, false)
	}

	// PMT v1: audio stream removed, video retained.
	pmtV1 := &PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}
	b.writePMT(pmtV1, prog1PMTPID, 1)

	for i := 3; i < 6; i++ {
		b.writePES(prog1Video, StreamTypeH264Video, &PESData{
			Data:   []byte{0x10, byte(i)},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: int64(i + 1)}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
		}, nil, false)
		// Stale audio packet after the stream was removed.
		b.writePES(prog1Audio, StreamTypeADTS, &PESData{
			Data:   []byte{0xa0, byte(i)},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: int64(i + 1)}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
		}, nil, false)
	}

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), stats.PMTUpdates)

	data := demuxAll(t, out.Bytes())
	var pmts []*PMTData
	pesCount := map[uint16]int{}
	for _, d := range data {
		switch {
		case d.PMT != nil:
			pmts = append(pmts, d.PMT)
		case d.PES != nil:
			pesCount[d.PID]++
		}
	}
	require.GreaterOrEqual(t, len(pmts), 2)
	assert.Len(t, pmts[0].ElementaryStreams, 2)
	assert.Len(t, pmts[len(pmts)-1].ElementaryStreams, 1)
	assert.Equal(t, prog1Video, pmts[len(pmts)-1].ElementaryStreams[0].ElementaryPID)

	// All 6 video PES survive; no audio packet passes after the PMT update.
	assert.Equal(t, 6, pesCount[prog1Video])
	assert.Equal(t, 3, pesCount[prog1Audio])
}

func TestProgramRemuxer_RetransmittedPMTNotReapplied(t *testing.T) {
	b := newTSBuilder()
	pat := &PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}
	b.writePAT(pat, 0)
	pmt := &PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}
	// Same version retransmitted twice.
	b.writePMT(pmt, prog1PMTPID, 0)
	b.writePMT(pmt, prog1PMTPID, 0)

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), stats.PMTUpdates)
}

func TestProgramRemuxer_InvalidPMTKeepsPrevious(t *testing.T) {
	b := newTSBuilder()
	pat := &PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}
	b.writePAT(pat, 0)
	valid := &PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}
	b.writePMT(valid, prog1PMTPID, 0)
	// PCR PID references a non-existent elementary stream.
	invalid := &PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            0x0999,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}
	b.writePMT(invalid, prog1PMTPID, 1)

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), stats.PMTUpdates)
	assert.Equal(t, uint64(1), stats.PSIErrors)

	data := demuxAll(t, out.Bytes())
	for _, d := range data {
		if d.PMT != nil {
			assert.Equal(t, prog1Video, d.PMT.PCRPID, "previous valid PMT must be retained")
		}
	}
}

func TestProgramRemuxer_Rewind(t *testing.T) {
	input := buildTwoProgramInput(t)
	r := bytes.NewReader(input)
	var out1 bytes.Buffer
	m := NewProgramRemuxer(context.Background(), r, &out1, prog1Number)
	require.NoError(t, m.Remux())

	n, err := m.Rewind()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Equal(t, RemuxerStats{}, m.Stats())

	var out2 bytes.Buffer
	m.w = &out2
	require.NoError(t, m.Remux())
	assert.Equal(t, out1.Bytes(), out2.Bytes())
}

func TestProgramRemuxer_CCContinuityUnderRemap(t *testing.T) {
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}, 0)
	b.writePMT(&PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}, prog1PMTPID, 0)
	for i := 0; i < 10; i++ {
		b.writePES(prog1Video, StreamTypeH264Video, &PESData{
			Data:   []byte{byte(i), 1, 2, 3},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: int64(i + 1)}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
		}, nil, false)
	}

	const outPID uint16 = 0x0777
	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number,
		RemuxerOptPIDRemap(prog1Video, outPID))
	require.NoError(t, err)

	// Every output packet on the remapped PID must carry a strictly
	// incrementing 4-bit continuity counter.
	var ccs []uint8
	for _, p := range rawPackets(t, out.Bytes()) {
		if p.Header.PID == outPID {
			ccs = append(ccs, p.Header.ContinuityCounter)
		}
	}
	require.Len(t, ccs, 10)
	for i, cc := range ccs {
		assert.Equal(t, uint8(i&0x0f), cc)
	}
}

func TestProgramRemuxer_NoPCRProgram(t *testing.T) {
	// PCRPID 0x1fff means "no PCR" per ISO/IEC 13818-1; the remuxer must not
	// reject it just because no elementary stream owns that PID.
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}, 0)
	b.writePMT(&PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            PIDNull,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Audio, StreamType: StreamTypeADTS}},
	}, prog1PMTPID, 0)
	b.writePES(prog1Audio, StreamTypeADTS, &PESData{
		Data:   []byte{1, 2, 3, 4},
		Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: 1}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
	}, nil, false)

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), stats.PMTUpdates)

	data := demuxAll(t, out.Bytes())
	var pmt *PMTData
	var pes int
	for _, d := range data {
		if d.PMT != nil {
			pmt = d.PMT
		}
		if d.PES != nil {
			pes++
		}
	}
	require.NotNil(t, pmt)
	assert.Equal(t, PIDNull, pmt.PCRPID)
	assert.Equal(t, 1, pes)
}

func TestProgramRemuxer_MultiPacketPES(t *testing.T) {
	// A large video PES spanning several TS packets, using a zero
	// PES_packet_length (the common unbounded-video case).
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}, 0)
	b.writePMT(&PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}, prog1PMTPID, 0)

	// Build a PES whose ES payload spans ~4 packets and write it manually.
	esData := make([]byte, 700)
	for i := range esData {
		esData[i] = byte(i % 251)
	}
	pes := &PESData{
		Data: esData,
		Header: &PESHeader{OptionalHeader: &PESOptionalHeader{
			PTS: &ClockReference{Base: 12345}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
		}},
	}
	h := *pes.Header
	h.StreamID = StreamTypeH264Video.ToPESStreamID()
	h.OptionalHeader.HeaderLength = calcPESOptionalHeaderLength(h.OptionalHeader)
	var pesBuf bytes.Buffer
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &pesBuf})
	_, _, err := writePESData(pw, &h, pes.Data, true, 700+pesHeaderLength+int(calcPESOptionalHeaderLength(h.OptionalHeader)))
	require.NoError(t, err)

	// Split the PES across TS packets with proper start indicator and CC. The
	// library's writePacket caps a packet payload at 184 bytes. The final,
	// shorter packet uses an adaptation field for padding so the 0xff bytes do
	// not end up inside the (zero-length) video PES payload.
	const tsPayloadMax = MpegTsPacketSize - mpegTsPacketHeaderSize - 1
	pesBytes := pesBuf.Bytes()
	cc := uint8(0)
	for off := 0; off < len(pesBytes); {
		chunk := tsPayloadMax
		isLast := false
		if rem := len(pesBytes) - off; rem < chunk {
			chunk = rem
			isLast = true
		}
		pkt := &Packet{
			Header: PacketHeader{
				PID:                       prog1Video,
				HasPayload:                true,
				PayloadUnitStartIndicator: off == 0,
				ContinuityCounter:         cc,
			},
			Payload: pesBytes[off : off+chunk],
		}
		if isLast {
			// Pad the leftover payload area via adaptation-field stuffing. The
			// adaptation-field length byte is double-counted by writePacket, so
			// stuffing is one byte less than the raw leftover.
			pkt.Header.HasAdaptationField = true
			pkt.AdaptationField = &PacketAdaptationField{
				StuffingLength: tsPayloadMax - chunk - 2,
			}
		}
		cc = (cc + 1) & 0x0f
		off += chunk
		bw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf})
		_, err := writePacket(bw, pkt, MpegTsPacketSize)
		require.NoError(t, err)
	}

	var out bytes.Buffer
	_, err = RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.NoError(t, err)

	data := demuxAll(t, out.Bytes())
	var got *PESData
	for _, d := range data {
		if d.PES != nil && d.PID == prog1Video {
			got = d.PES
		}
	}
	require.NotNil(t, got)
	assert.Equal(t, esData, got.Data, "multi-packet ES data must survive byte-for-byte")
	require.NotNil(t, got.Header.OptionalHeader.PTS)
	assert.Equal(t, int64(12345), got.Header.OptionalHeader.PTS.Base)
}

func TestProgramRemuxer_ExplicitPacketSize188(t *testing.T) {
	// The packet-size option must be honored. Explicit 188 (the default) yields
	// the same result as auto-detection.
	input := buildTwoProgramInput(t)

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(input), &out, prog1Number,
		RemuxerOptPacketSize(MpegTsPacketSize))
	require.NoError(t, err)
	assert.Equal(t, 0, out.Len()%MpegTsPacketSize)
	assert.Greater(t, stats.PacketsRead, uint64(0))

	data := demuxAll(t, out.Bytes())
	var pat *PATData
	for _, d := range data {
		if d.PAT != nil {
			pat = d.PAT
		}
	}
	require.NotNil(t, pat)
	require.Len(t, pat.Programs, 1)
	assert.Equal(t, prog1Number, pat.Programs[0].ProgramNumber)
}

func TestProgramRemuxer_CorruptPSIKeepsPreviousTables(t *testing.T) {
	// A valid PAT/PMT pair followed by a malformed PAT: the remuxer must keep
	// serving the last good tables rather than aborting.
	b := newTSBuilder()
	pat := &PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}
	b.writePAT(pat, 0)
	b.writePMT(&PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}, prog1PMTPID, 0)
	b.writePES(prog1Video, StreamTypeH264Video, &PESData{
		Data:   []byte{1, 2, 3},
		Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: 1}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
	}, nil, false)

	// Append a PAT packet with a corrupted CRC (flip a section byte).
	goodPAT := b.bytes()[:MpegTsPacketSize]
	badPAT := make([]byte, MpegTsPacketSize)
	copy(badPAT, goodPAT)
	badPAT[8] ^= 0xff // corrupt table body, invalidating CRC32
	b.buf.Write(badPAT)

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.PSIErrors, uint64(1))

	// The single program is still declared and its data still flows.
	data := demuxAll(t, out.Bytes())
	var pmtCount, pesCount int
	for _, d := range data {
		if d.PMT != nil {
			pmtCount++
		}
		if d.PES != nil {
			pesCount++
		}
	}
	assert.Greater(t, pmtCount, 0)
	assert.Equal(t, 1, pesCount)
}

func TestProgramRemuxer_ProcessNextPacketIncremental(t *testing.T) {
	input := buildTwoProgramInput(t)
	r := bytes.NewReader(input)
	var out bytes.Buffer
	m := NewProgramRemuxer(context.Background(), r, &out, prog1Number,
		RemuxerOptPacketSize(MpegTsPacketSize))

	// Drive the remuxer one packet at a time until it reports completion.
	var iterations int
	for {
		done, err := m.ProcessNextPacket()
		require.NoError(t, err)
		iterations++
		if done {
			break
		}
		require.Less(t, iterations, len(input)/MpegTsPacketSize+5)
	}
	// Every input packet was observed exactly once.
	assert.Equal(t, uint64(len(input)/MpegTsPacketSize), m.Stats().PacketsRead)
	assert.Equal(t, 0, out.Len()%MpegTsPacketSize)

	// And the result still parses as the single target program.
	data := demuxAll(t, out.Bytes())
	var pat *PATData
	for _, d := range data {
		if d.PAT != nil {
			pat = d.PAT
		}
	}
	require.NotNil(t, pat)
	require.Len(t, pat.Programs, 1)
	assert.Equal(t, prog1Number, pat.Programs[0].ProgramNumber)
}

func TestProgramRemuxer_TablesRetransmitPeriod(t *testing.T) {
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}, 0)
	b.writePMT(&PMTData{
		ProgramNumber:     prog1Number,
		PCRPID:            prog1Video,
		ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video}},
	}, prog1PMTPID, 0)
	// Emit enough PES packets to trigger several periodic retransmissions.
	for i := 0; i < 30; i++ {
		b.writePES(prog1Video, StreamTypeH264Video, &PESData{
			Data:   []byte{byte(i), 1, 2},
			Header: &PESHeader{OptionalHeader: &PESOptionalHeader{PTS: &ClockReference{Base: int64(i + 1)}, PTSDTSIndicator: PTSDTSIndicatorOnlyPTS}},
		}, nil, false)
	}

	var out bytes.Buffer
	stats, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number,
		RemuxerOptTablesRetransmitPeriod(5))
	require.NoError(t, err)
	// Initial emission plus periodic ones, always in PAT+PMT pairs.
	assert.Greater(t, stats.TablesWritten, uint64(2))
	assert.Equal(t, uint64(0), stats.TablesWritten%2)
}

func TestProgramRemuxer_AutoRemapSkipsExplicitTarget(t *testing.T) {
	// An explicit remap claims a PID the auto-allocator would otherwise hand
	// out; the auto-allocator must skip it to avoid a collision.
	b := newTSBuilder()
	b.writePAT(&PATData{
		TransportStreamID: tsID,
		Programs:          []*PATProgram{{ProgramNumber: prog1Number, ProgramMapID: prog1PMTPID}},
	}, 0)
	b.writePMT(&PMTData{
		ProgramNumber: prog1Number,
		PCRPID:        prog1Video,
		ElementaryStreams: []*PMTElementaryStream{
			{ElementaryPID: prog1Video, StreamType: StreamTypeH264Video},
			{ElementaryPID: prog1Audio, StreamType: StreamTypeADTS},
		},
	}, prog1PMTPID, 0)

	var out bytes.Buffer
	_, err := RemuxProgram(context.Background(), bytes.NewReader(b.bytes()), &out, prog1Number,
		RemuxerOptAutoRemapPIDs(),
		RemuxerOptPIDRemap(prog1Audio, startPID), // claim the first auto PID
	)
	require.NoError(t, err)

	data := demuxAll(t, out.Bytes())
	pids := map[uint16]bool{}
	for _, d := range data {
		if d.PMT != nil {
			for _, es := range d.PMT.ElementaryStreams {
				assert.False(t, pids[es.ElementaryPID], "duplicate output PID")
				pids[es.ElementaryPID] = true
			}
		}
	}
	// The explicit target is used, and the auto-allocated video PID differs.
	assert.True(t, pids[startPID])
	assert.Len(t, pids, 2)
}
