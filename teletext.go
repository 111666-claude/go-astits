package astits

import (
	"bytes"
	"errors"
	"fmt"
)

// DVB teletext / EBU teletext constants
// Reference: ETSI EN 300 472 (DVB teletext transport) and ETSI EN 300 706 (EBU teletext system)
const (
	// TeletextDataIdentifierFirst is the first valid value of the data_identifier byte
	// of a DVB teletext PES packet (EN 300 472, section 4.1).
	TeletextDataIdentifierFirst = 0x10
	// TeletextDataIdentifierLast is the last valid value of the data_identifier byte.
	// A value between 0x10 and 0x1f (included) means the PES packet carries EBU teletext data.
	TeletextDataIdentifierLast = 0x1f

	// TeletextDataUnitIDNonSubtitle is the data_unit_id of a non-subtitle EBU teletext data unit
	TeletextDataUnitIDNonSubtitle = 0x02
	// TeletextDataUnitIDSubtitle is the data_unit_id of an EBU teletext subtitle data unit
	TeletextDataUnitIDSubtitle = 0x03
	// TeletextDataUnitIDInvertedSequence is the data_unit_id of an EBU teletext data unit
	// whose data is transmitted in the inverted sequence.
	TeletextDataUnitIDInvertedSequence = 0x04
	// TeletextDataUnitIDFiller terminates a PES packet: every following byte is stuffing (0xff)
	TeletextDataUnitIDFiller = 0xff

	// teletextDataUnitLength is the fixed data_unit_length of an EBU teletext data unit (EN 300 472, section 4.2)
	teletextDataUnitLength = 0x2c // 44 bytes
	// teletextDataUnitHeaderLength is data_unit_id (1 byte) + data_unit_length (1 byte)
	teletextDataUnitHeaderLength = 2
	// teletextAddressByteOffset is the offset of the first Hamming-8/4 address byte inside a data unit.
	// Layout: data_unit_id, data_unit_length, 2 clock run-in bytes, framing code, address byte 1, ...
	teletextAddressByteOffset = 5
	// teletextRow0 is the row number of the page header packet
	teletextRow0 = 0
)

// Teletext extraction errors. They are always wrapped with contextual information
// and reported through TeletextEvent.Err, use errors.Is to match them.
var (
	// ErrTeletextNoDescriptor is returned when the selected elementary stream has no
	// teletext descriptor (tag 0x56) in its PMT entry.
	ErrTeletextNoDescriptor = errors.New("astits: no teletext descriptor on the selected elementary stream")
	// ErrTeletextNoMatch is returned when no teletext track matches the requested selector.
	ErrTeletextNoMatch = errors.New("astits: no teletext track matches the selector")
	// ErrTeletextMissingPTS is returned when a teletext PES packet carries no PTS.
	ErrTeletextMissingPTS = errors.New("astits: teletext PES packet has no PTS")
	// ErrTeletextBadDataUnitLength is returned when a teletext data unit does not have
	// the fixed 44-byte length mandated by EN 300 472.
	ErrTeletextBadDataUnitLength = errors.New("astits: invalid teletext data unit length")
	// ErrTeletextBadDataIdentifier is returned when a teletext PID PES packet starts
	// with a data_identifier outside the 0x10-0x1f range.
	ErrTeletextBadDataIdentifier = errors.New("astits: invalid teletext data identifier")
	// ErrTeletextTruncated is returned when a teletext data unit is cut short by the PES end.
	ErrTeletextTruncated = errors.New("astits: truncated teletext data unit")
	// ErrTeletextContinuity is returned when a continuity counter gap or a discontinuity
	// indicator is observed on the selected teletext PID.
	ErrTeletextContinuity = errors.New("astits: teletext PID continuity error")
	// ErrTeletextProgramChange is returned when the PAT/PMT of the selected program
	// changes while the extractor is running.
	ErrTeletextProgramChange = errors.New("astits: selected teletext program changed")
)

