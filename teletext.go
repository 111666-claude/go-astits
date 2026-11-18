package astits

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Teletext references:
//   - ETSI EN 301 775: DVB teletext carried in PES packets (private_stream_1)
//   - ETSI EN 300 706: teletext packet structure, Hamming 8/4 coding, page headers
//   - EBU Tech 3282 / ITU-R BT.653: teletext character sets

const (
	// teletextDataIdentifierMin/Max bracket the data_identifier range allocated to
	// DVB teletext in EN 301 775 (0x10-0x1F, the low nibble is the magazine).
	teletextDataIdentifierMin = 0x10
	teletextDataIdentifierMax = 0x1F

	// teletextDataUnitLength is the fixed data_unit_length of an EBU teletext
	// data unit (1 control byte + 1 framing code + 42 packet bytes).
	teletextDataUnitLength = 44

	// teletextFramingCode is the EBU teletext packet framing code (EN 300 706).
	teletextFramingCode = 0xE4

	// teletextPacketSize is the size of one EBU teletext packet.
	teletextPacketSize = 42

	// teletextLineColumns is the number of character columns in one packet.
	teletextLineColumns = 40

	// teletextMaxDisplayRow is the last row that carries displayable text.
	teletextMaxDisplayRow = 23

	// teletextDefaultMaxPages is the default page cache capacity.
	teletextDefaultMaxPages = 64
)

// Data unit ids defined by EN 301 775.
const (
	TeletextDataUnitIDEBUTeletextNonSubtitle = 0x02
	TeletextDataUnitIDEBUTeletextSubtitle    = 0x03
	TeletextDataUnitIDStuffing               = 0xFF
)

// Teletext error kinds. Use errors.Is to match them against returned errors.
var (
	// ErrTeletextHamming is returned when a Hamming 8/4 byte cannot be corrected.
	ErrTeletextHamming = errors.New("astits: uncorrectable teletext Hamming 8/4 error")
	// ErrTeletextTruncated is returned when a data unit is cut short at a PES or
	// stream boundary.
	ErrTeletextTruncated = errors.New("astits: truncated teletext data unit")
	// ErrTeletextInvalidPage is returned when a page header carries an illegal
	// BCD page number or a display row arrives before any page header.
	ErrTeletextInvalidPage = errors.New("astits: invalid teletext page number")
	// ErrTeletextCacheFull is returned when the page cache is full and the
	// oldest page has to be evicted to make room for a new one.
	ErrTeletextCacheFull = errors.New("astits: teletext page cache full, oldest page evicted")
	// ErrTeletextMalformed is returned for framing violations: bad data
	// identifier, wrong framing code, illegal data unit length or stream id.
	ErrTeletextMalformed = errors.New("astits: malformed teletext data")
)

// TeletextError wraps one of the teletext error sentinels with contextual
// information. Its Unwrap method returns the sentinel so errors.Is keeps working.
type TeletextError struct {
	// Op is the operation during which the error happened.
	Op string
	// Kind is one of the ErrTeletext* sentinels.
	Kind error
	// Detail carries raw values (offending byte, page id, ...).
	Detail string
}

// Error implements the error interface.
func (e *TeletextError) Error() string {
	return e.Kind.Error() + ": " + e.Op + ": " + e.Detail
}

// Unwrap exposes the wrapped sentinel.
func (e *TeletextError) Unwrap() error {
	return e.Kind
}

func newTeletextError(kind error, op, detail string) *TeletextError {
	return &TeletextError{Op: op, Kind: kind, Detail: detail}
}

// hamming84Encode maps a 4-bit nibble to its Hamming 8/4 codeword, as defined
// by EN 300 706 Annex A (table shared by EBU reference encoders such as vbit2).
var hamming84Encode = [16]byte{
	0x15, 0x02, 0x49, 0x5E, 0x64, 0x73, 0x38, 0x2F,
	0xD0, 0xC7, 0x8C, 0x9B, 0xA1, 0xB6, 0xFD, 0xEA,
}

type hamming84Result struct {
	nibble    byte
	corrected bool
	ok        bool
}

// hamming84Decode is built once from the encoder table: each codeword and its
// eight single-bit neighbours decode to the same nibble, everything else is an
// uncorrectable error. A distance-4 code guarantees the sets never overlap.
var hamming84Decode [256]hamming84Result

