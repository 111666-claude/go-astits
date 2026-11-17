package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asticode/go-astits"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// teletextEventJSON mirrors the teletextOutput struct of the probe command.
type teletextEventJSON struct {
	Kind          string `json:"kind"`
	PID           uint16 `json:"pid"`
	ProgramNumber uint16 `json:"program_number"`
	Language      string `json:"language"`
	DataUnitID    uint8  `json:"data_unit_id"`
	Magazine      uint8  `json:"magazine"`
	Row           uint8  `json:"row"`
	Page          uint8  `json:"page"`
	PTS           int64  `json:"pts"`
	PTSPresent    bool   `json:"pts_present"`
	DataHex       string `json:"data_hex"`
	Error         string `json:"error"`
}

// hamming84 is a small Hamming-8/4 encoder for the EBU teletext address bytes.
// It is the inverse of the decoder used inside the library, defined here with
// precomputed codewords so this test stays independent of internals.
var hamming84 = [16]byte{
	0x15, 0x02, 0x49, 0x5e, 0x64, 0x73, 0x38, 0x2f,
	0xd0, 0xc7, 0x8c, 0x9b, 0xa1, 0xb6, 0xfd, 0xea,
}

// teletextUnit builds a 46-byte raw DVB teletext data unit with public types.
// For row 0 packets the BCD page tens/units digits are placed in the low nibbles
// of data bytes 2 and 3 (offsets 7 and 8 of the unit).
func teletextUnit(unitID, magazine, row, pageTens, pageUnits uint8, body []byte) []byte {
	if magazine == 8 {
		magazine = 0
	}
	nibble1 := (magazine & 0x07) | ((row & 0x01) << 3)
	nibble2 := (row >> 1) & 0x0f
	u := make([]byte, 2+44)
	u[0] = unitID
	u[1] = 44
	u[2] = 0x55 // clock run-in
	u[3] = 0x55
	u[4] = 0x27 // framing code
	u[5] = hamming84[nibble1]
	u[6] = hamming84[nibble2]
	if row == 0 {
		u[7] = 0x80 | (pageTens & 0x0f)
		u[8] = 0x80 | (pageUnits & 0x0f)
	}
	copy(u[9:], body)
	return u
}

// teletextPESData builds the data_identifier + data units + filler payload of
// a DVB teletext PES packet (EN 300 472).
func teletextPESData(units ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteByte(0x10) // data_identifier: EBU teletext
	for _, u := range units {
		b.Write(u)
	}
	b.WriteByte(astits.TeletextDataUnitIDFiller)
	return b.Bytes()
}

// buildSingleProgramTS muxes a one-program stream with two teletext languages
// (eng page 340, deu page 577) using only the public muxer API, and returns
// the serialized transport stream.
func buildSingleProgramTS(t *testing.T) []byte {
	t.Helper()

	const ttPID = uint16(0x0101)
	engDesc := &astits.Descriptor{
		Tag:    astits.DescriptorTagTeletext,
		Length: 10, // 2 entries x 5 bytes
		Teletext: &astits.DescriptorTeletext{Items: []*astits.DescriptorTeletextItem{
			{Language: []byte("eng"), Type: astits.TeletextTypeTeletextSubtitlePage, Magazine: 3, Page: 40},
			{Language: []byte("deu"), Type: astits.TeletextTypeTeletextSubtitlePage, Magazine: 5, Page: 77},
		}},
	}

	var out bytes.Buffer
	muxer := astits.NewMuxer(context.Background(), &out)
	require.NoError(t, muxer.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID:               ttPID,
		StreamType:                  astits.StreamTypeMPEG2PacketizedData,
		ElementaryStreamDescriptors: []*astits.Descriptor{engDesc},
	}))
	muxer.SetPCRPID(ttPID)
	_, err := muxer.WriteTables()
	require.NoError(t, err)

	// English PES #1: row 0 page header then a subtitle row, spanning packets
	engHeader := teletextUnit(astits.TeletextDataUnitIDSubtitle, 3, 0, 4, 0, bytes.Repeat([]byte{0xa1}, 36))
	engRow1 := teletextUnit(astits.TeletextDataUnitIDSubtitle, 3, 1, 4, 0, bytes.Repeat([]byte{0xb2}, 36))
	pes1 := &astits.PESData{
		Header: &astits.PESHeader{
			StreamID: astits.StreamIDPrivateStream1,
			OptionalHeader: &astits.PESOptionalHeader{
				PTS:             &astits.ClockReference{Base: 90000 * 10},
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
			},
		},
		Data: teletextPESData(engHeader, engRow1,
			teletextUnit(astits.TeletextDataUnitIDNonSubtitle, 3, 4, 4, 0, bytes.Repeat([]byte{0xd4}, 36))),
	}
	_, err = muxer.WriteData(&astits.MuxerData{PID: ttPID, PES: pes1})
	require.NoError(t, err)

	// English PES #2, later PTS
	engRow2 := teletextUnit(astits.TeletextDataUnitIDSubtitle, 3, 2, 4, 0, bytes.Repeat([]byte{0xc3}, 36))
	pes2 := &astits.PESData{
		Header: &astits.PESHeader{
			StreamID: astits.StreamIDPrivateStream1,
			OptionalHeader: &astits.PESOptionalHeader{
				PTS:             &astits.ClockReference{Base: 90000 * 12},
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
			},
		},
		Data: teletextPESData(engRow2),
	}
	_, err = muxer.WriteData(&astits.MuxerData{PID: ttPID, PES: pes2})
	require.NoError(t, err)

	// German PES
	deuHeader := teletextUnit(astits.TeletextDataUnitIDSubtitle, 5, 0, 7, 7, bytes.Repeat([]byte{0xe5}, 36))
	deuRow1 := teletextUnit(astits.TeletextDataUnitIDSubtitle, 5, 1, 7, 7, bytes.Repeat([]byte{0xf6}, 36))
	pesDeu := &astits.PESData{
		Header: &astits.PESHeader{
			StreamID: astits.StreamIDPrivateStream1,
			OptionalHeader: &astits.PESOptionalHeader{
				PTS:             &astits.ClockReference{Base: 90000 * 20},
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
			},
		},
		Data: teletextPESData(deuHeader, deuRow1),
	}
	_, err = muxer.WriteData(&astits.MuxerData{PID: ttPID, PES: pesDeu})
	require.NoError(t, err)

	return out.Bytes()
}

