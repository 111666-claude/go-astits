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

// ---- Deterministic multi-PID TS fixture ------------------------------------

// tsFixtureBuilder assembles a transport stream out of independent, per-PID
// continuity counters. It produces both 188- and 192-byte packets.
type tsFixtureBuilder struct {
	packetSize int
	buf        bytes.Buffer
	cc         map[uint16]uint8
}

func newTSFixtureBuilder(packetSize int) *tsFixtureBuilder {
	return &tsFixtureBuilder{
		packetSize: packetSize,
		cc:         make(map[uint16]uint8),
	}
}

func (b *tsFixtureBuilder) bytes() []byte { return b.buf.Bytes() }

// nextCC returns the current continuity counter for a PID and advances it.
func (b *tsFixtureBuilder) nextCC(pid uint16) uint8 {
	cc := b.cc[pid]
	b.cc[pid] = (cc + 1) & 0x0f
	return cc
}

// peekCC returns the counter the next packet of the PID will carry.
func (b *tsFixtureBuilder) peekCC(pid uint16) uint8 { return b.cc[pid] }

// writePES writes one PES-carrying packet with the requested PTS and optional
// adaptation field (used for PCRs).
func (b *tsFixtureBuilder) writePES(t *testing.T, pid uint16, pts int64, withDTS bool, af *PacketAdaptationField) {
	pes := fixturePES(pts, withDTS)
	hasAF := af != nil
	h := PacketHeader{
		ContinuityCounter:         b.nextCC(pid),
		HasAdaptationField:        hasAF,
		HasPayload:                true,
		PayloadUnitStartIndicator: true,
		PID:                       pid,
	}
	b.writePacket(t, h, af, fixturePESBytes(t, pes, af))
}

// writeRaw writes a payload-only packet with an explicitly forced continuity counter,
// allowing the tests to simulate loss, duplication and reserved header values.
func (b *tsFixtureBuilder) writeRaw(t *testing.T, pid, cc uint16, payload []byte, hasPayload, hasAF bool, af *PacketAdaptationField, tei bool) {
	h := PacketHeader{
		ContinuityCounter:         uint8(cc & 0x0f),
		HasAdaptationField:        hasAF,
		HasPayload:                hasPayload,
		PayloadUnitStartIndicator: hasPayload,
		PID:                       pid,
		TransportErrorIndicator:   tei,
	}
	b.writePacket(t, h, af, payload)
}

// writeNull writes a null packet; its continuity counter is irrelevant.
func (b *tsFixtureBuilder) writeNull(t *testing.T) {
	b.writePacket(t, PacketHeader{PID: PIDNull, HasPayload: true, ContinuityCounter: b.peekCC(0)}, nil, bytes.Repeat([]byte{0xff}, MpegTsPacketSize-4))
}

func (b *tsFixtureBuilder) writePacket(t *testing.T, h PacketHeader, af *PacketAdaptationField, payload []byte) {
	// First serialize a standard 188-byte packet.
	var pkt bytes.Buffer
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &pkt})
	n, err := writePacket(pw, &Packet{Header: h, AdaptationField: af, Payload: payload}, MpegTsPacketSize)
	require.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, n)

	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf})
	if b.packetSize == 192 {
		// The library's 192-byte convention (see packet() in packet_test.go): sync byte,
		// 4-byte prefix, then the remaining 187 bytes of the 188-byte packet. The parser
		// validates the leading sync and seeks past the prefix before reading the header.
		require.NoError(t, w.Write(pkt.Bytes()[0])) // sync byte
		require.NoError(t, w.Write([]byte("test"))) // 4-byte prefix
		require.NoError(t, w.Write(pkt.Bytes()[1:]))
	} else {
		require.NoError(t, w.Write(pkt.Bytes()))
	}
}

// fixturePES builds the smallest valid PES packet carrying a PTS (and optionally a DTS).
func fixturePES(pts int64, withDTS bool) *PESData {
	oh := &PESOptionalHeader{
		HasOptionalFields: true,
		PTS:               &ClockReference{Base: pts},
		PTSDTSIndicator:   PTSDTSIndicatorOnlyPTS,
	}
	if withDTS {
		oh.PTSDTSIndicator = PTSDTSIndicatorBothPresent
		oh.DTS = &ClockReference{Base: pts - 90000} // DTS 1s behind PTS
	}
	return &PESData{
		Data:   []byte("elementary-stream-data"),
		Header: &PESHeader{StreamID: 0xe0, OptionalHeader: oh},
	}
}

