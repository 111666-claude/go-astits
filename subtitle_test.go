package astits

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture constants, must match internal/cmd/dvb-subtitle-fixture.
const (
	fixturePath     = "testdata/ts/dvb_subtitles.ts"
	fixtureSubPID1  = uint16(0x0400)
	fixtureSubPID2  = uint16(0x0500)
	fixturePMT1PID  = uint16(0x1000)
	fixturePMT2PID  = uint16(0x1001)
	fixtureProgram1 = uint16(0x0001)
	fixtureProgram2 = uint16(0x0002)
	fixturePageEng  = uint16(0x0200)
	fixturePageDeu  = uint16(0x0300)
	fixtureAncPage  = uint16(0x0100)
	fixturePTS1     = int64(0x123456789)
	fixturePTS2     = int64(0x123456fff)
	fixturePTS3     = int64(0x03456789a)
)

// openFixtureExtractor runs the extractor over the fixed two-page multi-segment
// fixture with the given options.
func openFixtureExtractor(t *testing.T, opts ...DVBSubtitleExtractorOpt) (*DVBSubtitleExtractor, *Demuxer) {
	t.Helper()
	f, err := os.Open(fixturePath)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	dmx := NewDemuxer(context.Background(), f, DemuxerOptPacketSize(188))
	return NewDVBSubtitleExtractor(dmx, opts...), dmx
}

func collectSegments(t *testing.T, e *DVBSubtitleExtractor) []*DVBSubtitleSegment {
	t.Helper()
	var out []*DVBSubtitleSegment
	for {
		s, err := e.NextSegment()
		if errors.Is(err, ErrNoMorePackets) {
			return out
		}
		require.NoError(t, err)
		out = append(out, s)
	}
}

func TestDVBSubtitleFixtureFiltersByLanguageAndPage(t *testing.T) {
	// Select the English stream of program 1.
	e, _ := openFixtureExtractor(t,
		DVBSubtitleOptLanguage([]byte("eng")),
		DVBSubtitleOptCompositionPage(fixturePageEng),
	)

	segments := collectSegments(t, e)

	// Once the PMT has been processed the active PID is the English
	// subtitling PID of program 1.
	activePID, ok := e.ActivePID()
	require.True(t, ok)
	assert.Equal(t, fixtureSubPID1, activePID)

	// PES #1 emits 3 composition-page segments; the ancillary CLUT (page
	// 0x0100) is filtered out. PES #2 emits 2 more segments.
	require.Len(t, segments, 5)

	want := []struct {
		typ  DVBSubtitleSegmentType
		body byte
		n    int
		pts  int64
	}{
		{DVBSubtitleSegmentTypePCS, 0xa1, 60, fixturePTS1},
		{DVBSubtitleSegmentTypeRCS, 0xa2, 60, fixturePTS1},
		{DVBSubtitleSegmentTypeObjectFirst, 0xa3, 120, fixturePTS1},
		{DVBSubtitleSegmentTypeObjectLast, 0xb1, 30, fixturePTS2},
		{DVBSubtitleSegmentTypeEnd, 0xb2, 10, fixturePTS2},
	}
	for i, w := range want {
		s := segments[i]
		assert.Equal(t, DVBSubtitleSegmentType(w.typ), s.Type, "segment %d type", i)
		assert.Equal(t, fixturePageEng, s.CompositionPageID, "segment %d page", i)
		assert.Equal(t, fixtureAncPage, s.AncillaryPageID, "segment %d ancillary page", i)
		assert.Equal(t, []byte("eng"), s.Language, "segment %d language", i)
		assert.Equal(t, fixtureSubPID1, s.PID, "segment %d pid", i)
		assert.Equal(t, fixtureProgram1, s.ProgramNumber, "segment %d program", i)
		assert.Equal(t, w.pts, s.PTS.Base, "segment %d pts", i)
		assert.Equal(t, i, s.SegmentNumber, "segment %d number", i)
		// Exact raw segment payload bytes.
		require.Len(t, s.Data, w.n, "segment %d data length", i)
		for _, b := range s.Data {
			assert.Equal(t, w.body, b, "segment %d data byte", i)
		}
	}
}

