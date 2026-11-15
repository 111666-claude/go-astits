package astits

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tdtSectionBytes builds a raw TDT section (table header + 5-byte UTC time), mirroring how a
// real broadcast encodes it: table_id 0x70, section_syntax_indicator 0, private bit 1,
// section_length 5.
func tdtSectionBytes(timeBytes []byte) []byte {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write(uint8(PSITableIDTDT)) // table id
	w.Write("0")                  // section_syntax_indicator
	w.Write("1")                  // private / reserved_future_use
	w.Write("11")                 // reserved
	w.WriteN(uint16(5), 12)       // section length
	w.Write(timeBytes)            // UTC time
	return buf.Bytes()
}

func TestParseTDTSection(t *testing.T) {
	h := &PSISectionHeader{TableID: PSITableIDTDT, SectionLength: tdtSectionLength}
	d, err := parseTDTSection(astikit.NewBytesIterator(dvbTimeBytes), h)
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.True(t, d.UTCTime.Equal(dvbTime))
	assert.Equal(t, time.UTC, d.UTCTime.Location())
}

func TestParseTDTSection_InvalidLength(t *testing.T) {
	for _, length := range []uint16{0, 4, 6, 1021} {
		h := &PSISectionHeader{TableID: PSITableIDTDT, SectionLength: length}
		_, err := parseTDTSection(astikit.NewBytesIterator(dvbTimeBytes), h)
		assert.True(t, errors.Is(err, ErrTDTSectionInvalid))
	}
}

func TestParseTDTSection_TruncatedPayload(t *testing.T) {
	h := &PSISectionHeader{TableID: PSITableIDTDT, SectionLength: tdtSectionLength}
	// Header promises 5 bytes but only 3 are present.
	_, err := parseTDTSection(astikit.NewBytesIterator([]byte{0xc0, 0x79, 0x12}), h)
	assert.Error(t, err)
}

func TestParseTDTSection_BadBCD(t *testing.T) {
	h := &PSISectionHeader{TableID: PSITableIDTDT, SectionLength: tdtSectionLength}
	bad := []byte{0xc0, 0x79, 0x25, 0x45, 0x00} // hour 25
	_, err := parseTDTSection(astikit.NewBytesIterator(bad), h)
	assert.True(t, errors.Is(err, ErrDVBTimeInvalidBCD))
}

func TestWriteParseTDTSection_RoundTrip(t *testing.T) {
	cases := []string{
		"1993-10-13 12:45:00",
		"2024-02-29 23:59:59", // leap day, end of day
		"2024-03-01 00:00:00", // crossed midnight
		"2000-02-29 00:00:00", // leap day, midnight
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			want := utcDate(c)

			// Write through the generic PSI section writer.
			section := &PSISection{
				Header: &PSISectionHeader{
					PrivateBit:             true,
					SectionLength:          tdtSectionLength,
					SectionSyntaxIndicator: false,
					TableID:                PSITableIDTDT,
					TableType:              PSITableTypeTDT,
				},
				Syntax: &PSISectionSyntax{Data: &PSISectionSyntaxData{TDT: &TDTData{UTCTime: want}}},
			}
			buf := &bytes.Buffer{}
			w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
			n, err := writePSISection(w, section)
			require.NoError(t, err)
			assert.Equal(t, 3+5, n) // 3-byte header + 5-byte time
			assert.Equal(t, tdtSectionBytes(dvbTimeStrictBytes(t, want)), buf.Bytes())

			// Parse back through the generic PSI parser.
			psi, err := parsePSIData(astikit.NewBytesIterator(append([]byte{0x00}, buf.Bytes()...)))
			require.NoError(t, err)
			require.Len(t, psi.Sections, 1)
			got := psi.Sections[0].Syntax.Data.TDT
			require.NotNil(t, got)
			assert.True(t, got.UTCTime.Equal(want))
		})
	}
}

func TestWritePSISection_TDTRejectsUnrepresentableTime(t *testing.T) {
	section := &PSISection{
		Header: &PSISectionHeader{TableID: PSITableIDTDT, SectionLength: tdtSectionLength},
		Syntax: &PSISectionSyntax{Data: &PSISectionSyntaxData{TDT: &TDTData{UTCTime: utcDate("1800-01-01 00:00:00")}}},
	}
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	_, err := writePSISection(w, section)
	assert.True(t, errors.Is(err, ErrDVBTimeOutOfRange))
}

// TestParsePSIData_TDT checks the full PSIData dispatch exposes a TDT on DemuxerData.
func TestParsePSIData_TDT(t *testing.T) {
	raw := append([]byte{0x00}, tdtSectionBytes(dvbTimeBytes)...)
	psi, err := parsePSIData(astikit.NewBytesIterator(raw))
	require.NoError(t, err)
	require.Len(t, psi.Sections, 1)

	s := psi.Sections[0]
	assert.Equal(t, PSITableIDTDT, s.Header.TableID)
	assert.Equal(t, PSITableTypeTDT, s.Header.TableType)
	assert.False(t, s.Header.SectionSyntaxIndicator)
	assert.True(t, s.Header.PrivateBit)
	assert.Equal(t, uint16(5), s.Header.SectionLength)

	ds := psi.toData(&Packet{}, PIDTDT)
	require.Len(t, ds, 1)
	assert.Equal(t, PIDTDT, ds[0].PID)
	require.NotNil(t, ds[0].TDT)
	assert.True(t, ds[0].TDT.UTCTime.Equal(dvbTime))
}

func dvbTimeStrictBytes(t *testing.T, tt time.Time) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	_, err := writeDVBTimeStrict(w, tt.UTC())
	require.NoError(t, err)
	return buf.Bytes()
}