// fixturePESBytes serializes the PES packet into a payload that fits one TS packet,
// accounting for the bytes an adaptation field would consume in the same packet.
func fixturePESBytes(t *testing.T, pes *PESData, af *PacketAdaptationField) []byte {
	var buf bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	afOverhead := 0
	if af != nil {
		// 1 length byte plus the serialized adaptation field.
		afOverhead = 1 + int(calcPacketAdaptationFieldLength(af))
	}
	n, _, err := writePESData(w, pes.Header, pes.Data, true, MpegTsPacketSize-4-afOverhead)
	require.NoError(t, err)
	require.Greater(t, n, 0)
	out := buf.Bytes()
	if len(out) > MpegTsPacketSize-4-afOverhead {
		t.Fatalf("fixture PES too large: %d bytes", len(out))
	}
	return out
}

func pcrAF(base, ext int64, discontinuous bool) *PacketAdaptationField {
	return &PacketAdaptationField{
		DiscontinuityIndicator: discontinuous,
		HasPCR:                 true,
		PCR:                    &ClockReference{Base: base, Extension: ext},
	}
}

// parseFixture runs the whole fixture through the demuxer and feeds every packet to a
// fresh diagnoser built with the given options.
func parseFixture(t *testing.T, packetSize int, data []byte, opts ...func(*ContinuityDiagnoser)) (*ContinuityDiagnoser, []*DiagnosticEvent) {
	d, err := NewContinuityDiagnoser(append([]func(*ContinuityDiagnoser){ContinuityDiagnoserOptPacketSize(packetSize)}, opts...)...)
	require.NoError(t, err)
	dmx := NewDemuxer(context.Background(), bytes.NewReader(data), DemuxerOptPacketSize(packetSize))
	events, err := d.HandleDemuxer(dmx)
	require.NoError(t, err)
	return d, events
}