func TestDVBSubtitleFixtureMultiProgramIsolation(t *testing.T) {
	// Select the German stream of program 2.
	e, _ := openFixtureExtractor(t,
		DVBSubtitleOptLanguage([]byte("deu")),
		DVBSubtitleOptCompositionPage(fixturePageDeu),
	)

	segments := collectSegments(t, e)

	activePID, ok := e.ActivePID()
	require.True(t, ok)
	assert.Equal(t, fixtureSubPID2, activePID)

	// Two composition-page segments; ancillary CLUT filtered out.
	require.Len(t, segments, 2)
	assert.Equal(t, DVBSubtitleSegmentTypePCS, segments[0].Type)
	assert.Equal(t, DVBSubtitleSegmentTypeRCS, segments[1].Type)
	for _, s := range segments {
		assert.Equal(t, fixtureSubPID2, s.PID)
		assert.Equal(t, fixtureProgram2, s.ProgramNumber)
		assert.Equal(t, []byte("deu"), s.Language)
		assert.Equal(t, fixturePageDeu, s.CompositionPageID)
		assert.Equal(t, fixturePTS3, s.PTS.Base)
	}
	assert.Equal(t, 0, segments[0].SegmentNumber)
	assert.Equal(t, 1, segments[1].SegmentNumber)
}

func TestDVBSubtitleFixtureNoLanguageFilter(t *testing.T) {
	// Without a language filter, the first registered subtitling stream
	// (program 1, English) is selected deterministically.
	e, _ := openFixtureExtractor(t)
	segments := collectSegments(t, e)
	pid, ok := e.ActivePID()
	require.True(t, ok)
	assert.Equal(t, fixtureSubPID1, pid)
	assert.Len(t, segments, 5)
}

func TestDVBSubtitleFixturePageFilterRejectsOtherPage(t *testing.T) {
	// The English stream never uses composition page 0x0300: nothing matches.
	e, _ := openFixtureExtractor(t,
		DVBSubtitleOptLanguage([]byte("eng")),
		DVBSubtitleOptCompositionPage(0x0300),
	)
	_, ok := e.ActivePID()
	assert.False(t, ok)
	assert.Empty(t, collectSegments(t, e))
}

func TestDVBSubtitleEvents(t *testing.T) {
	e, _ := openFixtureExtractor(t,
		DVBSubtitleOptLanguage([]byte("eng")),
		DVBSubtitleOptCompositionPage(fixturePageEng),
	)
	var segments, pidChanges, discontinuities int
	for {
		ev, err := e.NextEvent()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		switch ev.Type {
		case DVBSubtitleEventTypeSegment:
			segments++
		case DVBSubtitleEventTypePIDChange:
			pidChanges++
		case DVBSubtitleEventTypeDiscontinuity:
			discontinuities++
		}
	}
	assert.Equal(t, 5, segments)
	assert.Equal(t, 0, pidChanges)
	assert.Equal(t, 0, discontinuities)
}

// ---- Synthetic error/edge-case streams ----

const (
	synthPMTPID  = uint16(0x1000)
	synthSubPID  = uint16(0x0400)
	synthProgram = uint16(1)
	synthPage    = uint16(0x0200)
)

// synthBuilder assures a valid TS stream with one program and one subtitling
// PID. PSI packets are hand-authored while PES packets are packetized through
// the library Muxer so their transport packet layout is guaranteed to round
// trip through the Demuxer under test.
type synthBuilder struct {
	buf  bytes.Buffer
	cc   map[uint16]uint8
	muxs map[uint16]*Muxer
}

func newSynthBuilder(t *testing.T) *synthBuilder {
	t.Helper()
	b := &synthBuilder{
		cc:   map[uint16]uint8{},
		muxs: map[uint16]*Muxer{},
	}
	b.writePSI(PIDPAT, buildPATPID(t, synthPMTPID))
	b.writePSI(synthPMTPID, buildSubtitlePMT(t, synthSubPID))
	return b
}