// hamming84Encode maps a 4-bit nibble to its Hamming-8/4 protected byte (EN 300 706, Annex A).
// Bit layout on the wire is D1..D5 (parity), D6..D8 (data).
var hamming84Encode = [16]byte{
	0x15, 0x02, 0x49, 0x5e, // 0..3
	0x64, 0x73, 0x38, 0x2f, // 4..7
	0xd0, 0xc7, 0x8c, 0x9b, // 8..11
	0xa1, 0xb6, 0xfd, 0xea, // 12..15
}

// hamming84Decode decodes a Hamming-8/4 protected byte into its 4-bit nibble.
// corrected is true when one bit had to be flipped to recover the nibble.
// When more than one bit is wrong the error is not correctable: the nearest
// codeword is returned and corrected is false.
func hamming84Decode(b byte) (nibble uint8, corrected bool) {
	for n, code := range hamming84Encode {
		if code == b {
			return uint8(n), false
		}
	}
	// Single-bit error correction: look for the codeword at Hamming distance 1
	for n, code := range hamming84Encode {
		if hammingDistance(byte(code), b) == 1 {
			return uint8(n), true
		}
	}
	// Uncorrectable error: return the nearest codeword so framing stays usable,
	// but do not claim any correction happened.
	bestNibble, bestDistance := uint8(0), 9
	for n, code := range hamming84Encode {
		if d := hammingDistance(byte(code), b); d < bestDistance {
			bestDistance = d
			bestNibble = uint8(n)
		}
	}
	return bestNibble, false
}

// hammingDistance returns the number of differing bits between two bytes.
func hammingDistance(a, b byte) int {
	v := a ^ b
	n := 0
	for v != 0 {
		n += int(v & 1)
		v >>= 1
	}
	return n
}

// TeletextSelector selects a teletext track from the PMT teletext descriptors.
// Zero values act as wildcards: e.g. an empty Language selects every teletext
// entry of the program, while Magazine == 0 and Page == 0 match every page.
type TeletextSelector struct {
	// ProgramNumber is the PAT/PMT program number. 0 matches the first program found.
	ProgramNumber uint16
	// Language is the ISO 639-2/B language code of the teletext descriptor entry
	// (3 bytes, e.g. "eng"). Empty matches any language.
	Language []byte
	// Magazine is the teletext magazine number (1-8). 0 matches any magazine.
	Magazine uint8
	// Page is the teletext page number (0-99 in the descriptor encoding). 0 matches any page.
	Page uint8
}

// TeletextTrack describes one teletext entry found in a PMT teletext descriptor.
type TeletextTrack struct {
	// PID is the elementary stream PID carrying the teletext PES packets.
	PID uint16
	// ProgramNumber is the program the elementary stream belongs to.
	ProgramNumber uint16
	// Language is the ISO 639-2/B language code from the descriptor entry.
	Language []byte
	// Magazine is the descriptor magazine number (1-8).
	Magazine uint8
	// Page is the descriptor page number (0-99).
	Page uint8
	// Type is the teletext type from the descriptor (see TeletextType* constants).
	Type uint8
}

// TeletextDataUnit is a raw DVB teletext data unit with the PTS of the PES packet
// that carried it. Data bytes are preserved exactly as found on the wire, no
// teletext rendering or character decoding is performed.
type TeletextDataUnit struct {
	// PID is the PID of the PES packet carrying the data unit.
	PID uint16
	// ProgramNumber is the program the data unit belongs to.
	ProgramNumber uint16
	// Language is the language of the selected teletext track.
	Language []byte
	// DataUnitID is the data_unit_id byte.
	DataUnitID uint8
	// PTS is the 90 kHz Presentation Timestamp of the carrying PES packet.
	// Nil when the PES packet had no PTS, in which case PTSPresent is false.
	PTS *ClockReference
	// PTSPresent is false when the carrying PES packet had no PTS.
	PTSPresent bool
	// Data is the complete raw data unit, including its data_unit_id and
	// data_unit_length bytes, in transmission order.
	Data []byte
	// Magazine is the teletext magazine decoded from the Hamming-8/4 address bytes.
	Magazine uint8
	// Row is the teletext row decoded from the Hamming-8/4 address bytes.
	Row uint8
	// Page is the teletext page number (0-99) decoded from a row 0 page header
	// data unit seen previously on the same magazine. 0 when not yet known.
	Page uint8
	// Corrected is true when a single-bit Hamming error was corrected while
	// decoding the address bytes.
	Corrected bool
}