func eventTypes(events []*DiagnosticEvent) []DiagnosticEventType {
	out := make([]DiagnosticEventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func findEvent(events []*DiagnosticEvent, typ DiagnosticEventType, pid uint16) *DiagnosticEvent {
	for _, e := range events {
		if e.Type == typ && e.PID == pid {
			return e
		}
	}
	return nil
}

// ---- Tests -----------------------------------------------------------------

func TestContinuityDiagnoserOptions(t *testing.T) {
	// Invalid packet size
	_, err := NewContinuityDiagnoser(ContinuityDiagnoserOptPacketSize(200))
	assert.EqualError(t, err, ErrInvalidPacketSize.Error())

	// Defaults
	d, err := NewContinuityDiagnoser()
	require.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, d.optPacketSize)
	assert.Equal(t, int64(0), d.optPCRBackwardTol)
	assert.Equal(t, int64(0), d.optPTSBackwardTol)
	assert.Equal(t, int64(0), d.optDTSBackwardTol)
	assert.Equal(t, int64(0), d.optPCRMaxForwardGap)

	// 192 bytes accepted
	d, err = NewContinuityDiagnoser(ContinuityDiagnoserOptPacketSize(192))
	require.NoError(t, err)
	assert.Equal(t, 192, d.optPacketSize)
}

func TestContinuityDiagnoserHealthyStream(t *testing.T) {
	const (
		pidVideo uint16 = 0x0100
		pidAudio uint16 = 0x0101
	)
	b := newTSFixtureBuilder(188)
	b.writeNull(t) // index 0: must be ignored entirely
	b.writePES(t, pidVideo, 90000, false, nil)
	b.writePES(t, pidAudio, 81000, false, nil)
	b.writePES(t, pidVideo, 180000, false, nil)
	b.writePES(t, pidAudio, 162000, false, nil)
	b.writeNull(t)

	_, events := parseFixture(t, 188, b.bytes())
	assert.Equal(t, []DiagnosticEventType{
		DiagnosticEventTypeCCFirstSeen, // video at index 1
		DiagnosticEventTypeCCFirstSeen, // audio at index 2
	}, eventTypes(events))

	// Byte offset must account for the leading null packet.
	assert.Equal(t, 1, events[0].PacketIndex)
	assert.Equal(t, 188, events[0].ByteOffset)
}

func TestContinuityDiagnoserCCGap(t *testing.T) {
	const pid uint16 = 0x0200
	// Fully explicit CC sequence so the gap sizes are independent of the builder's
	// auto-incrementing writePES helper.
	b := newTSFixtureBuilder(188)
	b.writeRaw(t, pid, 0, []byte("payload-000000"), true, false, nil, false)
	b.writeRaw(t, pid, 1, []byte("payload-000001"), true, false, nil, false)
	// Two lost packets: observed counter jumps 1 -> 4 (CCs 2 and 3 missing).
	b.writeRaw(t, pid, 4, []byte("payload-000004"), true, false, nil, false)
	// Counter wraparound (14 -> 0) with CC 15 missing in between.
	for _, cc := range []uint16{5, 6, 7, 8, 9, 10, 11, 12, 13, 14} {
		b.writeRaw(t, pid, cc, []byte("payload-cont000"), true, false, nil, false)
	}
	b.writeRaw(t, pid, 0, []byte("payload-wrap000"), true, false, nil, false)

	_, events := parseFixture(t, 188, b.bytes())
	gaps := []*DiagnosticEvent{}
	for _, e := range events {
		if e.Type == DiagnosticEventTypeCCGap {
			gaps = append(gaps, e)
		}
	}
	require.Len(t, gaps, 2)

	assert.Equal(t, 3, gaps[0].MissingCount)
	assert.Equal(t, uint8(1), gaps[0].PreviousContinuityCounter)
	assert.Equal(t, uint8(4), gaps[0].ContinuityCounter)
	assert.Equal(t, 2, gaps[0].PacketIndex)
	assert.Equal(t, 2*188, gaps[0].ByteOffset)

	assert.Equal(t, 2, gaps[1].MissingCount) // modular distance 14 -> 0 is 2
	assert.Equal(t, uint8(14), gaps[1].PreviousContinuityCounter)
	assert.Equal(t, uint8(0), gaps[1].ContinuityCounter)
}

func TestContinuityDiagnoserLegalRetransmission(t *testing.T) {
	const pid uint16 = 0x0300
	pes := fixturePES(90000, false)
	payload := fixturePESBytes(t, pes, nil)

	b := newTSFixtureBuilder(188)
	b.writeRaw(t, pid, 0, payload, true, false, nil, false)
	// Exact retransmission: same counter, same bytes. Must not be a gap.
	b.writeRaw(t, pid, 0, append([]byte(nil), payload...), true, false, nil, false)
	// Same counter but different payload: an error, not a legal duplicate.
	b.writeRaw(t, pid, 1, payload, true, false, nil, false)
	b.writeRaw(t, pid, 1, []byte("different-payload-here!!"), true, false, nil, false)
	// Stream resumes correctly.
	b.writeRaw(t, pid, 2, payload, true, false, nil, false)

	_, events := parseFixture(t, 188, b.bytes())
	dup := findEvent(events, DiagnosticEventTypeCCDuplicate, pid)
	require.NotNil(t, dup)
	assert.Equal(t, 1, dup.PacketIndex)

	// No gap may be reported for the legal duplicate.
	assert.Nil(t, findEvent(events, DiagnosticEventTypeCCGap, pid))

	errs := []*DiagnosticEvent{}
	for _, e := range events {
		if e.Type == DiagnosticEventTypeCCError {
			errs = append(errs, e)
		}
	}
	require.Len(t, errs, 1)
	assert.Equal(t, 3, errs[0].PacketIndex)
}

func TestContinuityDiagnoserPCRWraparoundAndJump(t *testing.T) {
	const pid uint16 = 0x0400
	const maxBase int64 = 1<<33 - 1

	// Keep every PCR transition inside the unambiguous half-modulus window. PCRs are
	// compared in 27 MHz ticks (base*300 + extension).
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 90000, false, pcrAF(maxBase-300, 0, false))
	b.writePES(t, pid, 180000, false, pcrAF(maxBase-200, 0, false))
	b.writePES(t, pid, 270000, false, pcrAF(maxBase-100, 0, false))
	// Legal 33-bit base wraparound: maxBase-100 -> 100 is a forward modular step of 201 bases.
	b.writePES(t, pid, 360000, false, pcrAF(100, 0, false))
	// Backwards PCR, zero tolerance: must be a jump.
	b.writePES(t, pid, 450000, false, pcrAF(50, 0, false))

	_, events := parseFixture(t, 188, b.bytes())
	wrap := findEvent(events, DiagnosticEventTypeTimestampWrap, pid)
	require.NotNil(t, wrap)
	assert.Equal(t, "pcr", wrap.Timestamp)
	assert.Equal(t, (maxBase-100)*300, wrap.Previous)
	assert.Equal(t, int64(30000), wrap.Current) // 100*300 ticks
	assert.Equal(t, int64(60300), wrap.Delta)   // 201 bases * 300 ticks

	jumps := []*DiagnosticEvent{}
	for _, e := range events {
		if e.Type == DiagnosticEventTypePCRJump {
			jumps = append(jumps, e)
		}
	}
	require.Len(t, jumps, 1)
	assert.Equal(t, int64(-15000), jumps[0].Delta) // -50 bases * 300 ticks
	assert.Equal(t, 4, jumps[0].PacketIndex)
}