// muxerFor lazily creates a table-less Muxer bound to the shared buffer for
// the given subtitling PID.
func (b *synthBuilder) muxerFor(pid uint16) *Muxer {
	if mx, ok := b.muxs[pid]; ok {
		return mx
	}
	mx := NewMuxer(context.Background(), &b.buf,
		MuxerOptTablesRetransmitPeriod(1<<30),
		WithPMTPID(synthPMTPID))
	if err := mx.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: pid,
		StreamType:    StreamTypePrivateData,
	}); err != nil {
		panic(err)
	}
	mx.SetPCRPID(pid)
	b.muxs[pid] = mx
	return mx
}

func buildPATPID(t *testing.T, pmtPID uint16) []byte {
	pat := &PATData{
		TransportStreamID: 1,
		Programs:          []*PATProgram{{ProgramNumber: synthProgram, ProgramMapID: pmtPID}},
	}
	return marshalPSISection(t, PSITableIDPAT, 1, func(w *astikit.BitsWriter) {
		_, err := writePATSection(w, pat)
		require.NoError(t, err)
	}, calcPATSectionLength(pat))
}

func buildSubtitlePMT(t *testing.T, subPID uint16) []byte {
	pmt := &PMTData{
		ProgramNumber: synthProgram,
		PCRPID:        subPID,
		ElementaryStreams: []*PMTElementaryStream{{
			ElementaryPID: subPID,
			StreamType:    StreamTypePrivateData,
			ElementaryStreamDescriptors: []*Descriptor{{
				Tag:    DescriptorTagSubtitling,
				Length: 8,
				Subtitling: &DescriptorSubtitling{Items: []*DescriptorSubtitlingItem{{
					Language:          []byte("eng"),
					Type:              SubtitlingTypeDVBBitmap,
					CompositionPageID: synthPage,
					AncillaryPageID:   0x0100,
				}}},
			}},
		}},
	}
	return marshalPSISection(t, PSITableIDPMT, synthProgram, func(w *astikit.BitsWriter) {
		_, err := writePMTSection(w, pmt)
		require.NoError(t, err)
	}, calcPMTSectionLength(pmt))
}

// marshalPSISection wraps a section body in a PSI section with CRC and a
// pointer-field payload.
func marshalPSISection(t *testing.T, tableID PSITableID, tableIDExt uint16, writeBody func(*astikit.BitsWriter), bodyLen uint16) []byte {
	t.Helper()
	var body bytes.Buffer
	bw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &body})
	writeBody(bw)

	// section_length = 5 syntax bytes + body + 4 CRC
	sectionLength := uint16(5) + bodyLen + 4
	header := []byte{
		byte(tableID),
		0xb0 | byte(sectionLength>>8), byte(sectionLength),
		byte(tableIDExt >> 8), byte(tableIDExt),
		0xc1, // version 0, current/next
		0x00, 0x00,
	}
	crcInput := append(append([]byte{}, header...), body.Bytes()...)
	crc := computeCRC32(crcInput)
	section := append(crcInput,
		byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc),
	)
	return append([]byte{0x00}, section...) // pointer field
}

func (b *synthBuilder) writePSI(pid uint16, payload []byte) {
	b.writeRawPacket(pid, true, payload)
}

// writePES appends a subtitling PES packet (optionally without PTS) through
// the library Muxer. When withPTS is false an optional header without any
// PTS/DTS indicator is produced so the extractor reports a missing PTS.
func (b *synthBuilder) writePES(pid uint16, withPTS bool, pts int64, subtitlePayload []byte) {
	h := &PESHeader{StreamID: 0xbd}
	if withPTS {
		h.OptionalHeader = &PESOptionalHeader{
			PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
			PTS:             newClockReference(pts, 0),
		}
	} else {
		h.OptionalHeader = &PESOptionalHeader{}
	}
	if _, err := b.muxerFor(pid).WriteData(&MuxerData{
		PID: pid,
		PES: &PESData{Header: h, Data: subtitlePayload},
	}); err != nil {
		panic(err)
	}
}

func subtitlePayload(segments ...[]byte) []byte {
	out := []byte{dvbSubtitlingDataIdentifier}
	for _, s := range segments {
		out = append(out, s...)
	}
	return out
}

func synthSegment(segmentType DVBSubtitleSegmentType, pageID uint16, body []byte) []byte {
	return append([]byte{
		dvbSubtitlingSegmentSyncByte,
		byte(segmentType),
		byte(pageID >> 8), byte(pageID & 0xff),
		byte(len(body) >> 8), byte(len(body)),
	}, body...)
}

