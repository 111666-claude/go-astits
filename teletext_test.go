package astits

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Fixture builders -------------------------------------------------------

// hamming84EncodeForTest returns the EN 300 706 Hamming 8/4 codeword of nibble.
func hamming84EncodeForTest(nibble byte) byte {
	return hamming84Encode[nibble&0x0f]
}

// ttxAddressBytes builds the two Hamming 8/4 address bytes of a teletext
// packet (magazine 1-8, row 0-31).
func ttxAddressBytes(magazine, row byte) [2]byte {
	m := magazine
	if m == 8 {
		m = 0
	}
	return [2]byte{
		hamming84EncodeForTest((m & 0x07) | (row&0x10)>>1),
		hamming84EncodeForTest(row & 0x0f),
	}
}

// ttxHeaderPacket builds the 42-byte row-0 packet for page units/tens.
func ttxHeaderPacket(magazine, units, tens byte, subpage uint16, display string) []byte {
	p := make([]byte, teletextPacketSize)
	addr := ttxAddressBytes(magazine, 0)
	p[0], p[1] = addr[0], addr[1]
	p[2] = hamming84EncodeForTest(units)
	p[3] = hamming84EncodeForTest(tens)
	p[4] = hamming84EncodeForTest(0) // control nibble C11-C14
	p[5] = hamming84EncodeForTest(byte(subpage>>8) & 0x0f)
	p[6] = hamming84EncodeForTest(byte(subpage>>4) & 0x0f)
	p[7] = hamming84EncodeForTest(byte(subpage) & 0x0f)
	copy(p[8:], padTTXText(display))
	return p
}

// ttxDisplayPacket builds the 42-byte packet for one display row.
func ttxDisplayPacket(magazine, row byte, text string) []byte {
	p := make([]byte, teletextPacketSize)
	addr := ttxAddressBytes(magazine, row)
	p[0], p[1] = addr[0], addr[1]
	copy(p[2:], padTTXText(text))
	return p
}

func padTTXText(s string) []byte {
	b := make([]byte, teletextLineColumns)
	for idx := range b {
		b[idx] = 0x20
	}
	copy(b, s)
	return b
}

// ttxDataUnit wraps a 42-byte packet into a 44-byte data unit.
func ttxDataUnit(id byte, packet []byte, control byte) []byte {
	u := make([]byte, 2+teletextDataUnitLength)
	u[0] = id
	u[1] = teletextDataUnitLength
	u[2] = control
	u[3] = teletextFramingCode
	copy(u[4:], packet)
	return u
}

// ttxPES builds a private_stream_1 teletext PES via the library's own PES
// writer, so fixtures exercise parsePESData as well.
func ttxPES(payload []byte) *PESData {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	header := &PESHeader{
		StreamID: StreamIDPrivateStream1,
		OptionalHeader: &PESOptionalHeader{
			HasOptionalFields: true,
			MarkerBits:        0x2,
			PTSDTSIndicator:   PTSDTSIndicatorOnlyPTS,
			PTS:               newClockReference(90000, 0),
		},
	}
	if _, err := writePESHeader(w, header, len(payload)); err != nil {
		panic(err)
	}
	if err := w.Write(payload); err != nil {
		panic(err)
	}

	d, err := parsePESData(astikit.NewBytesIterator(buf.Bytes()))
	if err != nil {
		panic(err)
	}
	return d
}

// ttxPESPayload assembles data units behind a DVB teletext data_identifier.
func ttxPESPayload(dataIdentifier byte, units ...[]byte) []byte {
	p := []byte{dataIdentifier}
	for _, u := range units {
		p = append(p, u...)
	}
	return p
}

// ---- Hamming 8/4 -----------------------------------------------------------