func init() {
	for n, code := range hamming84Encode {
		put := func(b byte, corrected bool) {
			r := hamming84Result{nibble: byte(n), corrected: corrected, ok: true}
			if existing := hamming84Decode[b]; existing.ok && existing.nibble != byte(n) {
				panic("astits: overlapping Hamming 8/4 codewords")
			}
			hamming84Decode[b] = r
		}
		put(code, false)
		for bit := 0; bit < 8; bit++ {
			put(code^(1<<bit), true)
		}
	}
}

// decodeHamming84 decodes one Hamming 8/4 byte. corrected is true when a
// single-bit error was repaired. ok is false for uncorrectable patterns.
func decodeHamming84(b byte) (nibble byte, corrected bool, ok bool) {
	r := hamming84Decode[b]
	return r.nibble, r.corrected, r.ok
}

// TeletextPageID identifies a teletext page by magazine (1-8) and BCD page
// number (00-99). The full broadcast page number is Magazine*100 + Page.
type TeletextPageID struct {
	Magazine uint8
	Page     uint8
}

// String returns the usual 3-digit page number, e.g. "100".
func (id TeletextPageID) String() string {
	return fmt.Sprintf("%d%02d", id.Magazine, id.Page)
}

// TeletextControlKind classifies the basic control codes of EN 300 706.
type TeletextControlKind uint8

const (
	// TeletextControlAlphaColor is an alpha foreground color (codes 0x00-0x07).
	TeletextControlAlphaColor TeletextControlKind = iota
	// TeletextControlFlash toggles flashing (0x08 flash, 0x09 steady).
	TeletextControlFlash
	// TeletextControlBox starts/ends a boxed area (0x0A end, 0x0B start).
	TeletextControlBox
	// TeletextControlSize changes character size (0x0C normal, 0x0D double
	// height, 0x0E double width, 0x0F double size).
	TeletextControlSize
	// TeletextControlMosaicColor is a mosaic foreground color (0x10-0x17).
	TeletextControlMosaicColor
	// TeletextControlMisc groups concealment, mosaic and background controls
	// (0x18-0x1F).
	TeletextControlMisc
	// TeletextControlC1 is a spacing C1 control code (0x80-0x9F).
	TeletextControlC1
)

// TeletextControl records one basic control code encountered on a line.
type TeletextControl struct {
	// Position is the 0-based column the control code occupies.
	Position int
	// Code is the raw control byte.
	Code byte
	// Kind classifies the code.
	Kind TeletextControlKind
}

// Color returns the 0-7 color index and true for foreground color controls,
// false for every other kind.
func (c TeletextControl) Color() (int, bool) {
	if c.Kind == TeletextControlAlphaColor || c.Kind == TeletextControlMosaicColor {
		return int(c.Code & 0x07), true
	}
	return 0, false
}

// TeletextLine is one decoded row of a teletext page.
type TeletextLine struct {
	// Row is the teletext row number (0 for the page header, 1-23 for display).
	Row uint8
	// Text is the decoded line. Control codes and unsupported characters occupy
	// one column each (space or the unicode replacement character). Trailing
	// spaces are trimmed.
	Text string
	// Raw is a copy of the original 40 packet data bytes, kept so unsupported
	// characters can be reviewed against the stream.
	Raw []byte
	// Controls lists the basic control codes in stream order.
	Controls []TeletextControl
	// Corrected is the number of single-bit Hamming errors repaired while
	// decoding the packet address and, for row 0, the page header.
	Corrected int
	// Unsupported is the number of characters without a G0 mapping (G1 mosaic
	// and national-option characters); their raw bytes remain available in Raw.
	Unsupported int
	// FieldParity is the field parity bit of the data unit.
	FieldParity bool
	// LineOffset is the VBI line offset of the data unit.
	LineOffset uint8
	// PESIndex is the 1-based index of the PES packet in which this line was
	// last updated.
	PESIndex int
}

// TeletextLineUpdate is emitted by TeletextDecoder.FeedPES each time a line
// changes. Repeated identical rows are not emitted.
type TeletextLineUpdate struct {
	PageID TeletextPageID
	Line   TeletextLine
}

// TeletextPage accumulates the rows received for one page.
type TeletextPage struct {
	// ID is the magazine/page identifier.
	ID TeletextPageID
	// Subpage is the 12-bit subpage number from the most recent page header.
	Subpage uint16
	// Control is the 4-bit control nibble (C11-C14) of the most recent header.
	Control uint8

	lines map[uint8]*TeletextLine
	order []uint8
}

