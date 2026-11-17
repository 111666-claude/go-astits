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

// tsBuilder assembles an MPEG-TS stream packet by packet for fixture generation.
// It keeps one continuity counter per PID and splits large payloads across
// several TS packets, exactly like a real multiplexer would.
type tsBuilder struct {
	buf bytes.Buffer
	cc  map[uint16]uint8
}

func newTSBuilder() *tsBuilder {
	return &tsBuilder{cc: make(map[uint16]uint8)}
}

// writePayload wraps a PES or PSI payload in 188-byte TS packets for the given PID.
// The first packet gets the payload unit start indicator.
func (b *tsBuilder) writePayload(pid uint16, payload []byte) {
	first := true
	for len(payload) > 0 {
		p := &Packet{Header: PacketHeader{
			PID:                       pid,
			HasPayload:                true,
			PayloadUnitStartIndicator: first,
			ContinuityCounter:         b.cc[pid],
		}}
		b.cc[pid] = (b.cc[pid] + 1) & 0x0f

		// 188-byte packet minus the 3-byte TS header minus the 1-byte sync
		const maxPayload = MpegTsPacketSize - mpegTsPacketHeaderSize - 1 // 184 bytes
		if len(payload) > maxPayload {
			p.Payload = payload[:maxPayload]
			payload = payload[maxPayload:]
		} else {
			p.Payload = payload
			payload = nil
		}
		if _, err := writePacket(astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf}), p, MpegTsPacketSize); err != nil {
			panic(err)
		}
		first = false
	}
}

// writeRawPacket writes a fully formed TS packet, e.g. one with a broken
// continuity counter, bypassing the automatic counter bookkeeping.
func (b *tsBuilder) writeRawPacket(p *Packet) {
	if _, err := writePacket(astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf}), p, MpegTsPacketSize); err != nil {
		panic(err)
	}
}

// bytes returns the assembled transport stream.
func (b *tsBuilder) bytes() []byte {
	return b.buf.Bytes()
}

// buildPATSection builds the pointer-field + PAT section bytes of a PAT.
func buildPATSection(pat *PATData) []byte {
	var buf bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	section := &PSISection{
		Header: &PSISectionHeader{
			TableID:                PSITableIDPAT,
			SectionSyntaxIndicator: true,
		},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{
				TableIDExtension:     pat.TransportStreamID,
				CurrentNextIndicator: true,
				SectionNumber:        0,
				LastSectionNumber:    0,
			},
			Data: &PSISectionSyntaxData{PAT: pat},
		},
	}
	section.Header.SectionLength = calcPSISectionLength(section)
	psi := &PSIData{
		PointerField: 0,
		Sections:     []*PSISection{section},
	}
	_, err := writePSIData(w, psi)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// buildPMTSection builds the pointer-field + PMT section bytes of a PMT.
func buildPMTSection(pmt *PMTData) []byte {
	var buf bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	section := &PSISection{
		Header: &PSISectionHeader{
			TableID:                PSITableIDPMT,
			SectionSyntaxIndicator: true,
		},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{
				TableIDExtension:     pmt.ProgramNumber,
				CurrentNextIndicator: true,
				SectionNumber:        0,
				LastSectionNumber:    0,
			},
			Data: &PSISectionSyntaxData{PMT: pmt},
		},
	}
	section.Header.SectionLength = calcPSISectionLength(section)
	psi := &PSIData{
		PointerField: 0,
		Sections:     []*PSISection{section},
	}
	_, err := writePSIData(w, psi)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// teletextPES builds a teletext PES packet (stream_id 0xBD, private stream 1)
// carrying the given teletext data units. When pts is nil the optional header
// carries no PTS.
func teletextPES(pts *ClockReference, units ...[]byte) *PESData {
	var payload bytes.Buffer
	payload.WriteByte(0x10) // data_identifier: EBU teletext, non-subtitle
	for _, u := range units {
		payload.Write(u)
	}
	payload.WriteByte(TeletextDataUnitIDFiller) // rest of the PES is stuffing

	h := &PESHeader{StreamID: StreamIDPrivateStream1}
	if pts != nil {
		h.OptionalHeader = &PESOptionalHeader{
			PTS:             pts,
			PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
		}
	} else {
		h.OptionalHeader = &PESOptionalHeader{
			PTSDTSIndicator: PTSDTSIndicatorNoPTSOrDTS,
		}
	}
	return &PESData{Header: h, Data: payload.Bytes()}
}