func TestTeletextHamming84(t *testing.T) {
	// Round trip of every nibble.
	for n := 0; n < 16; n++ {
		code := hamming84EncodeForTest(byte(n))
		got, corrected, ok := decodeHamming84(code)
		require.True(t, ok)
		assert.False(t, corrected)
		assert.Equal(t, byte(n), got)
	}

	// Every single-bit flip must be corrected to the original nibble.
	for n := 0; n < 16; n++ {
		code := hamming84EncodeForTest(byte(n))
		for bit := 0; bit < 8; bit++ {
			got, corrected, ok := decodeHamming84(code ^ (1 << bit))
			require.True(t, ok, "nibble %x bit %d", n, bit)
			assert.True(t, corrected, "nibble %x bit %d", n, bit)
			assert.Equal(t, byte(n), got)
		}
	}

	// The codeword set has minimum distance 4: every double-bit flip must be
	// uncorrectable, never silently decoded to another nibble.
	uncorrectable := 0
	for n := 0; n < 16; n++ {
		code := hamming84EncodeForTest(byte(n))
		for b1 := 0; b1 < 8; b1++ {
			for b2 := b1 + 1; b2 < 8; b2++ {
				_, _, ok := decodeHamming84(code ^ (1 << b1) ^ (1 << b2))
				if !ok {
					uncorrectable++
				}
			}
		}
	}
	assert.Equal(t, 16*28, uncorrectable)

	// Patterns outside the 144 codeword/neighbour entries (e.g. 0x01) cannot be
	// decoded.
	_, _, ok := decodeHamming84(0x01)
	assert.False(t, ok)
}

// ---- Two pages, three rounds of updates ------------------------------------