func (b *synthBuilder) writeRawPacket(pid uint16, start bool, payload []byte) {
	b.writeRawPacketWithStuffing(pid, start, payload, 0)
}

func (b *synthBuilder) writeRawPacketWithStuffing(pid uint16, start bool, payload []byte, stuffing int) {
	p := make([]byte, MpegTsPacketSize)
	for i := range p {
		p[i] = 0xff
	}
	p[0] = syncByte
	afControl := uint8(0x1)
	offset := 4
	if stuffing > 0 {
		afControl = 0x3
		p[offset] = byte(1 + stuffing) // AF length = flags byte + stuffing
		offset++
		p[offset] = 0x00
		offset++
		offset += stuffing
	}
	cc := b.cc[pid]
	if start {
		p[1] = 0x40 | byte(pid>>8)
	} else {
		p[1] = byte(pid >> 8)
	}
	p[2] = byte(pid)
	p[3] = afControl<<4 | cc
	copy(p[offset:], payload)
	b.cc[pid] = (cc + 1) & 0x0f
	b.buf.Write(p)
}

func (b *synthBuilder) extractor(t *testing.T) *DVBSubtitleExtractor {
	dmx := NewDemuxer(context.Background(), bytes.NewReader(b.buf.Bytes()), DemuxerOptPacketSize(188))
	return NewDVBSubtitleExtractor(dmx)
}

// writeDiscontinuityPacket emits a packet whose continuity counter skips by
// two relative to the last packet actually written for the PID (including
// Muxer-produced packets), forcing the packet pool to detect a break. The
// owning Muxer's counter is then synchronized to the gapped packet so the
// following, well-formed PES continues the sequence instead of triggering a
// second spurious break.
func (b *synthBuilder) writeDiscontinuityPacket(pid uint16) {
	lastCC, found := b.lastContinuityCounter(pid)
	if !found {
		lastCC = 0x0f
	}
	// Skip one counter value to create a genuine gap.
	gapCC := (lastCC + 2) & 0x0f
	payload := subtitlePayload(synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0xdd}))
	p := make([]byte, MpegTsPacketSize)
	for i := range p {
		p[i] = 0xff
	}
	p[0] = syncByte
	p[1] = 0x40 | byte(pid>>8) // payload unit start indicator
	p[2] = byte(pid)
	p[3] = 0x10 | gapCC // payload only, gapped continuity counter
	copy(p[4:], payload)
	b.buf.Write(p)
	b.cc[pid] = (gapCC + 1) & 0x0f

	// Synchronize the Muxer counter to the packet following the gap so any
	// later Muxer-produced packet continues from gapCC + 1. The muxer reads
	// inc() before emitting each packet, so the stored value is gapCC.
	if mx, ok := b.muxs[pid]; ok {
		if ctx, ok := mx.esContexts[uint32(pid)]; ok {
			ctx.cc = wrappingCounter{value: int(gapCC), wrapAt: 0x0f}
		}
	}
}

// lastContinuityCounter scans the buffered TS packets and returns the
// continuity counter of the last packet matching pid.
func (b *synthBuilder) lastContinuityCounter(pid uint16) (uint8, bool) {
	raw := b.buf.Bytes()
	var last uint8
	found := false
	for off := 0; off+MpegTsPacketSize <= len(raw); off += MpegTsPacketSize {
		pkt := raw[off : off+MpegTsPacketSize]
		if pkt[0] != syncByte {
			continue
		}
		pktPID := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pktPID == pid {
			last = pkt[3] & 0x0f
			found = true
		}
	}
	return last, found
}

func TestDVBSubtitleBadSegmentSyncByte(t *testing.T) {
	b := newSynthBuilder(t)
	// data_identifier is valid, but the segment starts with 0x01 instead of 0x0f.
	bad := append([]byte{dvbSubtitlingDataIdentifier, 0x01, byte(DVBSubtitleSegmentTypePCS)}, make([]byte, 10)...)
	b.writePES(synthSubPID, true, 90000, bad)

	e := b.extractor(t)
	_, err := e.NextSegment()
	require.True(t, errors.Is(err, ErrDVBSubtitleSyncByte), err)
}