func TestContinuityDiagnoserPCRBackwardTolerance(t *testing.T) {
	const pid uint16 = 0x0401
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 90000, false, pcrAF(1000, 0, false))
	// 1 base (300 ticks) of backward drift: tolerated up to 300 27 MHz ticks.
	b.writePES(t, pid, 180000, false, pcrAF(999, 0, false))
	// 2 bases (600 ticks) of backward drift: exceeds the tolerance.
	b.writePES(t, pid, 270000, false, pcrAF(997, 0, false))

	_, events := parseFixture(t, 188, b.bytes(), ContinuityDiagnoserOptPCRBackwardTolerance(300))
	jumps := []*DiagnosticEvent{}
	for _, e := range events {
		if e.Type == DiagnosticEventTypePCRJump {
			jumps = append(jumps, e)
		}
	}
	require.Len(t, jumps, 1)
	assert.Equal(t, int64(-600), jumps[0].Delta)

	// Same drift with zero tolerance reports both backwards steps.
	_, events = parseFixture(t, 188, b.bytes())
	zeroTolJumps := 0
	for _, e := range events {
		if e.Type == DiagnosticEventTypePCRJump {
			zeroTolJumps++
		}
	}
	assert.Equal(t, 2, zeroTolJumps)
}

func TestContinuityDiagnoserPTSWraparoundAndForwardGap(t *testing.T) {
	const pid uint16 = 0x0500
	const maxPTS int64 = 1<<33 - 1

	// Start near the 33-bit ceiling so every consecutive step stays within the
	// unambiguous half-modulus window. The raw field then reaches its ceiling and
	// continues at a small value, which is a legal wraparound rather than a jump.
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, maxPTS-200000, false, nil)
	b.writePES(t, pid, maxPTS-100000, false, nil)
	b.writePES(t, pid, maxPTS-1, false, nil) // one unit below the ceiling
	b.writePES(t, pid, 90000, false, nil)    // wraps: forward modular step of 90001

	// Default config: wraparound is informational and no forward gap is configured.
	_, events := parseFixture(t, 188, b.bytes())
	wrap := findEvent(events, DiagnosticEventTypeTimestampWrap, pid)
	require.NotNil(t, wrap)
	assert.Equal(t, "pts", wrap.Timestamp)
	assert.Equal(t, maxPTS-1, wrap.Previous)
	assert.Equal(t, int64(90000), wrap.Current)
	assert.Equal(t, int64(90002), wrap.Delta) // 2^33 - (maxPTS-1) + 90000
	assert.Nil(t, findEvent(events, DiagnosticEventTypePTSJump, pid))

	// A forward gap limit of 95000 is exceeded by the three 100000-unit cadence steps
	// but not by the 90002-unit wraparound step.
	_, events = parseFixture(t, 188, b.bytes(), ContinuityDiagnoserOptPTSMaxForwardGap(95000))
	jumps := []*DiagnosticEvent{}
	for _, e := range events {
		if e.Type == DiagnosticEventTypePTSJump {
			jumps = append(jumps, e)
		}
	}
	assert.Len(t, jumps, 2)
	assert.Equal(t, int64(100000), jumps[0].Delta)
	assert.Equal(t, int64(99999), jumps[1].Delta)

	// A limit below the wrap step flags the wraparound itself as a jump too. Locate
	// the jump on the same packet as the wraparound, rather than the cadence jumps.
	_, events = parseFixture(t, 188, b.bytes(), ContinuityDiagnoserOptPTSMaxForwardGap(90001))
	wrapIdx := wrap.PacketIndex
	var wrapJump *DiagnosticEvent
	for _, e := range events {
		if e.Type == DiagnosticEventTypePTSJump && e.PacketIndex == wrapIdx {
			wrapJump = e
		}
	}
	require.NotNil(t, wrapJump)
	assert.Equal(t, int64(90002), wrapJump.Delta)
}

