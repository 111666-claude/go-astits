package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dataUnitsFromEvents extracts the data unit payloads of data-unit events in order.
func dataUnitsFromEvents(events []*TeletextEvent) []*TeletextDataUnit {
	var out []*TeletextDataUnit
	for _, ev := range events {
		if ev.Kind == TeletextEventKindDataUnit {
			out = append(out, ev.DataUnit)
		}
	}
	return out
}

// errorFromEvents returns the first event matching the sentinel error, or nil.
func errorFromEvents(events []*TeletextEvent, sentinel error) error {
	for _, ev := range events {
		if ev.Kind == TeletextEventKindError && errors.Is(ev.Err, sentinel) {
			return ev.Err
		}
	}
	return nil
}

// requireErrorEvent asserts that an event wrapping the sentinel was emitted.
func requireErrorEvent(t *testing.T, events []*TeletextEvent, sentinel error) {
	t.Helper()
	err := errorFromEvents(events, sentinel)
	require.Truef(t, errors.Is(err, sentinel), "expected error wrapping %v, got %v", sentinel, err)
}

// singleProgramTables returns PAT/PMT section bytes for a one-program stream
// whose single elementary stream carries the given descriptors.
func singleProgramTables(pmtPID, ttPID uint16, program uint16, esDescs []*Descriptor) (patBytes, pmtBytes []byte) {
	pat := &PATData{
		TransportStreamID: fxTSID,
		Programs:          []*PATProgram{{ProgramNumber: program, ProgramMapID: pmtPID}},
	}
	pmt := &PMTData{
		ProgramNumber: program,
		PCRPID:        ttPID,
		ElementaryStreams: []*PMTElementaryStream{{
			ElementaryPID:               ttPID,
			StreamType:                  StreamTypeMPEG2PacketizedData,
			ElementaryStreamDescriptors: esDescs,
		}},
	}
	return buildPATSection(pat), buildPMTSection(pmt)
}

// TestTeletextExtractorEnglishTrack checks the full happy path on the
// dual-language fixture: PMT-driven PID/language/page selection, PTS timing,
// raw byte preservation, data unit ordering and cross-program isolation.
func TestTeletextExtractorEnglishTrack(t *testing.T) {
	fx := newTeletextFixture(t)
	events, ext := drainExtractor(t, fx.ts, TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})

	// The selector resolved to the English descriptor entry
	tr := ext.SelectedTrack()
	require.NotNil(t, tr)
	assert.Equal(t, fxTTPID1, tr.PID)
	assert.Equal(t, []byte("eng"), tr.Language)
	assert.Equal(t, fxEngMag, tr.Magazine)
	assert.Equal(t, fxEngPage, tr.Page)

	units := dataUnitsFromEvents(events)
	// Expected English units: header, unit1, bigUnit (PES #1), unit2 (PES #2).
	// The non-subtitle unit and every German/French unit must be filtered out.
	require.Len(t, units, len(fx.engUnits))
	for i, want := range fx.engUnits {
		got := units[i]
		assert.Equalf(t, want, got.Data, "data unit %d bytes must be preserved in order", i)
		assert.Equalf(t, uint8(TeletextDataUnitIDSubtitle), got.DataUnitID, "data unit %d id", i)
		assert.Equalf(t, fxTTPID1, got.PID, "data unit %d pid", i)
		assert.Equalf(t, fxProgram1, got.ProgramNumber, "data unit %d program", i)
		assert.Equalf(t, []byte("eng"), got.Language, "data unit %d language", i)
	}

	// Timing: PES #1 units carry the first PTS, PES #2 units the second
	assert.Equal(t, fx.engPTS1.Base, units[0].PTS.Base)
	assert.True(t, units[0].PTSPresent)
	assert.Equal(t, fx.engPTS1.Base, units[2].PTS.Base)
	assert.Equal(t, fx.engPTS2.Base, units[3].PTS.Base)

	// Page decoding: the row 0 header was seen first, so every unit knows page 340
	for _, u := range units {
		assert.Equal(t, fxEngMag, u.Magazine)
		assert.Equal(t, fxEngPage, u.Page)
	}
	assert.Equal(t, uint8(0), units[0].Row)
	assert.Equal(t, uint8(1), units[1].Row)
	assert.Equal(t, uint8(3), units[2].Row)
	assert.Equal(t, uint8(2), units[3].Row)

	// No error events on a healthy stream
	assert.Nil(t, errorFromEvents(events, ErrTeletextMissingPTS))
	assert.Nil(t, errorFromEvents(events, ErrTeletextContinuity))
	assert.Nil(t, errorFromEvents(events, ErrTeletextBadDataUnitLength))
}