// pesBytes serializes a PESData the same way it appears in TS packet payloads.
func pesBytes(pes *PESData) []byte {
	var buf bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	_, _, err := writePESData(w, pes.Header, pes.Data, true, len(pes.Data)+64)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// hamming84 encodes a 4-bit nibble into its Hamming-8/4 byte.
func hamming84(nibble uint8) byte {
	return hamming84Encode[nibble&0x0f]
}

// teletextAddressBytes builds the two Hamming-8/4 address bytes of an EBU
// teletext packet for the given magazine (1-8) and row (0-31).
func teletextAddressBytes(magazine, row uint8) []byte {
	if magazine == 8 {
		magazine = 0 // magazine 0 is encoded with value 8
	}
	mag := magazine & 0x07
	nibble1 := mag | ((row & 0x01) << 3)
	nibble2 := (row >> 1) & 0x0f
	return []byte{hamming84(nibble1), hamming84(nibble2)}
}

// teletextUnit builds a 46-byte raw teletext data unit (2 header bytes + 44 data bytes).
func teletextUnit(unitID uint8, magazine, row uint8, pageTens, pageUnits byte, body []byte) []byte {
	u := make([]byte, teletextDataUnitHeaderLength+teletextDataUnitLength)
	u[0] = unitID
	u[1] = teletextDataUnitLength
	u[2] = 0x55 // clock run-in
	u[3] = 0x55 // clock run-in
	u[4] = 0x27 // framing code
	addr := teletextAddressBytes(magazine, row)
	u[5] = addr[0]
	u[6] = addr[1]
	// Row 0 page header: page tens/units in the low nibbles of bytes 7 and 8
	u[7] = 0x80 | (pageTens & 0x0f)
	u[8] = 0x80 | (pageUnits & 0x0f)
	copy(u[9:], body)
	return u
}

// teletextDescriptor builds a teletext descriptor with its Length field set,
// which the muxer side requires to actually write the descriptor payload.
func teletextDescriptor(items ...*DescriptorTeletextItem) *Descriptor {
	tt := &DescriptorTeletext{Items: items}
	return &Descriptor{
		Tag:      DescriptorTagTeletext,
		Length:   calcDescriptorTeletextLength(tt),
		Teletext: tt,
	}
}

// teletextFixture is the synthetic dual-language, multi-PES transport stream
// built for the extractor tests.
type teletextFixture struct {
	ts []byte

	// PIDs / programs
	patPID   uint16
	pmtPID1  uint16
	ttPID1   uint16
	pmtPID2  uint16
	ttPID2   uint16
	program1 uint16
	program2 uint16

	// Expected teletext content of program 1
	engPTS1, engPTS2 *ClockReference
	engUnits         [][]byte
	deuPTS1, deuPTS2 *ClockReference
	deuUnits         [][]byte
}

const (
	fxPMTPID1  = uint16(0x1000)
	fxTTPID1   = uint16(0x0101)
	fxPMTPID2  = uint16(0x1001)
	fxTTPID2   = uint16(0x0201)
	fxProgram1 = uint16(0x1111)
	fxProgram2 = uint16(0x2222)
	fxTSID     = uint16(0x04d2)
	fxEngMag   = uint8(3)
	fxEngPage  = uint8(40)
	fxDeuMag   = uint8(5)
	fxDeuPage  = uint8(77)
)

// newTeletextFixture builds the dual-language multi-PES fixture described in
// teletext_test.go. Two programs share the stream; program 1 carries English
// (magazine 3, page 40) and German (magazine 5, page 77) teletext, each spread
// over several multi-packet PES packets. Program 2 carries its own teletext
// PID which must never leak into program 1's extraction output.
func newTeletextFixture(t *testing.T) *teletextFixture {
	t.Helper()

	// PAT referencing both programs
	pat := &PATData{
		TransportStreamID: fxTSID,
		Programs: []*PATProgram{
			{ProgramNumber: fxProgram1, ProgramMapID: fxPMTPID1},
			{ProgramNumber: fxProgram2, ProgramMapID: fxPMTPID2},
		},
	}

	// PMT of program 1: one private-data elementary stream with a teletext
	// descriptor advertising two languages.
	pmt1 := &PMTData{
		ProgramNumber: fxProgram1,
		PCRPID:        fxTTPID1,
		ElementaryStreams: []*PMTElementaryStream{{
			ElementaryPID: fxTTPID1,
			StreamType:    StreamTypeMPEG2PacketizedData,
			ElementaryStreamDescriptors: []*Descriptor{teletextDescriptor(
				&DescriptorTeletextItem{Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxEngMag, Page: fxEngPage},
				&DescriptorTeletextItem{Language: []byte("deu"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxDeuMag, Page: fxDeuPage},
			)},
		}},
	}

	// PMT of program 2: another teletext PID, must stay isolated
	pmt2 := &PMTData{
		ProgramNumber: fxProgram2,
		PCRPID:        fxTTPID2,
		ElementaryStreams: []*PMTElementaryStream{{
			ElementaryPID: fxTTPID2,
			StreamType:    StreamTypeMPEG2PacketizedData,
			ElementaryStreamDescriptors: []*Descriptor{teletextDescriptor(
				&DescriptorTeletextItem{Language: []byte("fra"), Type: TeletextTypeTeletextSubtitlePage, Magazine: 2, Page: 10},
			)},
		}},
	}

	// English teletext data units, page 340: row 0 header then two subtitle rows
	engHeader := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 0, 4, 0, bytes.Repeat([]byte{0xa1}, 36))
	engUnit1 := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 1, 4, 0, bytes.Repeat([]byte{0xb2}, 36))
	engUnit2 := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 2, 4, 0, bytes.Repeat([]byte{0xc3}, 36))
	// A non-subtitle unit that must be filtered out of the output
	engNonSub := teletextUnit(TeletextDataUnitIDNonSubtitle, fxEngMag, 4, 4, 0, bytes.Repeat([]byte{0xd4}, 36))

	// German teletext data units, page 577
	deuHeader := teletextUnit(TeletextDataUnitIDSubtitle, fxDeuMag, 0, 7, 7, bytes.Repeat([]byte{0xe5}, 36))
	deuUnit1 := teletextUnit(TeletextDataUnitIDSubtitle, fxDeuMag, 1, 7, 7, bytes.Repeat([]byte{0xf6}, 36))
	deuUnit2 := teletextUnit(TeletextDataUnitIDSubtitle, fxDeuMag, 2, 7, 7, bytes.Repeat([]byte{0x07}, 36))

	// Foreign program unit on the other PID, must never appear in program 1 output
	fraUnit := teletextUnit(TeletextDataUnitIDSubtitle, 2, 1, 1, 0, bytes.Repeat([]byte{0x18}, 36))

	engPTS1 := &ClockReference{Base: 90000 * 10} // 10 s
	engPTS2 := &ClockReference{Base: 90000 * 12} // 12 s
	deuPTS1 := &ClockReference{Base: 90000 * 20} // 20 s
	deuPTS2 := &ClockReference{Base: 90000 * 22} // 22 s

	b := newTSBuilder()
	b.writePayload(0x0, buildPATSection(pat))
	b.writePayload(fxPMTPID1, buildPMTSection(pmt1))
	b.writePayload(fxPMTPID2, buildPMTSection(pmt2))

	// PES #1 on PID 1: English header + first subtitle + a non-subtitle unit,
	// deliberately large enough to span several TS packets.
	bigUnit := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 3, 4, 0, bytes.Repeat([]byte{0xee}, 36))
	pes1 := teletextPES(engPTS1, engHeader, engUnit1, engNonSub, bigUnit)
	b.writePayload(fxTTPID1, pesBytes(pes1))

	// PES #1 on PID 2: French unit, must stay isolated
	b.writePayload(fxTTPID2, pesBytes(teletextPES(&ClockReference{Base: 90000 * 30}, fraUnit)))

	// PES #2 on PID 1: English second subtitle
	pes2 := teletextPES(engPTS2, engUnit2)
	b.writePayload(fxTTPID1, pesBytes(pes2))

	// PES #3/#4 on PID 1: German units, interleaved
	b.writePayload(fxTTPID1, pesBytes(teletextPES(deuPTS1, deuHeader, deuUnit1)))
	b.writePayload(fxTTPID1, pesBytes(teletextPES(deuPTS2, deuUnit2)))

	fx := &teletextFixture{
		ts:       b.bytes(),
		patPID:   0x0,
		pmtPID1:  fxPMTPID1,
		ttPID1:   fxTTPID1,
		pmtPID2:  fxPMTPID2,
		ttPID2:   fxTTPID2,
		program1: fxProgram1,
		program2: fxProgram2,
		engPTS1:  engPTS1,
		engPTS2:  engPTS2,
		engUnits: [][]byte{engHeader, engUnit1, bigUnit, engUnit2},
		deuPTS1:  deuPTS1,
		deuPTS2:  deuPTS2,
		deuUnits: [][]byte{deuHeader, deuUnit1, deuUnit2},
	}
	return fx
}