// Line returns a copy of the row and true when the page contains it.
func (p *TeletextPage) Line(row uint8) (TeletextLine, bool) {
	if l, ok := p.lines[row]; ok {
		return *l, true
	}
	return TeletextLine{}, false
}

// Rows returns the row numbers currently held by the page, sorted ascending.
func (p *TeletextPage) Rows() []uint8 {
	rows := make([]uint8, 0, len(p.order))
	rows = append(rows, p.order...)
	sort.Slice(rows, func(i, j int) bool { return rows[i] < rows[j] })
	return rows
}

// Text exports the page as text: held rows in ascending order, joined by new
// lines, each trimmed of trailing spaces.
func (p *TeletextPage) Text() string {
	var b strings.Builder
	for idx, row := range p.order {
		if idx > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.lines[row].Text)
	}
	return b.String()
}

// TeletextDecoder accumulates teletext pages out of PES payloads. It is not
// safe for concurrent use, matching the single-goroutine model of the demuxer.
type TeletextDecoder struct {
	maxPages int

	// pages/pageOrder implement the bounded page cache (FIFO eviction).
	pages     map[TeletextPageID]*TeletextPage
	pageOrder []TeletextPageID

	// A display packet only carries a magazine, so it belongs to the page
	// header most recently seen on that magazine (EN 300 706 section 7.1).
	current    [8]TeletextPageID
	hasCurrent [8]bool

	// carry holds a partial data unit waiting for the next PES packet.
	carry []byte

	pesIndex int
	errs     []error
}

// TeletextDecoderOptMaxPages limits the number of cached pages. Values below 1
// fall back to the default.
func TeletextDecoderOptMaxPages(n int) func(*TeletextDecoder) {
	return func(d *TeletextDecoder) {
		if n >= 1 {
			d.maxPages = n
		}
	}
}