func TestDVBSubtitleBadDataIdentifier(t *testing.T) {
	b := newSynthBuilder(t)
	bad := append([]byte{0x21}, synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0xaa})...)
	b.writePES(synthSubPID, true, 90000, bad)

	e := b.extractor(t)
	_, err := e.NextSegment()
	require.True(t, errors.Is(err, ErrDVBSubtitleSyncByte), err)
}

func TestDVBSubtitleSegmentLengthOutOfBounds(t *testing.T) {
	b := newSynthBuilder(t)
	// segment_length claims 200 bytes but only 3 follow.
	truncated := []byte{
		dvbSubtitlingDataIdentifier,
		dvbSubtitlingSegmentSyncByte,
		byte(DVBSubtitleSegmentTypePCS),
		byte(synthPage >> 8), byte(synthPage & 0xff),
		0x00, 0xc8, // length 200
		1, 2, 3,
	}
	b.writePES(synthSubPID, true, 90000, truncated)

	e := b.extractor(t)
	_, err := e.NextSegment()
	require.True(t, errors.Is(err, ErrDVBSubtitleSegmentLength), err)
}

func TestDVBSubtitlePTSMissing(t *testing.T) {
	b := newSynthBuilder(t)
	b.writePES(synthSubPID, false, 0,
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0xaa})))

	e := b.extractor(t)
	_, err := e.NextSegment()
	require.True(t, errors.Is(err, ErrDVBSubtitlePTSMissing), err)
}

func TestDVBSubtitlePIDChange(t *testing.T) {
	b := newSynthBuilder(t)
	// First segment on the originally advertised PID.
	b.writePES(synthSubPID, true, 90000,
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0x01})))

	// Rebuild the PMT moving the same language/page to a new PID.
	const newPID = uint16(0x0401)
	b.writePSI(synthPMTPID, buildSubtitlePMTAtPID(t, synthSubPID, newPID))
	b.cc[newPID] = 0
	b.writePES(newPID, true, 180000,
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypeRCS, synthPage, []byte{0x02})))

	dmx := NewDemuxer(context.Background(), bytes.NewReader(b.buf.Bytes()), DemuxerOptPacketSize(188))
	e := NewDVBSubtitleExtractor(dmx)

	// First event is the segment on the old PID.
	ev, err := e.NextEvent()
	require.NoError(t, err)
	require.Equal(t, DVBSubtitleEventTypeSegment, ev.Type)
	assert.Equal(t, synthSubPID, ev.Segment.PID)

	// Then a PID change event.
	ev, err = e.NextEvent()
	require.NoError(t, err)
	require.Equal(t, DVBSubtitleEventTypePIDChange, ev.Type)
	assert.Equal(t, synthSubPID, ev.PIDChange.OldPID)
	assert.Equal(t, newPID, ev.PIDChange.NewPID)
	assert.Equal(t, []byte("eng"), ev.PIDChange.Language)
	assert.Equal(t, synthPage, ev.PIDChange.CompositionPageID)

	// Then the segment on the new PID.
	ev, err = e.NextEvent()
	require.NoError(t, err)
	require.Equal(t, DVBSubtitleEventTypeSegment, ev.Type)
	assert.Equal(t, newPID, ev.Segment.PID)
	assert.Equal(t, int64(180000), ev.Segment.PTS.Base)

	pid, ok := e.ActivePID()
	require.True(t, ok)
	assert.Equal(t, newPID, pid)
}

func buildSubtitlePMTAtPID(t *testing.T, originalPID, newSubPID uint16) []byte {
	// Keep PCR PID on original PID, move subtitling ES to newSubPID.
	pmt := &PMTData{
		ProgramNumber: synthProgram,
		PCRPID:        originalPID,
		ElementaryStreams: []*PMTElementaryStream{{
			ElementaryPID: newSubPID,
			StreamType:    StreamTypePrivateData,
			ElementaryStreamDescriptors: []*Descriptor{{
				Tag:    DescriptorTagSubtitling,
				Length: 8,
				Subtitling: &DescriptorSubtitling{Items: []*DescriptorSubtitlingItem{{
					Language:          []byte("eng"),
					Type:              SubtitlingTypeDVBBitmap,
					CompositionPageID: synthPage,
					AncillaryPageID:   0x0100,
				}}},
			}},
		}},
	}
	return marshalPSISection(t, PSITableIDPMT, synthProgram, func(w *astikit.BitsWriter) {
		_, err := writePMTSection(w, pmt)
		require.NoError(t, err)
	}, calcPMTSectionLength(pmt))
}