// drain runs the demuxer through the fixture and collects extractor events.
func drainExtractor(t *testing.T, ts []byte, selector TeletextSelector) ([]*TeletextEvent, *TeletextExtractor) {
	t.Helper()
	ext := NewTeletextExtractor(selector)
	dmx := NewDemuxer(context.Background(), bytes.NewReader(ts),
		DemuxerOptPacketSize(MpegTsPacketSize),
		DemuxerOptPacketSkipper(ext.PacketSkipper()))

	var events []*TeletextEvent
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		events = append(events, ext.HandleData(d)...)
	}
	// Continuity errors detected on packets after the last data are drained here
	events = append(events, ext.DrainPending()...)
	return events, ext
}

func TestFindTeletextTracks(t *testing.T) {
	pmt := &PMTData{
		ProgramNumber: fxProgram1,
		ElementaryStreams: []*PMTElementaryStream{
			{
				ElementaryPID: fxTTPID1,
				ElementaryStreamDescriptors: []*Descriptor{
					{Tag: DescriptorTagTeletext, Teletext: &DescriptorTeletext{Items: []*DescriptorTeletextItem{
						{Language: []byte("eng"), Magazine: 3, Page: 40, Type: TeletextTypeTeletextSubtitlePage},
						{Language: []byte("deu"), Magazine: 5, Page: 77, Type: TeletextTypeTeletextSubtitlePage},
					}}},
					{Tag: DescriptorTagVBITeletext, VBITeletext: &DescriptorTeletext{Items: []*DescriptorTeletextItem{
						{Language: []byte("ita"), Magazine: 1, Page: 5},
					}}},
				},
			},
			{
				ElementaryPID:               fxTTPID2,
				ElementaryStreamDescriptors: []*Descriptor{{Tag: DescriptorTagStreamIdentifier, StreamIdentifier: &DescriptorStreamIdentifier{ComponentTag: 7}}},
			},
		},
	}

	tracks := FindTeletextTracks(pmt)
	require.Len(t, tracks, 2)
	assert.Equal(t, fxTTPID1, tracks[0].PID)
	assert.Equal(t, fxProgram1, tracks[0].ProgramNumber)
	assert.Equal(t, []byte("eng"), tracks[0].Language)
	assert.Equal(t, uint8(3), tracks[0].Magazine)
	assert.Equal(t, uint8(40), tracks[0].Page)
	assert.Equal(t, uint8(TeletextTypeTeletextSubtitlePage), tracks[0].Type)
	assert.Equal(t, []byte("deu"), tracks[1].Language)

	// VBI teletext descriptor and non-teletext streams are ignored
	assert.Nil(t, FindTeletextTracks(nil))
}