// TeletextEventKind enumerates the kinds of events emitted by the extractor.
type TeletextEventKind uint8

const (
	// TeletextEventKindDataUnit is emitted for every subtitle data unit matching the selector.
	TeletextEventKindDataUnit TeletextEventKind = iota
	// TeletextEventKindError is emitted for every recoverable or fatal error condition.
	TeletextEventKindError
)

// TeletextEvent is either an extracted data unit or an error condition.
type TeletextEvent struct {
	// Kind is the event kind.
	Kind TeletextEventKind
	// DataUnit is set when Kind == TeletextEventKindDataUnit.
	DataUnit *TeletextDataUnit
	// Err is set when Kind == TeletextEventKindError, never nil in that case.
	Err error
	// PID is the PID the event relates to (0 when not applicable).
	PID uint16
}

// TeletextExtractor extracts raw teletext data units from a demuxed transport stream.
// It is not thread-safe: feed it the output of Demuxer.NextData() sequentially.
type TeletextExtractor struct {
	selector TeletextSelector

	// Selection state, filled from the first matching PMT
	selected      *TeletextTrack // descriptor entry the selector resolved to
	pid           uint16         // selected teletext PID, 0 until a PMT is seen
	programNumber uint16
	initialized   bool
	noDescriptor  bool // PMT for the program was seen but the PID had no teletext descriptor

	// PAT/PMT change detection
	lastPAT *PATData

	// Current teletext page per magazine, decoded from row 0 header data units
	activePage map[uint8]uint8

	// Continuity tracking per selected PID
	lastCC        uint8
	lastCCValid   bool
	continuityPID uint16

	// pending holds events queued between HandleData calls, notably continuity
	// errors produced by the packet skipper while Demuxer.NextData() reads packets.
	pending []*TeletextEvent
	// emitted holds the events returned by the latest HandleData call.
	emitted []*TeletextEvent
}

// NewTeletextExtractor creates a teletext extractor for the given selector.
func NewTeletextExtractor(selector TeletextSelector) *TeletextExtractor {
	return &TeletextExtractor{
		selector:   selector,
		activePage: make(map[uint8]uint8),
	}
}

// FindTeletextTracks returns every teletext track advertised by a PMT through
// teletext descriptors (tag 0x56). VBI teletext descriptors (tag 0x46) are not
// returned: they describe VBI-carried teletext rather than PES-carried DVB teletext.
func FindTeletextTracks(pmt *PMTData) []*TeletextTrack {
	if pmt == nil {
		return nil
	}
	var tracks []*TeletextTrack
	for _, es := range pmt.ElementaryStreams {
		for _, d := range es.ElementaryStreamDescriptors {
			if d.Tag != DescriptorTagTeletext || d.Teletext == nil {
				continue
			}
			for _, item := range d.Teletext.Items {
				tracks = append(tracks, &TeletextTrack{
					PID:           es.ElementaryPID,
					ProgramNumber: pmt.ProgramNumber,
					Language:      append([]byte(nil), item.Language...),
					Magazine:      item.Magazine,
					Page:          item.Page,
					Type:          item.Type,
				})
			}
		}
	}
	return tracks
}

// SelectedTrack returns the descriptor entry the selector resolved to, or nil
// before the first matching PMT has been processed.
func (e *TeletextExtractor) SelectedTrack() *TeletextTrack {
	return e.selected
}