func TestContinuityDiagnoserPTSBackwardJumpZeroTolerance(t *testing.T) {
	const pid uint16 = 0x0501
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 270000, false, nil)
	// Unambiguous backwards step (far inside the half-modulus window): zero tolerance
	// means any backwards step is a jump.
	b.writePES(t, pid, 180000, false, nil)

	_, events := parseFixture(t, 188, b.bytes())
	jump := findEvent(events, DiagnosticEventTypePTSJump, pid)
	require.NotNil(t, jump)
	assert.Equal(t, int64(-90000), jump.Delta)
	assert.Equal(t, 1, jump.PacketIndex)

	// With a tolerance covering the drift it is no longer reported.
	_, events = parseFixture(t, 188, b.bytes(), ContinuityDiagnoserOptPTSBackwardTolerance(90000))
	assert.Nil(t, findEvent(events, DiagnosticEventTypePTSJump, pid))
}

func TestContinuityDiagnoserDeclaredDiscontinuity(t *testing.T) {
	const pid uint16 = 0x0600
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 90000, false, pcrAF(1000, 0, false))
	// Discontinuity packet with a wild CC jump and a reset PCR: must be re-based, not gapped.
	b.writeRaw(t, pid, 7, fixturePESBytes(t, fixturePES(180000, false), pcrAF(1, 0, true)), true, true, pcrAF(1, 0, true), false)
	// Stream continues from the re-based CC.
	b.writeRaw(t, pid, 8, fixturePESBytes(t, fixturePES(270000, false), nil), true, false, nil, false)

	_, events := parseFixture(t, 188, b.bytes())
	disc := findEvent(events, DiagnosticEventTypeDiscontinuity, pid)
	require.NotNil(t, disc)
	assert.Equal(t, 1, disc.PacketIndex)
	assert.Nil(t, findEvent(events, DiagnosticEventTypeCCGap, pid))
	assert.Nil(t, findEvent(events, DiagnosticEventTypePCRJump, pid))
	assert.Nil(t, findEvent(events, DiagnosticEventTypePTSJump, pid))
}

func TestContinuityDiagnoserTransportError(t *testing.T) {
	const pid uint16 = 0x0700
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 90000, false, nil) // cc 0
	// Corrupt packet: TEI set. It does not advance the continuity baseline.
	b.writeRaw(t, pid, 1, []byte("garbage"), true, false, nil, true)
	// The next healthy packet continues at cc 1 (the value the corrupt packet stole),
	// so the corruption costs one packet but produces no extra false gap: the
	// duplicate CC is a legal retransmission-style repeat with different payload.
	b.writeRaw(t, pid, 1, []byte("recovered-payload"), true, false, nil, false)

	_, events := parseFixture(t, 188, b.bytes())
	terr := findEvent(events, DiagnosticEventTypeTransportError, pid)
	require.NotNil(t, terr)
	assert.Equal(t, 1, terr.PacketIndex)
	assert.Nil(t, findEvent(events, DiagnosticEventTypeCCGap, pid))

	// A corrupt first packet is reported as corrupt, never as a healthy first-seen.
	b2 := newTSFixtureBuilder(188)
	b2.writeRaw(t, pid, 0, []byte("garbage"), true, false, nil, true)
	_, events = parseFixture(t, 188, b2.bytes())
	require.NotNil(t, findEvent(events, DiagnosticEventTypeTransportError, pid))
	assert.Nil(t, findEvent(events, DiagnosticEventTypeCCFirstSeen, pid))
}