// TestTeletextExtractorGermanTrack checks selection of the second language on
// the same PID: English units must not leak into the German output.
func TestTeletextExtractorGermanTrack(t *testing.T) {
	fx := newTeletextFixture(t)
	events, ext := drainExtractor(t, fx.ts, TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("deu"),
	})

	tr := ext.SelectedTrack()
	require.NotNil(t, tr)
	assert.Equal(t, []byte("deu"), tr.Language)
	assert.Equal(t, fxTTPID1, tr.PID)

	units := dataUnitsFromEvents(events)
	require.Len(t, units, len(fx.deuUnits))
	for i, want := range fx.deuUnits {
		assert.Equalf(t, want, units[i].Data, "german data unit %d bytes", i)
		assert.Equal(t, []byte("deu"), units[i].Language)
		assert.Equal(t, fxDeuMag, units[i].Magazine)
		assert.Equal(t, fxDeuPage, units[i].Page)
	}
	assert.Equal(t, fx.deuPTS1.Base, units[0].PTS.Base)
	assert.Equal(t, fx.deuPTS1.Base, units[1].PTS.Base)
	assert.Equal(t, fx.deuPTS2.Base, units[2].PTS.Base)

	// Magazine isolation: no English magazine 3 unit slipped through
	for _, u := range units {
		assert.NotEqual(t, fxEngMag, u.Magazine)
	}
}

// TestTeletextExtractorMultiPacketPES verifies that a PES packet spanning
// several TS packets is reassembled before data unit parsing.
func TestTeletextExtractorMultiPacketPES(t *testing.T) {
	fx := newTeletextFixture(t)

	// PES #1 holds 4 data units + headers and must occupy more than one 188-byte packet
	pes1 := teletextPES(fx.engPTS1, fx.engUnits[0], fx.engUnits[1],
		teletextUnit(TeletextDataUnitIDNonSubtitle, fxEngMag, 4, 4, 0, bytes.Repeat([]byte{0xd4}, 36)),
		fx.engUnits[2])
	raw := pesBytes(pes1)
	require.Greater(t, len(raw), MpegTsPacketSize, "fixture PES must span multiple TS packets")

	// The demuxer must still reassemble it without error
	events, _ := drainExtractor(t, fx.ts, TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	units := dataUnitsFromEvents(events)
	require.GreaterOrEqual(t, len(units), 3)
	// The large unit carried by the multi-packet PES is byte-identical to the input
	assert.Equal(t, fx.engUnits[2], units[2].Data)
}

// TestTeletextExtractorProgramIsolation verifies that two programs on the same
// transport stream never mix their teletext output.
func TestTeletextExtractorProgramIsolation(t *testing.T) {
	fx := newTeletextFixture(t)

	events1, _ := drainExtractor(t, fx.ts, TeletextSelector{ProgramNumber: fxProgram1})
	for _, ev := range events1 {
		if ev.Kind == TeletextEventKindDataUnit {
			assert.Equal(t, fxTTPID1, ev.DataUnit.PID)
			assert.NotEqual(t, fxTTPID2, ev.DataUnit.PID)
		}
	}

	events2, ext2 := drainExtractor(t, fx.ts, TeletextSelector{ProgramNumber: fxProgram2})
	tr2 := ext2.SelectedTrack()
	require.NotNil(t, tr2)
	assert.Equal(t, fxTTPID2, tr2.PID)
	for _, ev := range events2 {
		if ev.Kind == TeletextEventKindDataUnit {
			assert.Equal(t, fxTTPID2, ev.DataUnit.PID)
			assert.NotEqual(t, fxTTPID1, ev.DataUnit.PID)
			assert.Equal(t, []byte("fra"), ev.DataUnit.Language)
		}
	}
}

// TestTeletextExtractorMissingDescriptor feeds a PMT without any teletext
// descriptor and expects an explicit ErrTeletextNoDescriptor event.
func TestTeletextExtractorMissingDescriptor(t *testing.T) {
	pmt := &PMTData{
		ProgramNumber: fxProgram1,
		PCRPID:        fxTTPID1,
		ElementaryStreams: []*PMTElementaryStream{{
			ElementaryPID: fxTTPID1,
			StreamType:    StreamTypeADTS,
			ElementaryStreamDescriptors: []*Descriptor{{
				Tag:    DescriptorTagISO639LanguageAndAudioType,
				Length: 4,
				ISO639LanguageAndAudioType: &DescriptorISO639LanguageAndAudioType{
					Language: []byte("eng"), Type: 0,
				},
			}},
		}},
	}

	b := newTSBuilder()
	b.writePayload(0x0, buildPATSection(&PATData{
		TransportStreamID: fxTSID,
		Programs:          []*PATProgram{{ProgramNumber: fxProgram1, ProgramMapID: fxPMTPID1}},
	}))
	b.writePayload(fxPMTPID1, buildPMTSection(pmt))

	events, ext := drainExtractor(t, b.bytes(), TeletextSelector{ProgramNumber: fxProgram1})
	requireErrorEvent(t, events, ErrTeletextNoDescriptor)
	assert.Nil(t, ext.SelectedTrack())
	assert.Empty(t, dataUnitsFromEvents(events))
}

// TestTeletextExtractorNoMatch expects ErrTeletextNoMatch when no descriptor
// entry satisfies the language selector.
func TestTeletextExtractorNoMatch(t *testing.T) {
	fx := newTeletextFixture(t)
	events, ext := drainExtractor(t, fx.ts, TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("jpn"),
	})
	requireErrorEvent(t, events, ErrTeletextNoMatch)
	assert.Nil(t, ext.SelectedTrack())
	assert.Empty(t, dataUnitsFromEvents(events))
}