// buildProbe compiles the probe binary once and returns its path.
func buildProbe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "astits-probe")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building probe failed: %v\n%s", err, out)
	}
	return bin
}

// runProbe runs the probe teletext command against the given TS file.
func runProbe(t *testing.T, bin, tsPath string, args ...string) []teletextEventJSON {
	t.Helper()
	full := append([]string{"teletext", "-i", tsPath}, args...)
	cmd := exec.Command(bin, full...)
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, "probe output: %s", raw)

	var events []teletextEventJSON
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev teletextEventJSON
		require.NoError(t, json.Unmarshal([]byte(line), &ev), "line: %s", line)
		events = append(events, ev)
	}
	return events
}

func TestProbeTeletextEnglish(t *testing.T) {
	dir := t.TempDir()
	tsPath := filepath.Join(dir, "in.ts")
	require.NoError(t, os.WriteFile(tsPath, buildSingleProgramTS(t), 0o644))

	events := runProbe(t, buildProbe(t), tsPath, "-teletext-language", "eng")

	var units []teletextEventJSON
	for _, ev := range events {
		assert.NotEqual(t, "error", ev.Kind, "unexpected error: %+v", ev)
		if ev.Kind == "data_unit" {
			units = append(units, ev)
		}
	}
	// Header + row1 (PES #1) and row2 (PES #2); the non-subtitle unit is filtered
	require.Len(t, units, 3)
	for _, u := range units {
		assert.Equal(t, "eng", u.Language)
		assert.Equal(t, uint8(3), u.Magazine)
		assert.Equal(t, uint8(40), u.Page)
		assert.True(t, u.PTSPresent)
		assert.Len(t, u.DataHex, 92) // 46 raw bytes per data unit
		// Hex must round-trip back to 46 bytes
		raw, err := hex.DecodeString(u.DataHex)
		require.NoError(t, err)
		assert.Len(t, raw, 46)
	}
	assert.Equal(t, int64(90000*10), units[0].PTS)
	assert.Equal(t, int64(90000*12), units[2].PTS)
	assert.Equal(t, uint8(0), units[0].Row)
	assert.Equal(t, uint8(1), units[1].Row)
	assert.Equal(t, uint8(2), units[2].Row)
}

func TestProbeTeletextGerman(t *testing.T) {
	dir := t.TempDir()
	tsPath := filepath.Join(dir, "in.ts")
	require.NoError(t, os.WriteFile(tsPath, buildSingleProgramTS(t), 0o644))

	events := runProbe(t, buildProbe(t), tsPath, "-teletext-language", "deu")

	var units []teletextEventJSON
	for _, ev := range events {
		if ev.Kind == "data_unit" {
			units = append(units, ev)
		}
	}
	require.Len(t, units, 2)
	for _, u := range units {
		assert.Equal(t, "deu", u.Language)
		assert.Equal(t, uint8(5), u.Magazine)
		assert.Equal(t, uint8(77), u.Page)
		assert.Equal(t, int64(90000*20), u.PTS)
	}
}

func TestProbeTeletextNoMatch(t *testing.T) {
	dir := t.TempDir()
	tsPath := filepath.Join(dir, "in.ts")
	require.NoError(t, os.WriteFile(tsPath, buildSingleProgramTS(t), 0o644))

	events := runProbe(t, buildProbe(t), tsPath, "-teletext-language", "jpn")

	var sawNoMatch bool
	for _, ev := range events {
		if ev.Kind == "error" && strings.Contains(ev.Error, "no teletext track matches") {
			sawNoMatch = true
		}
	}
	assert.True(t, sawNoMatch)
}

func TestProbeTeletextDefaultLanguage(t *testing.T) {
	// With no language flag the first descriptor entry (eng) is selected.
	dir := t.TempDir()
	tsPath := filepath.Join(dir, "in.ts")
	require.NoError(t, os.WriteFile(tsPath, buildSingleProgramTS(t), 0o644))

	events := runProbe(t, buildProbe(t), tsPath)
	for _, ev := range events {
		if ev.Kind == "data_unit" {
			assert.Equal(t, "eng", ev.Language)
		}
	}
}