func TestContinuityDiagnoserMalformedAndMissingPTS(t *testing.T) {
	const pid uint16 = 0x0800
	b := newTSFixtureBuilder(188)
	// Reserved adaptation field control 0b00.
	b.writeRaw(t, pid, 0, nil, false, false, nil, false)
	// Valid PES with PTS, counter forced to 1 to follow the malformed packet.
	validPES := fixturePES(90000, false)
	b.writeRaw(t, pid, 1, fixturePESBytes(t, validPES, nil), true, false, nil, false)
	// PES with an optional header but no PTS (no-PTS/no-DTS indicator).
	noPTS := &PESData{
		Data: []byte("x"),
		Header: &PESHeader{
			StreamID:       0xc0, // audio, optional header allowed
			OptionalHeader: &PESOptionalHeader{HasOptionalFields: true, PTSDTSIndicator: PTSDTSIndicatorNoPTSOrDTS},
		},
	}
	var buf bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	_, _, err := writePESData(w, noPTS.Header, noPTS.Data, true, MpegTsPacketSize-4)
	require.NoError(t, err)
	b.writeRaw(t, pid, 2, buf.Bytes(), true, false, nil, false)

	_, events := parseFixture(t, 188, b.bytes())
	require.NotNil(t, findEvent(events, DiagnosticEventTypeCCMalformed, pid))
	require.NotNil(t, findEvent(events, DiagnosticEventTypeMissingPTS, pid))

	// After a missing PTS the next valid PES re-establishes the baseline instead of
	// being compared to the pre-gap PTS.
	b.writeRaw(t, pid, 3, fixturePESBytes(t, fixturePES(180000, false), nil), true, false, nil, false)
	_, events = parseFixture(t, 188, b.bytes())
	assert.Nil(t, findEvent(events, DiagnosticEventTypePTSJump, pid))
}

func TestContinuityDiagnoserPIDRemap(t *testing.T) {
	const pid uint16 = 0x0900
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 90000, false, nil)
	b.writePES(t, pid, 180000, false, nil)

	d, err := NewContinuityDiagnoser()
	require.NoError(t, err)

	dmx := NewDemuxer(context.Background(), bytes.NewReader(b.bytes()), DemuxerOptPacketSize(188))
	events, err := d.HandleDemuxer(dmx)
	require.NoError(t, err)
	assert.Len(t, events, 1) // first seen only

	// The PID is remapped to a different elementary stream out-of-band.
	d.ResetPID(pid)

	// A remapped stream legitimately restarts its counter and PTS at arbitrary values.
	b2 := newTSFixtureBuilder(188)
	b2.writePES(t, pid, 5000000, false, nil)
	dmx2 := NewDemuxer(context.Background(), bytes.NewReader(b2.bytes()), DemuxerOptPacketSize(188))
	events, err = d.HandleDemuxer(dmx2)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, DiagnosticEventTypeCCFirstSeen, events[0].Type)
	assert.Nil(t, findEvent(events, DiagnosticEventTypeCCGap, pid))
	assert.Nil(t, findEvent(events, DiagnosticEventTypePTSJump, pid))
}