// TestTeletextExtractorMissingPTS builds a teletext PES without PTS and expects
// an explicit ErrTeletextMissingPTS event while data units keep being emitted.
func TestTeletextExtractorMissingPTS(t *testing.T) {
	header := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 0, 4, 0, bytes.Repeat([]byte{0xa1}, 36))
	unit := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 1, 4, 0, bytes.Repeat([]byte{0xb2}, 36))
	pesNoPTS := teletextPES(nil, header, unit)

	b := newTSBuilder()
	patBytes, pmtBytes := singleProgramTables(fxPMTPID1, fxTTPID1, fxProgram1, []*Descriptor{
		teletextDescriptor(&DescriptorTeletextItem{
			Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxEngMag, Page: fxEngPage,
		}),
	})
	b.writePayload(0x0, patBytes)
	b.writePayload(fxPMTPID1, pmtBytes)
	b.writePayload(fxTTPID1, pesBytes(pesNoPTS))

	events, _ := drainExtractor(t, b.bytes(), TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	requireErrorEvent(t, events, ErrTeletextMissingPTS)

	units := dataUnitsFromEvents(events)
	require.Len(t, units, 2)
	for _, u := range units {
		assert.False(t, u.PTSPresent)
		assert.Nil(t, u.PTS)
	}
}

// TestTeletextExtractorBadDataUnitLength crafts a data unit whose
// data_unit_length is not 44 and expects an explicit error event.
func TestTeletextExtractorBadDataUnitLength(t *testing.T) {
	good := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 0, 4, 0, bytes.Repeat([]byte{0xa1}, 36))
	bad := make([]byte, teletextDataUnitHeaderLength+10)
	bad[0] = TeletextDataUnitIDSubtitle
	bad[1] = 10 // invalid length, EN 300 472 mandates 44
	after := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 1, 4, 0, bytes.Repeat([]byte{0xc3}, 36))

	pes := teletextPES(&ClockReference{Base: 90000 * 10}, good, bad, after)

	b := newTSBuilder()
	patBytes, pmtBytes := singleProgramTables(fxPMTPID1, fxTTPID1, fxProgram1, []*Descriptor{
		teletextDescriptor(&DescriptorTeletextItem{
			Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxEngMag, Page: fxEngPage,
		}),
	})
	b.writePayload(0x0, patBytes)
	b.writePayload(fxPMTPID1, pmtBytes)
	b.writePayload(fxTTPID1, pesBytes(pes))

	events, _ := drainExtractor(t, b.bytes(), TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	requireErrorEvent(t, events, ErrTeletextBadDataUnitLength)

	// The good unit before the bad one is still delivered
	units := dataUnitsFromEvents(events)
	require.Len(t, units, 1)
	assert.Equal(t, good, units[0].Data)
}