func TestTeletextDecoderTwoPagesThreeRounds(t *testing.T) {
	dec := NewTeletextDecoder()

	// Round 1: page 100 announces itself with two display rows; page 300 then
	// announces itself with one row. A duplicate row is sent to prove repeated
	// identical rows produce no update.
	p100Header := ttxHeaderPacket(1, 0, 0, 0x123, "HEADER 100")
	p100Row1 := ttxDisplayPacket(1, 1, "HELLO SUBTITLE")
	p100Row2 := ttxDisplayPacket(1, 2, "SECOND LINE")
	p300Header := ttxHeaderPacket(3, 0, 0, 0x001, "HEADER 300")
	p300Row1 := ttxDisplayPacket(3, 1, "PAGE 300 LINE 1")

	pes1 := ttxPES(ttxPESPayload(0x10,
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Header, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Row1, 0x20),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Row2, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextNonSubtitle, p300Header, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextNonSubtitle, p300Row1, 0x00),
		// Repeated row 1: same raw bytes, must be ignored.
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Row1, 0x20),
	))

	updates, err := dec.FeedPES(pes1)
	require.NoError(t, err)
	assert.Len(t, updates, 5)
	assert.Equal(t, []TeletextPageID{
		{Magazine: 1, Page: 0},
		{Magazine: 3, Page: 0},
	}, dec.PageIDs())

	page100, ok := dec.Page(TeletextPageID{1, 0})
	require.True(t, ok)
	assert.Equal(t, uint16(0x123), page100.Subpage)
	assert.Equal(t, []uint8{0, 1, 2}, page100.Rows())

	row0, ok := page100.Line(0)
	require.True(t, ok)
	// Row 0 first six columns are control bytes, rendered as spaces; the header
	// text starts at column 6.
	assert.Equal(t, "      HEADER 100", row0.Text)
	assert.Equal(t, 0, row0.Corrected)
	assert.Equal(t, 1, row0.PESIndex)
	assert.True(t, row0.FieldParity == false)

	row1, ok := page100.Line(1)
	require.True(t, ok)
	assert.Equal(t, "HELLO SUBTITLE", row1.Text)
	assert.True(t, row1.FieldParity)
	assert.Equal(t, uint8(0), row1.LineOffset)

	page300, ok := dec.Page(TeletextPageID{3, 0})
	require.True(t, ok)
	row, ok := page300.Line(1)
	require.True(t, ok)
	assert.Equal(t, "PAGE 300 LINE 1", row.Text)
	assert.Equal(t, uint16(0x001), page300.Subpage)

	// Raw bytes are retained verbatim for later review.
	assert.Equal(t, p100Row1[2:], row1.Raw)

	// Round 2: page 100 row 1 changes (with a leading alpha-red control code),
	// page 300 row 1 changes, page 100 row 2 stays the same (no update).
	p100Row1v2 := ttxDisplayPacket(1, 1, string([]byte{0x01})+"UPDATED SUBTITLE")
	p300Row1v2 := ttxDisplayPacket(3, 1, "PAGE 300 V2")
	pes2 := ttxPES(ttxPESPayload(0x13,
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Row1v2, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextNonSubtitle, p300Row1v2, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Row2, 0x00),
	))

	updates, err = dec.FeedPES(pes2)
	require.NoError(t, err)
	assert.Len(t, updates, 2)
	assert.Equal(t, TeletextPageID{1, 0}, updates[0].PageID)
	assert.Equal(t, uint8(1), updates[0].Line.Row)
	assert.Equal(t, 2, updates[0].Line.PESIndex)

	page100, _ = dec.Page(TeletextPageID{1, 0})
	row1, _ = page100.Line(1)
	assert.Equal(t, " UPDATED SUBTITLE", row1.Text)
	require.Len(t, row1.Controls, 1)
	assert.Equal(t, TeletextControlAlphaColor, row1.Controls[0].Kind)
	color, isColor := row1.Controls[0].Color()
	assert.True(t, isColor)
	assert.Equal(t, 1, color)
	assert.Equal(t, 0, row1.Controls[0].Position)

	// Round 3: a new subpage of page 100 plus a size control, and a G1 mosaic
	// character that has no G0 mapping (kept raw, rendered as replacement).
	p100HeaderV2 := ttxHeaderPacket(1, 0, 0, 0x456, "HEADER 100 V2")
	p100Row3 := ttxDisplayPacket(1, 3, string([]byte{'A', 0x0e, 'B', 0x0c, 0xa1, 'C'}))
	p300Row2 := ttxDisplayPacket(3, 2, "300 SECOND")
	pes3 := ttxPES(ttxPESPayload(0x10,
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100HeaderV2, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, p100Row3, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextNonSubtitle, p300Row2, 0x00),
		[]byte{TeletextDataUnitIDStuffing, 0x20, 0xff, 0xff},
	))

	updates, err = dec.FeedPES(pes3)
	require.NoError(t, err)
	// header row 0 changes, new row 3, new page 300 row 2: 3 updates.
	assert.Len(t, updates, 3)
	assert.NoError(t, dec.Flush())
	assert.Empty(t, dec.Errors())

	page100, _ = dec.Page(TeletextPageID{1, 0})
	assert.Equal(t, uint16(0x456), page100.Subpage)
	row0, _ = page100.Line(0)
	assert.Equal(t, "      HEADER 100 V2", row0.Text)
	row3, ok := page100.Line(3)
	require.True(t, ok)
	// 'A', double-width size -> space, 'B' with its blank half-cell, normal
	// size, mosaic 0xa1 -> replacement char, 'C'.
	assert.Equal(t, "A B �C", row3.Text)
	require.Len(t, row3.Controls, 2)
	assert.Equal(t, TeletextControlSize, row3.Controls[0].Kind)
	assert.Equal(t, TeletextControlSize, row3.Controls[1].Kind)
	assert.Equal(t, 1, row3.Unsupported)
	assert.Equal(t, byte(0xa1), row3.Raw[4])

	page300, _ = dec.Page(TeletextPageID{3, 0})
	assert.Equal(t, "      HEADER 300\nPAGE 300 V2\n300 SECOND", page300.Text())

	// Full page 100 export keeps rows in ascending order.
	assert.Equal(t, []string{
		"      HEADER 100 V2",
		" UPDATED SUBTITLE",
		"SECOND LINE",
		"A B �C",
	}, strings.Split(page100.Text(), "\n"))
}

// ---- Error handling ---------------------------------------------------------