func TestContinuityDiagnoser192Bytes(t *testing.T) {
	const pid uint16 = 0x0a00
	// Explicit CC sequence 0, 1, 3: the jump from 1 to 3 means one missing packet.
	// Non-PES payloads (no 0x000001 prefix) keep the test focused on the CC logic.
	b := newTSFixtureBuilder(192)
	b.writeRaw(t, pid, 0, []byte("payload-zero00"), true, false, nil, false) // index 0
	b.writeRaw(t, pid, 1, []byte("payload-one000"), true, false, nil, false)
	b.writeRaw(t, pid, 3, []byte("payload-three0"), true, false, nil, false) // gap here

	_, events := parseFixture(t, 192, b.bytes())
	gap := findEvent(events, DiagnosticEventTypeCCGap, pid)
	require.NotNil(t, gap)
	assert.Equal(t, 2, gap.PacketIndex)
	assert.Equal(t, 2*192, gap.ByteOffset)
	// CC 1 -> 3 means CC 2 was lost: two packets separate the observed counters.
	assert.Equal(t, 2, gap.MissingCount)
	assert.Equal(t, uint8(1), gap.PreviousContinuityCounter)
	assert.Equal(t, uint8(3), gap.ContinuityCounter)

	// The demuxer parses 192-byte framing when configured explicitly.
	d, err := NewContinuityDiagnoser(ContinuityDiagnoserOptPacketSize(192))
	require.NoError(t, err)
	dmx := NewDemuxer(context.Background(), bytes.NewReader(b.bytes()), DemuxerOptPacketSize(192))
	p, err := dmx.NextPacket()
	require.NoError(t, err)
	assert.Equal(t, pid, p.Header.PID)
	first := d.HandlePacket(p)
	require.Len(t, first, 1)
	assert.Equal(t, DiagnosticEventTypeCCFirstSeen, first[0].Type)
	assert.Equal(t, 0, first[0].ByteOffset)
}

func TestContinuityDiagnoserBoundedState(t *testing.T) {
	d, err := NewContinuityDiagnoser(ContinuityDiagnoserOptMaxTrackedPIDs(2))
	require.NoError(t, err)

	feed := func(pid uint16, cc uint8) {
		events := d.HandlePacket(&Packet{
			Header:  PacketHeader{PID: pid, ContinuityCounter: cc, HasPayload: true},
			Payload: []byte("p"),
		})
		// Every observation here is either first-seen or an expected event; callers
		// only assert on state map size.
		_ = events
	}

	feed(0x100, 0)
	feed(0x101, 0)
	feed(0x102, 0) // evicts 0x100
	assert.Len(t, d.states, 2)
	assert.NotContains(t, d.states, uint32(0x100))

	// 0x100 returns: it is a fresh baseline, not a gap.
	events := d.HandlePacket(&Packet{
		Header:  PacketHeader{PID: 0x100, ContinuityCounter: 9, HasPayload: true},
		Payload: []byte("p"),
	})
	require.Len(t, events, 1)
	assert.Equal(t, DiagnosticEventTypeCCFirstSeen, events[0].Type)

	// Reset clears everything.
	d.Reset()
	assert.Empty(t, d.states)
	assert.Equal(t, 0, d.packetIndex)
}

func TestContinuityDiagnoserNullPacketsIgnored(t *testing.T) {
	d, err := NewContinuityDiagnoser()
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		assert.Empty(t, d.HandlePacket(&Packet{
			Header:  PacketHeader{PID: PIDNull, ContinuityCounter: uint8(i), HasPayload: true},
			Payload: []byte{0xff},
		}))
	}
	assert.Empty(t, d.states)
	// Null packets still count toward global packet indexing for byte offsets.
	assert.Equal(t, 5, d.packetIndex)
}

func TestDiagnosticEventStringAndSeverity(t *testing.T) {
	assert.Equal(t, diagnosticSeverityError, DiagnosticEventTypeCCGap.Severity())
	assert.Equal(t, diagnosticSeverityError, DiagnosticEventTypePCRJump.Severity())
	assert.Equal(t, diagnosticSeverityWarning, DiagnosticEventTypeMissingPTS.Severity())
	assert.Equal(t, diagnosticSeverityInfo, DiagnosticEventTypeCCDuplicate.Severity())
	assert.Equal(t, diagnosticSeverityInfo, DiagnosticEventTypeCCFirstSeen.Severity())

	e := DiagnosticEvent{
		Type:                      DiagnosticEventTypeCCGap,
		PID:                       0x200,
		PacketIndex:               7,
		ByteOffset:                7 * 188,
		ContinuityCounter:         4,
		PreviousContinuityCounter: 1,
		MissingCount:              2,
	}
	assert.Contains(t, e.String(), "pid 512")
	assert.Contains(t, e.String(), "2 packet(s) missing")
}