// PacketSkipper returns a PacketSkipper compatible with DemuxerOptPacketSkipper.
// It never skips packets: it only inspects continuity counters of the selected
// teletext PID and queues TeletextEventKindError events on discontinuities so
// that data loss caused by TS layer gaps is never silent.
func (e *TeletextExtractor) PacketSkipper() PacketSkipper {
	return func(p *Packet) bool {
		// Only watch the selected teletext PID once it is known
		if e.pid == 0 || p.Header.PID != e.pid {
			return false
		}

		// Explicit discontinuity indicator on the adaptation field
		if p.Header.HasAdaptationField && p.AdaptationField != nil && p.AdaptationField.DiscontinuityIndicator {
			e.queuePendingError(e.pid, fmt.Errorf("%w: adaptation field discontinuity indicator set", ErrTeletextContinuity))
			e.lastCC = p.Header.ContinuityCounter
			e.lastCCValid = true
			e.continuityPID = p.Header.PID
			return false
		}

		// Continuity counter check, packets without payload may repeat the counter
		if e.lastCCValid && e.continuityPID == p.Header.PID && p.Header.HasPayload {
			if expected := (e.lastCC + 1) & 0x0f; expected != p.Header.ContinuityCounter {
				e.queuePendingError(e.pid, fmt.Errorf("%w: expected continuity counter %d, got %d",
					ErrTeletextContinuity, expected, p.Header.ContinuityCounter))
			}
		}
		if p.Header.HasPayload {
			e.lastCC = p.Header.ContinuityCounter
			e.lastCCValid = true
			e.continuityPID = p.Header.PID
		}
		return false
	}
}

// HandleData feeds one DemuxerData to the extractor and returns the events it
// produced. Events queued asynchronously by the packet skipper (continuity
// errors) are drained as well, so every call returns the full event backlog.
// The returned slice is valid until the next call and must not be retained
// without copying the data units it references.
func (e *TeletextExtractor) HandleData(d *DemuxerData) []*TeletextEvent {
	// Start from the events queued by the packet skipper while the demuxer was
	// reading the packets backing this data. They must not be reset before this.
	e.emitted = append(e.emitted[:0], e.pending...)
	e.pending = e.pending[:0]
	if d == nil {
		return e.emitted
	}

	switch {
	case d.PAT != nil:
		e.handlePAT(d.PAT)
	case d.PMT != nil:
		e.handlePMT(d.PMT)
	case d.PES != nil:
		e.handlePES(d)
	}
	return e.emitted
}

// emitError appends an error event to the events returned by the current
// HandleData call.
func (e *TeletextExtractor) emitError(pid uint16, err error) {
	e.emitted = append(e.emitted, &TeletextEvent{
		Kind: TeletextEventKindError,
		Err:  err,
		PID:  pid,
	})
}

// DrainPending returns events queued since the last HandleData call without
// feeding new data. Call it once the demuxer reaches ErrNoMorePackets so that
// continuity errors detected on trailing packets are not lost.
func (e *TeletextExtractor) DrainPending() []*TeletextEvent {
	out := e.pending
	e.pending = nil
	return out
}

// handlePAT tracks PAT updates to detect program changes.
func (e *TeletextExtractor) handlePAT(pat *PATData) {
	if e.lastPAT != nil && !samePAT(e.lastPAT, pat) {
		// A PAT change may remap the selected program; report it.
		e.emitError(0, fmt.Errorf("%w: PAT now advertises %d program(s)",
			ErrTeletextProgramChange, len(pat.Programs)))
	}
	e.lastPAT = pat
}

// samePAT compares the program entries of two PATs.
func samePAT(a, b *PATData) bool {
	if len(a.Programs) != len(b.Programs) {
		return false
	}
	for i := range a.Programs {
		if a.Programs[i].ProgramNumber != b.Programs[i].ProgramNumber ||
			a.Programs[i].ProgramMapID != b.Programs[i].ProgramMapID {
			return false
		}
	}
	return true
}