func TestTeletextDecoderErrors(t *testing.T) {
	t.Run("bad data identifier", func(t *testing.T) {
		dec := NewTeletextDecoder()
		_, err := dec.FeedPES(ttxPES([]byte{0x05, 0x02, 0x2c}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextMalformed))
		require.Len(t, dec.Errors(), 1)
		assert.True(t, errors.Is(dec.Errors()[0], ErrTeletextMalformed))
	})

	t.Run("wrong stream id", func(t *testing.T) {
		dec := NewTeletextDecoder()
		pes := ttxPES([]byte{0x10})
		pes.Header.StreamID = 0xe0 // video
		_, err := dec.FeedPES(pes)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextMalformed))
	})

	t.Run("nil pes", func(t *testing.T) {
		dec := NewTeletextDecoder()
		_, err := dec.FeedPES(nil)
		require.Error(t, err)
		_, err = dec.FeedPES(&PESData{})
		require.Error(t, err)
	})

	t.Run("empty pes payload", func(t *testing.T) {
		dec := NewTeletextDecoder()
		_, err := dec.FeedPES(ttxPES(nil))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextTruncated))
	})

	t.Run("bad subpage hamming", func(t *testing.T) {
		dec := NewTeletextDecoder()
		hdr := ttxHeaderPacket(1, 0, 0, 0, "H")
		hdr[7] ^= 0x03 ^ 0x0c // double-bit flip on the S1 subpage byte
		_, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00))))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextHamming))
	})

	t.Run("uncorrectable row nibble", func(t *testing.T) {
		dec := NewTeletextDecoder()
		pkt := ttxDisplayPacket(1, 1, "X")
		pkt[1] = 0x01 // 0x01 is not a codeword nor a single-bit neighbour
		_, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, pkt, 0x00))))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextHamming))
	})

	t.Run("error string and unwrap", func(t *testing.T) {
		e := newTeletextError(ErrTeletextHamming, "address", "bad byte")
		assert.Contains(t, e.Error(), "uncorrectable teletext Hamming 8/4 error")
		assert.Contains(t, e.Error(), "address")
		assert.True(t, errors.Is(e, ErrTeletextHamming))
	})

	t.Run("bad framing code", func(t *testing.T) {
		dec := NewTeletextDecoder()
		pkt := ttxDisplayPacket(1, 1, "X")
		bad := ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, pkt, 0x00)
		bad[3] = 0x00 // framing code byte
		updates, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10, bad)))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextMalformed))
		assert.Empty(t, updates)
	})

	t.Run("uncorrectable address hamming", func(t *testing.T) {
		dec := NewTeletextDecoder()
		pkt := ttxDisplayPacket(1, 1, "X")
		addr := pkt[0]
		pkt[0] = addr ^ 0x03 ^ 0x0c // two-bit flip: uncorrectable
		_, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, pkt, 0x00))))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextHamming))
	})

	t.Run("correctable address hamming still produces a line", func(t *testing.T) {
		dec := NewTeletextDecoder()
		hdr := ttxHeaderPacket(1, 0, 0, 0, "H")
		pkt := ttxDisplayPacket(1, 1, "RECOVERED")
		pkt[1] ^= 0x01 // single-bit flip, must be repaired
		updates, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00),
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, pkt, 0x00))))
		require.NoError(t, err)
		require.Len(t, updates, 2)
		line := updates[1].Line
		assert.Equal(t, "RECOVERED", line.Text)
		assert.Equal(t, 1, line.Corrected)
	})

	t.Run("bad page number hamming", func(t *testing.T) {
		dec := NewTeletextDecoder()
		hdr := ttxHeaderPacket(1, 0, 0, 0, "H")
		hdr[2] ^= 0x03 ^ 0x0c // double-bit flip on page units byte
		_, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00))))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextHamming))
	})

	t.Run("illegal bcd page number", func(t *testing.T) {
		dec := NewTeletextDecoder()
		hdr := ttxHeaderPacket(1, 0x0a, 0, 0, "H") // units nibble 0xA
		_, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00))))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextInvalidPage))
	})

	t.Run("display row without page header", func(t *testing.T) {
		dec := NewTeletextDecoder()
		pkt := ttxDisplayPacket(2, 5, "ORPHAN")
		_, err := dec.FeedPES(ttxPES(ttxPESPayload(0x12,
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, pkt, 0x00))))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextInvalidPage))
		assert.Empty(t, dec.PageIDs())
	})

	t.Run("illegal teletext data unit length", func(t *testing.T) {
		dec := NewTeletextDecoder()
		// A complete but wrongly-sized teletext unit (length 16 instead of 44).
		pes := ttxPES([]byte{0x10, TeletextDataUnitIDEBUTeletextSubtitle, 0x10,
			0x00, 0xe4, 0x2c, 0x4f, 0x20, 0x20, 0x20, 0x20,
			0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20})
		_, err := dec.FeedPES(pes)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTeletextMalformed))
	})

	t.Run("non teletext units and unknown ids are skipped", func(t *testing.T) {
		dec := NewTeletextDecoder()
		hdr := ttxHeaderPacket(1, 0, 0, 0, "H")
		payload := ttxPESPayload(0x10,
			[]byte{0x06, 0x02, 0xaa, 0xbb}, // VPS-like unit, ignored
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00),
		)
		updates, err := dec.FeedPES(ttxPES(payload))
		require.NoError(t, err)
		assert.Len(t, updates, 1)
	})
}