func TestDVBSubtitleContinuityBreak(t *testing.T) {
	b := newSynthBuilder(t)
	b.writePES(synthSubPID, true, 90000,
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0x01})))
	b.writeDiscontinuityPacket(synthSubPID)
	// A well-formed PES after the break, so the stream continues afterwards.
	b.writePES(synthSubPID, true, 270000,
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypeRCS, synthPage, []byte{0x02})))

	var discontinuities int
	e := b.extractor(t)
	for {
		ev, err := e.NextEvent()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		if ev.Type == DVBSubtitleEventTypeDiscontinuity {
			discontinuities++
			assert.Equal(t, synthSubPID, ev.Discontinuity.PID)
		}
	}
	assert.Equal(t, 1, discontinuities)
}

func TestDVBSubtitleChainedDiscontinuityHandler(t *testing.T) {
	b := newSynthBuilder(t)
	b.writePES(synthSubPID, true, 90000,
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0x01})))
	b.writeDiscontinuityPacket(synthSubPID)

	var externalCalls int
	dmx := NewDemuxer(context.Background(), bytes.NewReader(b.buf.Bytes()), DemuxerOptPacketSize(188),
		DemuxerOptDiscontinuityHandler(func(pid uint16) { externalCalls++ }))
	NewDVBSubtitleExtractor(dmx)

	for {
		_, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
	}
	assert.Equal(t, 1, externalCalls)
}

func TestDVBSubtitleMultipleSegmentsInOnePES(t *testing.T) {
	b := newSynthBuilder(t)
	payload := subtitlePayload(
		synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0x01, 0x02}),
		synthSegment(DVBSubtitleSegmentTypeRCS, synthPage, []byte{0x03}),
		synthSegment(DVBSubtitleSegmentTypeEnd, synthPage, nil),
	)
	b.writePES(synthSubPID, true, 540000, payload)

	segments := collectSegments(t, b.extractor(t))
	require.Len(t, segments, 3)
	assert.Equal(t, DVBSubtitleSegmentTypePCS, segments[0].Type)
	assert.Equal(t, []byte{0x01, 0x02}, segments[0].Data)
	assert.Equal(t, 0, segments[0].SegmentNumber)
	assert.Equal(t, DVBSubtitleSegmentTypeRCS, segments[1].Type)
	assert.Equal(t, []byte{0x03}, segments[1].Data)
	assert.Equal(t, 1, segments[1].SegmentNumber)
	assert.Equal(t, DVBSubtitleSegmentTypeEnd, segments[2].Type)
	assert.Empty(t, segments[2].Data)
	assert.Equal(t, 2, segments[2].SegmentNumber)
	for _, s := range segments {
		assert.Equal(t, int64(540000), s.PTS.Base)
	}
}

func TestDVBSubtitleStuffingBytesIgnored(t *testing.T) {
	b := newSynthBuilder(t)
	// Trailing 0xff stuffing after a valid segment.
	payload := append(
		subtitlePayload(synthSegment(DVBSubtitleSegmentTypePCS, synthPage, []byte{0x09})),
		0xff, 0xff,
	)
	b.writePES(synthSubPID, true, 630000, payload)
	segments := collectSegments(t, b.extractor(t))
	require.Len(t, segments, 1)
	assert.Equal(t, []byte{0x09}, segments[0].Data)
}

func TestDVBSubtitleSegmentTypeString(t *testing.T) {
	assert.Equal(t, "page_composition", DVBSubtitleSegmentType(DVBSubtitleSegmentTypePCS).String())
	assert.Equal(t, "end_of_display_set", DVBSubtitleSegmentType(DVBSubtitleSegmentTypeEnd).String())
	assert.Contains(t, DVBSubtitleSegmentType(0x55).String(), "unknown")
}