// handlePMT resolves the teletext PID/track from PMT teletext descriptors and
// detects PMT changes for the selected program.
func (e *TeletextExtractor) handlePMT(pmt *PMTData) {
	// Restrict to the requested program once the selector pins one down
	if e.selector.ProgramNumber != 0 && pmt.ProgramNumber != e.selector.ProgramNumber {
		return
	}

	tracks := FindTeletextTracks(pmt)

	// Resolve the program number when the selector left it open
	programNumber := e.selector.ProgramNumber
	if programNumber == 0 {
		programNumber = pmt.ProgramNumber
	}

	// No teletext descriptor at all on this program's elementary streams
	if len(tracks) == 0 {
		if e.initialized && e.programNumber == programNumber {
			e.emitError(0, fmt.Errorf("%w: PMT of program %d lost its teletext descriptor",
				ErrTeletextProgramChange, pmt.ProgramNumber))
			e.pid = 0
			e.initialized = false
			e.selected = nil
		} else if !e.noDescriptor {
			e.noDescriptor = true
			e.emitError(0, fmt.Errorf("%w: program %d", ErrTeletextNoDescriptor, pmt.ProgramNumber))
		}
		return
	}
	e.noDescriptor = false

	// Pick the track matching the language/magazine/page selector
	match := e.selectTrack(tracks)
	if match == nil {
		if !e.initialized {
			e.emitError(0, fmt.Errorf("%w: program %d has %d teletext track(s) but none matches the selector",
				ErrTeletextNoMatch, pmt.ProgramNumber, len(tracks)))
		}
		return
	}

	// After initialization, a new PMT remapping the PID or losing the selected
	// entry is a program change the caller must know about.
	if e.initialized && programNumber == e.programNumber && match.PID != e.pid {
		e.emitError(match.PID, fmt.Errorf("%w: teletext PID switched from %d to %d",
			ErrTeletextProgramChange, e.pid, match.PID))
	}

	e.selected = match
	e.pid = match.PID
	e.programNumber = programNumber
	e.initialized = true
}

// selectTrack applies the language/magazine/page filters of the selector.
func (e *TeletextExtractor) selectTrack(tracks []*TeletextTrack) *TeletextTrack {
	for _, t := range tracks {
		if e.selector.ProgramNumber != 0 && t.ProgramNumber != e.selector.ProgramNumber {
			continue
		}
		if len(e.selector.Language) > 0 && !bytes.Equal(t.Language, e.selector.Language) {
			continue
		}
		if e.selector.Magazine != 0 && t.Magazine != e.selector.Magazine {
			continue
		}
		if e.selector.Page != 0 && t.Page != e.selector.Page {
			continue
		}
		return t
	}
	return nil
}

// handlePES parses the teletext data units of one PES packet.
func (e *TeletextExtractor) handlePES(d *DemuxerData) {
	// Ignore PES packets of other programs/PIDs entirely: multi-program streams
	// must never bleed into each other's output.
	if !e.initialized || d.PID != e.pid {
		return
	}

	pes := d.PES
	data := pes.Data
	if len(data) == 0 {
		return
	}

	// data_identifier
	if data[0] < TeletextDataIdentifierFirst || data[0] > TeletextDataIdentifierLast {
		e.emitError(d.PID, fmt.Errorf("%w: 0x%02x", ErrTeletextBadDataIdentifier, data[0]))
		return
	}

	// PTS handling: teletext PES packets must carry a PTS (EN 300 472, section 4.1)
	var pts *ClockReference
	ptsPresent := false
	if pes.Header != nil && pes.Header.OptionalHeader != nil && pes.Header.OptionalHeader.PTS != nil {
		pts = pes.Header.OptionalHeader.PTS
		ptsPresent = true
	} else {
		e.emitError(d.PID, fmt.Errorf("%w: PID %d", ErrTeletextMissingPTS, d.PID))
	}

	// Parse the data unit loop
	offset := 1 // data_identifier
	for offset < len(data) {
		// data_unit_id
		unitID := data[offset]

		// Filler terminates the PES packet, the rest is 0xff stuffing
		if unitID == TeletextDataUnitIDFiller {
			break
		}

		// data_unit_length
		if offset+1 >= len(data) {
			e.emitError(d.PID, fmt.Errorf("%w: missing data_unit_length at offset %d", ErrTeletextTruncated, offset))
			return
		}
		unitLength := int(data[offset+1])
		if unitLength != teletextDataUnitLength {
			e.emitError(d.PID, fmt.Errorf("%w: data_unit_id 0x%02x declares %d bytes, expected %d",
				ErrTeletextBadDataUnitLength, unitID, unitLength, teletextDataUnitLength))
			// We cannot trust the framing anymore: skip the header and stop parsing the packet
			return
		}

		start := offset
		end := offset + teletextDataUnitHeaderLength + unitLength
		if end > len(data) {
			e.emitError(d.PID, fmt.Errorf("%w: data unit of %d bytes at offset %d overflows PES payload of %d bytes",
				ErrTeletextTruncated, teletextDataUnitLength, offset, len(data)))
			return
		}

		unit := append([]byte(nil), data[start:end]...)
		e.processDataUnit(d.PID, unitID, unit, pts, ptsPresent)
		offset = end
	}
}