// ---- Data units spanning PES packets ---------------------------------------

func TestTeletextDecoderDataUnitSpansPES(t *testing.T) {
	dec := NewTeletextDecoder()
	hdr := ttxHeaderPacket(1, 0, 0, 0, "SPLIT HEADER")
	unit := ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00)

	// First PES: data_identifier + data unit id/length + first 10 bytes.
	first := ttxPES(append([]byte{0x10}, unit[:12]...))
	updates, err := dec.FeedPES(first)
	require.NoError(t, err)
	assert.Empty(t, updates)
	require.Len(t, dec.Errors(), 0)

	// Second PES: its own data_identifier, the rest of the split unit plus a
	// complete second unit.
	row := ttxDisplayPacket(1, 4, "ARRIVED LATER")
	unit2 := ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, row, 0x00)
	secondPayload := append([]byte{0x10}, unit[12:]...)
	secondPayload = append(secondPayload, unit2...)
	updates, err = dec.FeedPES(ttxPES(secondPayload))
	require.NoError(t, err)
	assert.Len(t, updates, 2)
	assert.Equal(t, uint8(0), updates[0].Line.Row)
	assert.Equal(t, "      SPLIT HEADER", updates[0].Line.Text)
	assert.Equal(t, uint8(4), updates[1].Line.Row)
	assert.Equal(t, "ARRIVED LATER", updates[1].Line.Text)

	// Also cover the split landing between the unit id and length bytes.
	dec2 := NewTeletextDecoder()
	_, err = dec2.FeedPES(ttxPES([]byte{0x11, TeletextDataUnitIDEBUTeletextSubtitle}))
	require.NoError(t, err)
	updates, err = dec2.FeedPES(ttxPES(append([]byte{0x11, teletextDataUnitLength}, unit[2:]...)))
	require.NoError(t, err)
	assert.Len(t, updates, 1)
	assert.NoError(t, dec2.Flush())

	// The continuation PES must still start with a valid data_identifier.
	dec3 := NewTeletextDecoder()
	_, err = dec3.FeedPES(ttxPES([]byte{0x10, TeletextDataUnitIDEBUTeletextSubtitle}))
	require.NoError(t, err)
	_, err = dec3.FeedPES(ttxPES([]byte{0x05, teletextDataUnitLength}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTeletextMalformed))
}

// A data unit cut between PES packets is buffered up to the carry limit and
// reported as truncated at end of stream instead of being silently dropped.
func TestTeletextDecoderCarryAndTruncation(t *testing.T) {
	dec := NewTeletextDecoder()
	// data_identifier + a unit header claiming 255 bytes, only 5 of which arrive.
	payload := []byte{0x10, 0x03, 0xff, 0x00, 0xe4, 0x2c, 0x4f, 0x20}
	updates, err := dec.FeedPES(ttxPES(payload))
	require.NoError(t, err)
	assert.Empty(t, updates)

	// More bytes of the same unit arrive but it is still incomplete: they keep
	// accumulating behind the unit header rather than being reparsed as units.
	updates, err = dec.FeedPES(ttxPES(append([]byte{0x10}, make([]byte, 10)...)))
	require.NoError(t, err)
	assert.Empty(t, updates)

	err = dec.Flush()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTeletextTruncated))
}