func TestHamming84RoundTrip(t *testing.T) {
	for n := 0; n < 16; n++ {
		encoded := hamming84Encode[n]
		decoded, corrected := hamming84Decode(encoded)
		assert.False(t, corrected)
		assert.Equal(t, uint8(n), decoded)
	}

	// Every single-bit flip must be corrected
	for n := 0; n < 16; n++ {
		encoded := hamming84Encode[n]
		for bit := uint(0); bit < 8; bit++ {
			flipped := encoded ^ (1 << bit)
			decoded, corrected := hamming84Decode(flipped)
			assert.True(t, corrected, "nibble %d bit %d should be corrected", n, bit)
			assert.Equal(t, uint8(n), decoded)
		}
	}
}

func TestTeletextAddressBytes(t *testing.T) {
	// Magazine 3, row 1 (EN 300 706): nibble1 = (row&1)<<3 | mag = 0b1011,
	// nibble2 = row >> 1 = 0
	addr := teletextAddressBytes(3, 1)
	n1, c1 := hamming84Decode(addr[0])
	n2, c2 := hamming84Decode(addr[1])
	assert.False(t, c1)
	assert.False(t, c2)
	assert.Equal(t, uint8(0x0b), n1)
	assert.Equal(t, uint8(0x00), n2)

	// Row 0 magazine 3: nibble1 = mag = 3
	addr = teletextAddressBytes(3, 0)
	n1, _ = hamming84Decode(addr[0])
	assert.Equal(t, uint8(0x03), n1)

	// Magazine 8 is wire-encoded as 0
	addr = teletextAddressBytes(8, 0)
	n1, _ = hamming84Decode(addr[0])
	assert.Equal(t, uint8(0), n1)
}