// processDataUnit decodes the address bytes of a data unit and emits it when it
// belongs to the page selected through the descriptor.
func (e *TeletextExtractor) processDataUnit(pid uint16, unitID uint8, unit []byte, pts *ClockReference, ptsPresent bool) {
	// Decode the two Hamming-8/4 address bytes (EN 300 706)
	addr1, corrected1 := hamming84Decode(unit[teletextAddressByteOffset])
	addr2, corrected2 := hamming84Decode(unit[teletextAddressByteOffset+1])
	corrected := corrected1 || corrected2

	magazine := addr1 & 0x07
	row := ((addr1 >> 3) & 0x01) | (addr2 << 1)
	if magazine == 0 {
		// Magazine 0 is transmitted as magazine 8 on the wire
		magazine = 8
	}

	// Row 0 is the page header: refresh the active page of this magazine
	if row == teletextRow0 {
		if page, ok := decodeTeletextPage(unit); ok {
			e.activePage[magazine] = page
		}
	}

	// Only subtitle data units are part of the extracted subtitle sequence
	if unitID != TeletextDataUnitIDSubtitle {
		return
	}

	// Page filtering: the descriptor pins one magazine/page, the stream is shared
	// by every page of the service.
	if e.selected != nil && e.selected.Magazine != 0 && magazine != e.selected.Magazine {
		return
	}
	page := e.activePage[magazine]
	if e.selected != nil && e.selected.Page != 0 && page != 0 && page != e.selected.Page {
		return
	}

	e.emitted = append(e.emitted, &TeletextEvent{
		Kind: TeletextEventKindDataUnit,
		DataUnit: &TeletextDataUnit{
			PID:           pid,
			ProgramNumber: e.programNumber,
			Language:      append([]byte(nil), e.selected.Language...),
			DataUnitID:    unitID,
			PTS:           pts,
			PTSPresent:    ptsPresent,
			Data:          unit,
			Magazine:      magazine,
			Row:           row,
			Page:          page,
			Corrected:     corrected,
		},
	})
}

// decodeTeletextPage decodes the BCD page number from a row 0 page header data
// unit. The page tens/units digits live in the low nibbles of the two bytes
// following the address bytes. ok is false when the nibbles are not BCD digits.
func decodeTeletextPage(unit []byte) (page uint8, ok bool) {
	tens := unit[teletextAddressByteOffset+2] & 0x0f
	units := unit[teletextAddressByteOffset+3] & 0x0f
	if tens > 9 || units > 9 {
		return 0, false
	}
	return tens*10 + units, true
}

// queuePendingError appends an error event produced outside of HandleData,
// typically by the packet skipper while the demuxer is reading packets. The
// event is held until the next HandleData call drains it.
func (e *TeletextExtractor) queuePendingError(pid uint16, err error) {
	e.pending = append(e.pending, &TeletextEvent{
		Kind: TeletextEventKindError,
		Err:  err,
		PID:  pid,
	})
}