// ---- End of stream / truncation --------------------------------------------

func TestTeletextDecoderFlushTruncated(t *testing.T) {
	dec := NewTeletextDecoder()
	_, err := dec.FeedPES(ttxPES([]byte{0x10, TeletextDataUnitIDEBUTeletextSubtitle, 44, 0x00, 0xe4}))
	require.NoError(t, err) // incomplete unit waits for more data
	err = dec.Flush()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTeletextTruncated))
	// Flush drains the carry: a second flush is a no-op.
	assert.NoError(t, dec.Flush())
	require.Len(t, dec.Errors(), 1)
}

// ---- Page cache limit -------------------------------------------------------

func TestTeletextDecoderCacheLimit(t *testing.T) {
	dec := NewTeletextDecoder(TeletextDecoderOptMaxPages(2))

	feedHeader := func(magazine, page byte) ([]TeletextLineUpdate, error) {
		hdr := ttxHeaderPacket(magazine, page%10, page/10, 0, fmt.Sprintf("P%d", page))
		return dec.FeedPES(ttxPES(ttxPESPayload(0x10|(magazine-1),
			ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00))))
	}

	_, err := feedHeader(1, 0) // page 100
	require.NoError(t, err)
	_, err = feedHeader(2, 0) // page 200, cache now full
	require.NoError(t, err)
	updates, err := feedHeader(3, 0) // page 300 evicts page 100
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTeletextCacheFull))
	// The row 0 update of the newly cached page is still delivered.
	require.Len(t, updates, 1)
	assert.Equal(t, TeletextPageID{3, 0}, updates[0].PageID)
	assert.Equal(t, uint8(0), updates[0].Line.Row)
	errs := dec.Errors()
	require.Len(t, errs, 1)
	assert.True(t, errors.Is(errs[0], ErrTeletextCacheFull))
	assert.Equal(t, []TeletextPageID{{2, 0}, {3, 0}}, dec.PageIDs())
	if _, ok := dec.Page(TeletextPageID{1, 0}); ok {
		t.Fatal("page 100 should have been evicted")
	}
	// Despite the eviction error, the new page's header line was still stored.
	page300, ok := dec.Page(TeletextPageID{3, 0})
	require.True(t, ok)
	assert.Equal(t, []uint8{0}, page300.Rows())

	// A display row targeting the evicted magazine is reported as invalid since
	// no current page survives for magazine 1.
	pkt := ttxDisplayPacket(1, 1, "LOST")
	_, err = dec.FeedPES(ttxPES(ttxPESPayload(0x10,
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, pkt, 0x00))))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTeletextInvalidPage))
}

// ---- Rows outside the display range ----------------------------------------

func TestTeletextDecoderIgnoresServiceRows(t *testing.T) {
	dec := NewTeletextDecoder()
	hdr := ttxHeaderPacket(1, 0, 0, 0, "H")
	// Row 27 (enhancements/service) and row 30 (item marker) carry no text.
	p27 := ttxDisplayPacket(1, 27, "SERVICE")
	p30 := ttxDisplayPacket(1, 30, "ITEM")
	updates, err := dec.FeedPES(ttxPES(ttxPESPayload(0x10,
		ttxDataUnit(TeletextDataUnitIDEBUTeletextSubtitle, hdr, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextNonSubtitle, p27, 0x00),
		ttxDataUnit(TeletextDataUnitIDEBUTeletextNonSubtitle, p30, 0x00))))
	require.NoError(t, err)
	assert.Len(t, updates, 1)
	page, _ := dec.Page(TeletextPageID{1, 0})
	assert.Equal(t, []uint8{0}, page.Rows())
}

// ---- Page id formatting -----------------------------------------------------