// NewTeletextDecoder creates a teletext page decoder.
func NewTeletextDecoder(opts ...func(*TeletextDecoder)) *TeletextDecoder {
	d := &TeletextDecoder{
		maxPages: teletextDefaultMaxPages,
		pages:    make(map[TeletextPageID]*TeletextPage),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Errors returns every teletext error observed since decoder creation, in
// arrival order. FeedPES also returns the errors of the current PES directly.
func (d *TeletextDecoder) Errors() []error {
	out := make([]error, len(d.errs))
	copy(out, d.errs)
	return out
}

// PageIDs returns the cached page identifiers in insertion order.
func (d *TeletextDecoder) PageIDs() []TeletextPageID {
	out := make([]TeletextPageID, len(d.pageOrder))
	copy(out, d.pageOrder)
	return out
}

// Page returns the accumulated page for id when it is still cached.
func (d *TeletextDecoder) Page(id TeletextPageID) (*TeletextPage, bool) {
	p, ok := d.pages[id]
	return p, ok
}

// FeedPES parses one teletext PES packet (stream id 0xBD, private_stream_1).
// Data units may span several PES packets: incomplete units are buffered and
// completed on the next call. It returns one update per changed line, in
// arrival order, and every non-fatal teletext error encountered in the PES
// (joined with errors.Join); decoding always continues past bad units.
func (d *TeletextDecoder) FeedPES(pes *PESData) (updates []TeletextLineUpdate, err error) {
	if pes == nil {
		return nil, fmt.Errorf("astits: teletext decoder expects a non-nil PES")
	}
	if pes.Header == nil {
		return nil, fmt.Errorf("astits: teletext decoder expects a PES header")
	}

	d.pesIndex++
	var errs []error
	addErr := func(kind error, op, detail string) {
		errs = append(errs, newTeletextError(kind, op, detail))
	}

	if pes.Header.StreamID != StreamIDPrivateStream1 {
		addErr(ErrTeletextMalformed, "FeedPES",
			fmt.Sprintf("stream id 0x%02x is not private_stream_1 (0x%02x)", pes.Header.StreamID, StreamIDPrivateStream1))
	}

	// Every PES starts with its own data_identifier (EN 301 775), including PES
	// packets that continue a data unit split across PES boundaries.
	payload := pes.Data
	if len(payload) == 0 {
		addErr(ErrTeletextTruncated, "data_identifier", "PES payload is empty")
		d.errs = append(d.errs, errs...)
		return nil, errors.Join(errs...)
	}
	if id := payload[0]; id < teletextDataIdentifierMin || id > teletextDataIdentifierMax {
		addErr(ErrTeletextMalformed, "data_identifier", fmt.Sprintf("0x%02x outside 0x10-0x1f", id))
		d.errs = append(d.errs, errs...)
		return nil, errors.Join(errs...)
	}
	payload = payload[1:]

	// Splice any partial data unit carried from the previous PES in front.
	buf := make([]byte, 0, len(d.carry)+len(payload))
	buf = append(buf, d.carry...)
	buf = append(buf, payload...)
	d.carry = d.carry[:0]

	off := 0

	for len(buf)-off >= 2 {
		unitID := buf[off]
		unitLen := int(buf[off+1])
		headerEnd := off + 2

		// Stuffing terminates the PES: every remaining byte is padding.
		if unitID == TeletextDataUnitIDStuffing {
			off = len(buf)
			break
		}

		// Wait for the full unit data, possibly in a later PES. A data unit is
		// at most 2 header bytes + 255 data bytes, so only one can be in flight.
		if len(buf)-headerEnd < unitLen {
			d.carry = append(d.carry, buf[off:]...)
			off = len(buf)
			break
		}

		data := buf[headerEnd : headerEnd+unitLen]
		off = headerEnd + unitLen

		switch unitID {
		case TeletextDataUnitIDEBUTeletextNonSubtitle, TeletextDataUnitIDEBUTeletextSubtitle:
			if unitLen != teletextDataUnitLength {
				addErr(ErrTeletextMalformed, "data_unit",
					fmt.Sprintf("teletext data unit length is %d, expected %d", unitLen, teletextDataUnitLength))
				continue
			}
			lineUpdates, perr := d.parseTeletextDataUnit(data)
			updates = append(updates, lineUpdates...)
			if perr != nil {
				errs = append(errs, perr)
				continue
			}
		default:
			// Reserved ids and other VBI services (VPS, WSS, ...) share the PES
			// but are not teletext pages: skip their bytes silently.
		}
	}

	// A single leftover byte (a split data unit id) is carried as well.
	if off < len(buf) {
		d.carry = append(d.carry, buf[off:]...)
	}

	d.errs = append(d.errs, errs...)
	return updates, errors.Join(errs...)
}

// Flush marks the end of the stream. A data unit still pending at that point
// can never be completed and is reported as truncated and discarded.
func (d *TeletextDecoder) Flush() error {
	if len(d.carry) == 0 {
		return nil
	}
	err := newTeletextError(ErrTeletextTruncated, "Flush",
		fmt.Sprintf("%d bytes of an incomplete data unit remain at end of stream", len(d.carry)))
	d.carry = d.carry[:0]
	d.errs = append(d.errs, err)
	return err
}

// parseTeletextDataUnit decodes one 44-byte EBU teletext data unit.
func (d *TeletextDecoder) parseTeletextDataUnit(data []byte) ([]TeletextLineUpdate, error) {
	// Byte 0: reserved bits + field parity (bit 5) + line offset (bits 0-4).
	fieldParity := data[0]&0x20 != 0
	lineOffset := data[0] & 0x1f

	// Byte 1: framing code.
	if data[1] != teletextFramingCode {
		return nil, newTeletextError(ErrTeletextMalformed, "framing_code",
			fmt.Sprintf("0x%02x, expected 0x%02x", data[1], teletextFramingCode))
	}

	// Bytes 2-43: the 42-byte teletext packet.
	packet := data[2:]
	magazine, row, corrected, err := decodeTeletextAddress(packet[0], packet[1])
	if err != nil {
		return nil, err
	}

	if row == 0 {
		return d.parsePageHeader(magazine, packet, corrected, fieldParity, lineOffset)
	}
	if row > teletextMaxDisplayRow {
		// Packets 24-31 carry parallel transmission, enhancement and service data,
		// not displayable rows.
		return nil, nil
	}
	return d.parseDisplayRow(magazine, row, packet, corrected, fieldParity, lineOffset)
}

// decodeTeletextAddress decodes the two Hamming 8/4 address bytes into magazine
// (1-8, transmitted magazine 0 means 8) and row (0-31).
func decodeTeletextAddress(b1, b2 byte) (magazine, row uint8, corrected int, err error) {
	n1, c1, ok1 := decodeHamming84(b1)
	n2, c2, ok2 := decodeHamming84(b2)
	if !ok1 || !ok2 {
		return 0, 0, 0, newTeletextError(ErrTeletextHamming, "address",
			fmt.Sprintf("address bytes 0x%02x 0x%02x", b1, b2))
	}
	if c1 {
		corrected++
	}
	if c2 {
		corrected++
	}
	magazine = n1 & 0x07
	if magazine == 0 {
		magazine = 8
	}
	row = (n1&0x08)<<1 | n2
	return magazine, row, corrected, nil
}

// parsePageHeader handles row 0: it announces the current page of a magazine
// and refreshes its header line.
func (d *TeletextDecoder) parsePageHeader(magazine uint8, packet []byte, corrected int, fieldParity bool, lineOffset uint8) ([]TeletextLineUpdate, error) {
	// packet[2]/packet[3]: BCD page units and tens.
	units, c1, ok1 := decodeHamming84(packet[2])
	tens, c2, ok2 := decodeHamming84(packet[3])
	if !ok1 || !ok2 {
		return nil, newTeletextError(ErrTeletextHamming, "page_number",
			fmt.Sprintf("page bytes 0x%02x 0x%02x", packet[2], packet[3]))
	}
	if c1 {
		corrected++
	}
	if c2 {
		corrected++
	}
	if units > 9 || tens > 9 {
		return nil, newTeletextError(ErrTeletextInvalidPage, "page_number",
			fmt.Sprintf("magazine %d carries non-BCD page nibbles %x %x", magazine, tens, units))
	}
	id := TeletextPageID{Magazine: magazine, Page: tens*10 + units}

	// packet[4]: control nibble, packet[5..7]: subpage number S1-S3.
	nibbles := make([]byte, 4)
	for idx, pos := range []int{4, 5, 6, 7} {
		n, correctedBit, ok := decodeHamming84(packet[pos])
		if !ok {
			return nil, newTeletextError(ErrTeletextHamming, "page_header",
				fmt.Sprintf("header byte at position %d is 0x%02x", pos, packet[pos]))
		}
		if correctedBit {
			corrected++
		}
		nibbles[idx] = n
	}

	page, cacheErr := d.obtainPage(id)
	page.Control = nibbles[0]
	page.Subpage = uint16(nibbles[1])<<8 | uint16(nibbles[2])<<4 | uint16(nibbles[3])
	d.current[magazine-1] = id
	d.hasCurrent[magazine-1] = true

	// Row 0 occupies 40 columns; the six control/Hamming bytes render as spaces,
	// display text starts at column 6.
	line := buildTeletextLine(0, packet[2:], true, corrected, fieldParity, lineOffset, d.pesIndex)
	// A cache-eviction error does not invalidate the header itself: the new page
	// still receives its row 0 update.
	return d.storeLine(page, line), cacheErr
}

// parseDisplayRow handles rows 1-23, attributed to the page last announced on
// the row's magazine.
func (d *TeletextDecoder) parseDisplayRow(magazine, row uint8, packet []byte, corrected int, fieldParity bool, lineOffset uint8) ([]TeletextLineUpdate, error) {
	if !d.hasCurrent[magazine-1] {
		return nil, newTeletextError(ErrTeletextInvalidPage, "display_row",
			fmt.Sprintf("row %d of magazine %d arrives before any page header", row, magazine))
	}
	id := d.current[magazine-1]
	page, ok := d.pages[id]
	if !ok {
		return nil, newTeletextError(ErrTeletextInvalidPage, "display_row",
			fmt.Sprintf("current page %s of magazine %d is no longer cached", id, magazine))
	}
	line := buildTeletextLine(row, packet[2:], false, corrected, fieldParity, lineOffset, d.pesIndex)
	return d.storeLine(page, line), nil
}

// obtainPage returns the cached page or creates it, evicting the oldest page
// when the cache is full.
func (d *TeletextDecoder) obtainPage(id TeletextPageID) (*TeletextPage, error) {
	if page, ok := d.pages[id]; ok {
		return page, nil
	}
	var err error
	if len(d.pageOrder) >= d.maxPages {
		evicted := d.pageOrder[0]
		delete(d.pages, evicted)
		d.pageOrder = d.pageOrder[1:]
		for mag := 0; mag < len(d.current); mag++ {
			if d.hasCurrent[mag] && d.current[mag] == evicted {
				d.hasCurrent[mag] = false
			}
		}
		err = newTeletextError(ErrTeletextCacheFull, "obtainPage",
			fmt.Sprintf("page %s evicted to cache %s", evicted, id))
	}
	page := &TeletextPage{ID: id, lines: make(map[uint8]*TeletextLine)}
	d.pages[id] = page
	d.pageOrder = append(d.pageOrder, id)
	return page, err
}

// storeLine replaces a row only when its raw bytes change and returns the
// update list (empty for repeated identical rows).
func (d *TeletextDecoder) storeLine(page *TeletextPage, line *TeletextLine) []TeletextLineUpdate {
	if existing, ok := page.lines[line.Row]; ok && bytesEqual(existing.Raw, line.Raw) {
		return nil
	}
	if _, ok := page.lines[line.Row]; !ok {
		page.order = append(page.order, line.Row)
		sort.Slice(page.order, func(i, j int) bool { return page.order[i] < page.order[j] })
	}
	page.lines[line.Row] = line
	return []TeletextLineUpdate{{PageID: page.ID, Line: *line}}
}

// buildTeletextLine turns the 40 packet data bytes into a TeletextLine. When
// header is true the first six columns are page control bytes and render as
// spaces.
func buildTeletextLine(row uint8, data []byte, header bool, corrected int, fieldParity bool, lineOffset uint8, pesIndex int) *TeletextLine {
	raw := make([]byte, teletextLineColumns)
	copy(raw, data)

	text := make([]rune, teletextLineColumns)
	controls := make([]TeletextControl, 0)
	unsupported := 0
	doubleWidth := false
	blankNext := false

	for col := 0; col < teletextLineColumns; col++ {
		c := data[col]

		// Row 0 columns 0-5 are the Hamming-coded page and subpage bytes: they
		// occupy display columns but must never be interpreted as control codes.
		if header && col < 6 {
			text[col] = ' '
			continue
		}

		if kind, ok := teletextControlKind(c); ok {
			controls = append(controls, TeletextControl{Position: col, Code: c, Kind: kind})
			text[col] = ' '
			// A size control changes width mode and cancels any pending blank
			// half-cell left by the previous double-width character.
			if kind == TeletextControlSize {
				doubleWidth = c == 0x0e || c == 0x0f
				blankNext = false
			}
			continue
		}

		// The column following a double-width character is its blank half-cell.
		// blankNext is one-shot and is consumed here; doubleWidth persists.
		if blankNext {
			text[col] = ' '
			blankNext = false
			continue
		}

		r, supported := decodeTeletextRune(c)
		if !supported {
			unsupported++
		}
		text[col] = r
		if doubleWidth && col+1 < teletextLineColumns {
			blankNext = true
		}
	}

	return &TeletextLine{
		Row:         row,
		Text:        strings.TrimRight(string(text), " "),
		Raw:         raw,
		Controls:    controls,
		Corrected:   corrected,
		Unsupported: unsupported,
		FieldParity: fieldParity,
		LineOffset:  lineOffset,
		PESIndex:    pesIndex,
	}
}

// teletextControlKind classifies a spacing control code.
func teletextControlKind(c byte) (TeletextControlKind, bool) {
	switch {
	case c <= 0x07:
		return TeletextControlAlphaColor, true
	case c >= 0x08 && c <= 0x09:
		return TeletextControlFlash, true
	case c >= 0x0a && c <= 0x0b:
		return TeletextControlBox, true
	case c >= 0x0c && c <= 0x0f:
		return TeletextControlSize, true
	case c >= 0x10 && c <= 0x17:
		return TeletextControlMosaicColor, true
	case c >= 0x18 && c <= 0x1f || c == 0x7f:
		return TeletextControlMisc, true
	case c >= 0x80 && c <= 0x9f:
		return TeletextControlC1, true
	default:
		return 0, false
	}
}

// decodeTeletextRune maps one display byte to a rune. The default English G0
// national option subset is used (printable ASCII plus the pound sign). G1
// mosaics, other national options and hard space have no G0 mapping: they
// return the unicode replacement character with supported=false and their raw
// byte stays available in TeletextLine.Raw for later review.
func decodeTeletextRune(c byte) (rune, bool) {
	if c == 0x23 {
		return '£', true
	}
	if c >= 0x20 && c < 0x7f {
		return rune(c), true
	}
	return utf8.RuneError, false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for idx := range a {
		if a[idx] != b[idx] {
			return false
		}
	}
	return true
}