// TestTeletextExtractorTruncatedDataUnit crafts a data unit claiming 44 bytes
// at the very end of a PES payload and expects an ErrTeletextTruncated event.
func TestTeletextExtractorTruncatedDataUnit(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteByte(0x10) // data_identifier
	payload.WriteByte(TeletextDataUnitIDSubtitle)
	payload.WriteByte(teletextDataUnitLength)
	payload.Write(make([]byte, 10)) // only 10 of the 44 data bytes are present

	h := &PESHeader{
		StreamID: StreamIDPrivateStream1,
		OptionalHeader: &PESOptionalHeader{
			PTS:             &ClockReference{Base: 90000},
			PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
		},
	}
	pes := &PESData{Header: h, Data: payload.Bytes()}

	b := newTSBuilder()
	patBytes, pmtBytes := singleProgramTables(fxPMTPID1, fxTTPID1, fxProgram1, []*Descriptor{
		teletextDescriptor(&DescriptorTeletextItem{
			Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxEngMag, Page: fxEngPage,
		}),
	})
	b.writePayload(0x0, patBytes)
	b.writePayload(fxPMTPID1, pmtBytes)
	b.writePayload(fxTTPID1, pesBytes(pes))

	events, _ := drainExtractor(t, b.bytes(), TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	requireErrorEvent(t, events, ErrTeletextTruncated)
	assert.Empty(t, dataUnitsFromEvents(events))
}

// TestTeletextExtractorBadDataIdentifier expects ErrTeletextBadDataIdentifier
// when the first PES payload byte is outside 0x10-0x1f.
func TestTeletextExtractorBadDataIdentifier(t *testing.T) {
	h := &PESHeader{
		StreamID: StreamIDPrivateStream1,
		OptionalHeader: &PESOptionalHeader{
			PTS:             &ClockReference{Base: 90000},
			PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
		},
	}
	pes := &PESData{Header: h, Data: []byte{0x42, TeletextDataUnitIDFiller}}

	b := newTSBuilder()
	patBytes, pmtBytes := singleProgramTables(fxPMTPID1, fxTTPID1, fxProgram1, []*Descriptor{
		teletextDescriptor(&DescriptorTeletextItem{
			Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxEngMag, Page: fxEngPage,
		}),
	})
	b.writePayload(0x0, patBytes)
	b.writePayload(fxPMTPID1, pmtBytes)
	b.writePayload(fxTTPID1, pesBytes(pes))

	events, _ := drainExtractor(t, b.bytes(), TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	requireErrorEvent(t, events, ErrTeletextBadDataIdentifier)
}

// TestTeletextExtractorContinuityBreak injects a TS packet with a wrong
// continuity counter on the teletext PID and expects an ErrTeletextContinuity
// event produced through the packet skipper hook.
func TestTeletextExtractorContinuityBreak(t *testing.T) {
	fx := newTeletextFixture(t)
	ts := fx.ts

	// Flip the continuity counter of the second packet of the teletext PID.
	// Locate the first two packets whose PID is the teletext PID.
	var positions []int
	for off := 0; off+MpegTsPacketSize <= len(ts); off += MpegTsPacketSize {
		pid := uint16(ts[off+1]&0x1f)<<8 | uint16(ts[off+2])
		if pid == fxTTPID1 {
			positions = append(positions, off)
			if len(positions) == 2 {
				break
			}
		}
	}
	require.Len(t, positions, 2)

	corrupted := make([]byte, len(ts))
	copy(corrupted, ts)
	// Break the counter: the packet normally carries cc = previous + 1, set it to +5
	corrupted[positions[1]+3] = corrupted[positions[1]+3] + 4

	ext := NewTeletextExtractor(TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	dmx := NewDemuxer(context.Background(), bytes.NewReader(corrupted),
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
	events = append(events, ext.DrainPending()...)
	requireErrorEvent(t, events, ErrTeletextContinuity)
}

// TestTeletextExtractorProgramChange rewrites the PAT mid-stream with a new
// PMT PID mapping and expects an ErrTeletextProgramChange event.
func TestTeletextExtractorProgramChange(t *testing.T) {
	fx := newTeletextFixture(t)

	// Append a second, different PAT referencing a new PMT PID for program 1.
	pat2 := &PATData{
		TransportStreamID: fxTSID,
		Programs: []*PATProgram{
			{ProgramNumber: fxProgram1, ProgramMapID: 0x1100},
			{ProgramNumber: fxProgram2, ProgramMapID: fxPMTPID2},
		},
	}
	b := newTSBuilder()
	b.writePayload(0x0, buildPATSection(pat2))
	ts := append(append([]byte(nil), fx.ts...), b.bytes()...)

	events, _ := drainExtractor(t, ts, TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	requireErrorEvent(t, events, ErrTeletextProgramChange)

	// Data extraction up to the change still worked
	assert.NotEmpty(t, dataUnitsFromEvents(events))
}

// TestTeletextExtractorPageFiltering verifies that a data unit of another page
// on the same magazine is rejected once the active page is known.
func TestTeletextExtractorPageFiltering(t *testing.T) {
	// Two pages on magazine 3: page 40 (selected) and page 41 (foreign)
	header40 := teletextUnit(TeletextDataUnitIDSubtitle, 3, 0, 4, 0, bytes.Repeat([]byte{0xa1}, 36))
	unit40 := teletextUnit(TeletextDataUnitIDSubtitle, 3, 1, 4, 0, bytes.Repeat([]byte{0xb2}, 36))
	header41 := teletextUnit(TeletextDataUnitIDSubtitle, 3, 0, 4, 1, bytes.Repeat([]byte{0xc3}, 36))
	unit41 := teletextUnit(TeletextDataUnitIDSubtitle, 3, 1, 4, 1, bytes.Repeat([]byte{0xd4}, 36))

	b := newTSBuilder()
	patBytes, pmtBytes := singleProgramTables(fxPMTPID1, fxTTPID1, fxProgram1, []*Descriptor{
		teletextDescriptor(&DescriptorTeletextItem{
			Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: 3, Page: 40,
		}),
	})
	b.writePayload(0x0, patBytes)
	b.writePayload(fxPMTPID1, pmtBytes)
	b.writePayload(fxTTPID1, pesBytes(teletextPES(&ClockReference{Base: 90000}, header40, unit40)))
	b.writePayload(fxTTPID1, pesBytes(teletextPES(&ClockReference{Base: 180000}, header41, unit41)))

	events, _ := drainExtractor(t, b.bytes(), TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	units := dataUnitsFromEvents(events)

	// Only page 40 units survive; the page 41 header/unit are filtered out
	require.Len(t, units, 2)
	assert.Equal(t, header40, units[0].Data)
	assert.Equal(t, unit40, units[1].Data)
	for _, u := range units {
		assert.Equal(t, uint8(40), u.Page)
	}
}

// TestTeletextExtractorFillerStuffing verifies that 0xff filler terminates the
// data unit loop and that trailing stuffing never gets parsed as data units.
func TestTeletextExtractorFillerStuffing(t *testing.T) {
	unit := teletextUnit(TeletextDataUnitIDSubtitle, fxEngMag, 1, 4, 0, bytes.Repeat([]byte{0xb2}, 36))

	var payload bytes.Buffer
	payload.WriteByte(0x10)
	payload.Write(unit)
	payload.WriteByte(TeletextDataUnitIDFiller)
	payload.Write(bytes.Repeat([]byte{0xff}, 30)) // PES stuffing

	h := &PESHeader{
		StreamID: StreamIDPrivateStream1,
		OptionalHeader: &PESOptionalHeader{
			PTS:             &ClockReference{Base: 90000},
			PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
		},
	}
	pes := &PESData{Header: h, Data: payload.Bytes()}

	b := newTSBuilder()
	patBytes, pmtBytes := singleProgramTables(fxPMTPID1, fxTTPID1, fxProgram1, []*Descriptor{
		teletextDescriptor(&DescriptorTeletextItem{
			Language: []byte("eng"), Type: TeletextTypeTeletextSubtitlePage, Magazine: fxEngMag, Page: fxEngPage,
		}),
	})
	b.writePayload(0x0, patBytes)
	b.writePayload(fxPMTPID1, pmtBytes)
	b.writePayload(fxTTPID1, pesBytes(pes))

	events, _ := drainExtractor(t, b.bytes(), TeletextSelector{
		ProgramNumber: fxProgram1,
		Language:      []byte("eng"),
	})
	units := dataUnitsFromEvents(events)
	require.Len(t, units, 1)
	assert.Equal(t, unit, units[0].Data)
	assert.Nil(t, errorFromEvents(events, ErrTeletextBadDataUnitLength))
}