func TestTeletextPageIDString(t *testing.T) {
	assert.Equal(t, "899", TeletextPageID{Magazine: 8, Page: 99}.String())
	assert.Equal(t, "100", TeletextPageID{Magazine: 1, Page: 0}.String())
}

// ---- Control codes and character mapping -----------------------------------

func TestTeletextControlClassification(t *testing.T) {
	cases := []struct {
		code byte
		kind TeletextControlKind
	}{
		{0x00, TeletextControlAlphaColor},
		{0x07, TeletextControlAlphaColor},
		{0x08, TeletextControlFlash},
		{0x09, TeletextControlFlash},
		{0x0a, TeletextControlBox},
		{0x0b, TeletextControlBox},
		{0x0c, TeletextControlSize},
		{0x0f, TeletextControlSize},
		{0x10, TeletextControlMosaicColor},
		{0x17, TeletextControlMosaicColor},
		{0x18, TeletextControlMisc},
		{0x1f, TeletextControlMisc},
		{0x7f, TeletextControlMisc},
		{0x80, TeletextControlC1},
		{0x9f, TeletextControlC1},
	}
	for _, tc := range cases {
		kind, ok := teletextControlKind(tc.code)
		require.True(t, ok, "code 0x%02x", tc.code)
		assert.Equal(t, tc.kind, kind)
	}
	for _, code := range []byte{0x20, 0x41, 0x7e, 0xa0, 0xff} {
		_, ok := teletextControlKind(code)
		assert.False(t, ok, "code 0x%02x is display data", code)
	}

	// Color helper.
	red := TeletextControl{Code: 0x01, Kind: TeletextControlAlphaColor}
	color, isColor := red.Color()
	assert.True(t, isColor)
	assert.Equal(t, 1, color)
	mosaic := TeletextControl{Code: 0x15, Kind: TeletextControlMosaicColor}
	color, isColor = mosaic.Color()
	assert.True(t, isColor)
	assert.Equal(t, 5, color)
	_, isColor = TeletextControl{Code: 0x08, Kind: TeletextControlFlash}.Color()
	assert.False(t, isColor)
}

func TestTeletextRuneMapping(t *testing.T) {
	r, ok := decodeTeletextRune('#')
	assert.True(t, ok)
	assert.Equal(t, '£', r)
	r, ok = decodeTeletextRune('A')
	assert.True(t, ok)
	assert.Equal(t, 'A', r)
	r, ok = decodeTeletextRune(0xa1)
	assert.False(t, ok)
	assert.Equal(t, utf8.RuneError, r)
	r, ok = decodeTeletextRune(0x7f)
	assert.False(t, ok)
	assert.Equal(t, utf8.RuneError, r)
}

func TestTeletextLineMiss(t *testing.T) {
	page := &TeletextPage{lines: map[uint8]*TeletextLine{}}
	_, ok := page.Line(7)
	assert.False(t, ok)
	assert.Empty(t, page.Rows())
	assert.Empty(t, page.Text())
}

func TestTeletextBuildLineDoubleHeight(t *testing.T) {
	// Double height (0x0d) changes vertical size only: no horizontal half-cell is
	// blanked, unlike double width/double size (0x0e/0x0f).
	line := buildTeletextLine(1, padTTXText(string([]byte{0x0d, 'A', 'B'})), false, 0, false, 0, 1)
	assert.Equal(t, " AB", line.Text)
	require.Len(t, line.Controls, 1)
	assert.Equal(t, TeletextControlSize, line.Controls[0].Kind)

	line = buildTeletextLine(1, padTTXText(string([]byte{0x0f, 'A', 'B'})), false, 0, false, 0, 1)
	// Double size: col 0 is the control code (one space column), 'A' at col 1
	// with a blank half-cell at col 2; remaining blank columns are trimmed.
	assert.Equal(t, " A", line.Text)
}

func TestBytesEqual(t *testing.T) {
	assert.False(t, bytesEqual([]byte{1, 2}, []byte{1}))
	assert.False(t, bytesEqual([]byte{1, 2}, []byte{1, 3}))
	assert.True(t, bytesEqual([]byte{1, 2}, []byte{1, 2}))
}