func TestContinuityDiagnoserDTS(t *testing.T) {
	const pid uint16 = 0x0b00
	// PES packets carrying both PTS and DTS (DTS = PTS - 90000).
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 270000, true, nil)
	b.writePES(t, pid, 360000, true, nil)
	// Backward DTS step of 45000 units, zero tolerance.
	b.writePES(t, pid, 315000, true, nil)

	_, events := parseFixture(t, 188, b.bytes())
	dtsJump := findEvent(events, DiagnosticEventTypeDTSJump, pid)
	require.NotNil(t, dtsJump)
	// DTS values are 270000-90000=180000, 360000-90000=270000, then 315000-90000=225000.
	assert.Equal(t, int64(-45000), dtsJump.Delta)
	assert.Equal(t, 2, dtsJump.PacketIndex)

	// With a matching tolerance the jump is suppressed, and a forward gap option is
	// exercised on the healthy packets without firing.
	_, events = parseFixture(t, 188, b.bytes(),
		ContinuityDiagnoserOptDTSBackwardTolerance(45000),
		ContinuityDiagnoserOptDTSMaxForwardGap(200000))
	assert.Nil(t, findEvent(events, DiagnosticEventTypeDTSJump, pid))
}

func TestContinuityDiagnoserPCRForwardGap(t *testing.T) {
	const pid uint16 = 0x0b01
	b := newTSFixtureBuilder(188)
	b.writePES(t, pid, 90000, false, pcrAF(1000, 0, false))
	// Forward jump of 1000 bases (300000 ticks).
	b.writePES(t, pid, 180000, false, pcrAF(2000, 0, false))

	// Default: forward checking disabled, nothing reported.
	_, events := parseFixture(t, 188, b.bytes())
	assert.Nil(t, findEvent(events, DiagnosticEventTypePCRJump, pid))

	// A tight forward limit flags the same step.
	_, events = parseFixture(t, 188, b.bytes(), ContinuityDiagnoserOptPCRMaxForwardGap(100000))
	jump := findEvent(events, DiagnosticEventTypePCRJump, pid)
	require.NotNil(t, jump)
	assert.Equal(t, int64(300000), jump.Delta)
}

// errReader fails after zero bytes to exercise the HandleDemuxer error branch.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errFixtureRead }

var errFixtureRead = errors.New("fixture: synthetic read failure")

func TestContinuityDiagnoserHandleDemuxerError(t *testing.T) {
	d, err := NewContinuityDiagnoser()
	require.NoError(t, err)
	dmx := NewDemuxer(context.Background(), errReader{}, DemuxerOptPacketSize(188))
	events, err := d.HandleDemuxer(dmx)
	assert.Error(t, err)
	assert.Nil(t, events)
}

func TestDiagnosticEventStringBranches(t *testing.T) {
	cases := []struct {
		typ DiagnosticEventType
		sub string
	}{
		{DiagnosticEventTypeCCFirstSeen, "first seen"},
		{DiagnosticEventTypeCCDuplicate, "retransmission"},
		{DiagnosticEventTypeCCError, "continuity error"},
		{DiagnosticEventTypeCCMalformed, "malformed"},
		{DiagnosticEventTypeDiscontinuity, "declared discontinuity"},
		{DiagnosticEventTypeTransportError, "corrupt"},
		{DiagnosticEventTypeTimestampWrap, "wraparound"},
		{DiagnosticEventTypeMissingPTS, "without PTS"},
		{DiagnosticEventTypePESMalformed, "malformed PES"},
		{DiagnosticEventTypePTSJump, "pts jump"},
		{DiagnosticEventTypeDTSJump, "dts jump"},
	}
	for _, c := range cases {
		ts := "pts"
		if c.typ == DiagnosticEventTypeDTSJump {
			ts = "dts"
		}
		e := DiagnosticEvent{Type: c.typ, PID: 42, PacketIndex: 3, ByteOffset: 3 * 188, Timestamp: ts}
		assert.Contains(t, e.String(), c.sub, c.typ)
		assert.Contains(t, e.String(), "pid 42")
	}
}
