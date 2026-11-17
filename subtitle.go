package astits

import (
	"bytes"
	"errors"
	"fmt"
)

// DVB subtitling descriptor subtitling type for DVB bitmap subtitles
// (EN 300 468, chapter 6.2.41). Values 0x10-0x1f indicate DVB subtitling
// services carried in private data PES streams.
const SubtitlingTypeDVBBitmap = 0x10

// data_identifier at the start of a DVB subtitling PES payload (EN 300 743,
// chapter 7.2.1). It is a fixed value of 0x20.
const dvbSubtitlingDataIdentifier = 0x20

// sync_byte introducing every DVB subtitling segment (EN 300 743,
// chapter 7.2.1).
const dvbSubtitlingSegmentSyncByte = 0x0f

// DVB subtitle segment types (EN 300 743, chapter 7.2.1, table 3)
const (
	DVBSubtitleSegmentTypePCS          DVBSubtitleSegmentType = 0x10 // page composition segment
	DVBSubtitleSegmentTypeRCS          DVBSubtitleSegmentType = 0x11 // region composition segment
	DVBSubtitleSegmentTypeCLUT         DVBSubtitleSegmentType = 0x12 // CLUT definition segment
	DVBSubtitleSegmentTypeObjectFirst  DVBSubtitleSegmentType = 0x13 // first object data segment
	DVBSubtitleSegmentTypeObjectLast   DVBSubtitleSegmentType = 0x14 // last object data segment
	DVBSubtitleSegmentTypeDisplay      DVBSubtitleSegmentType = 0x15 // display definition segment
	DVBSubtitleSegmentTypeDisparity    DVBSubtitleSegmentType = 0x16 // disparity signalling segment
	DVBSubtitleSegmentTypeDoubleWindow DVBSubtitleSegmentType = 0x17 // double window object data segment
	DVBSubtitleSegmentTypeTone         DVBSubtitleSegmentType = 0x18 // tone mapping segment
	DVBSubtitleSegmentTypeAuxiliary    DVBSubtitleSegmentType = 0xf0 // auxiliary segment
	DVBSubtitleSegmentTypeEnd          DVBSubtitleSegmentType = 0xff // end of display set segment
)

// Errors returned by the DVB subtitle extractor. They can be tested with
// errors.Is.
var (
	// ErrDVBSubtitleSyncByte is returned when a DVB subtitling PES payload
	// does not start with the 0x20 data_identifier or when a segment does not
	// start with the 0x0f segment sync_byte.
	ErrDVBSubtitleSyncByte = errors.New("astits: invalid DVB subtitling sync byte")

	// ErrDVBSubtitleSegmentLength is returned when a segment_length field
	// points past the end of the PES payload.
	ErrDVBSubtitleSegmentLength = errors.New("astits: DVB subtitling segment length exceeds PES payload")

	// ErrDVBSubtitlePTSMissing is returned when a subtitling PES packet
	// carries no PTS in its optional header even though DVB subtitling PES
	// packets must contain one.
	ErrDVBSubtitlePTSMissing = errors.New("astits: DVB subtitling PES packet has no PTS")
)

// DVBSubtitleSegmentType represents the type of a DVB subtitle segment.
type DVBSubtitleSegmentType uint8

// String implements the Stringer interface.
func (t DVBSubtitleSegmentType) String() string {
	switch t {
	case DVBSubtitleSegmentTypePCS:
		return "page_composition"
	case DVBSubtitleSegmentTypeRCS:
		return "region_composition"
	case DVBSubtitleSegmentTypeCLUT:
		return "clut_definition"
	case DVBSubtitleSegmentTypeObjectFirst:
		return "object_data_first"
	case DVBSubtitleSegmentTypeObjectLast:
		return "object_data_last"
	case DVBSubtitleSegmentTypeDisplay:
		return "display_definition"
	case DVBSubtitleSegmentTypeDisparity:
		return "disparity_signalling"
	case DVBSubtitleSegmentTypeDoubleWindow:
		return "double_window_object_data"
	case DVBSubtitleSegmentTypeTone:
		return "tone_mapping"
	case DVBSubtitleSegmentTypeAuxiliary:
		return "auxiliary"
	case DVBSubtitleSegmentTypeEnd:
		return "end_of_display_set"
	}
	return fmt.Sprintf("unknown(0x%02x)", uint8(t))
}

// DVBSubtitleSegment represents a raw DVB subtitle segment (EN 300 743).
// No decoding or rendering of the bitmap is performed.
type DVBSubtitleSegment struct {
	// AncillaryPageID is the ancillary page ID carried by the segment header.
	AncillaryPageID uint16
	// CompositionPageID is the composition page ID carried by the segment
	// header. Only segments whose composition page matches the one selected
	// through the PMT subtitling descriptor are emitted.
	CompositionPageID uint16
	// Data is the raw segment payload, i.e. the segment_length bytes following
	// the segment header (segment_type, page_id and segment_length excluded).
	Data []byte
	// Language is the ISO 639-2/B language code selected from the PMT
	// subtitling descriptor.
	Language []byte
	// PID is the transport stream PID carrying the segment.
	PID uint16
	// ProgramNumber is the program the subtitling stream belongs to.
	ProgramNumber uint16
	// PTS is the presentation timestamp of the PES packet carrying the
	// segment.
	PTS *ClockReference
	// SegmentNumber is the 0-based index of the segment across all segments
	// emitted by the extractor. It increments monotonically across PES packet
	// and program boundaries.
	SegmentNumber int
	// Type is the segment_type byte.
	Type DVBSubtitleSegmentType
}

// DVBSubtitleEventType discriminates the events emitted by the extractor.
type DVBSubtitleEventType uint8

const (
	// DVBSubtitleEventTypeSegment is emitted for every subtitle segment
	// matching the selected language and composition page.
	DVBSubtitleEventTypeSegment DVBSubtitleEventType = iota
	// DVBSubtitleEventTypePIDChange is emitted when the PMT subtitling
	// descriptor moves the selected subtitling stream to another PID.
	DVBSubtitleEventTypePIDChange
	// DVBSubtitleEventTypeDiscontinuity is emitted when a continuity counter
	// break is detected on a PID carrying a registered subtitling stream.
	DVBSubtitleEventTypeDiscontinuity
)

// DVBSubtitleEvent represents an extractor event.
type DVBSubtitleEvent struct {
	// Discontinuity is set when Type is DVBSubtitleEventTypeDiscontinuity.
	Discontinuity *DVBSubtitleDiscontinuity
	// PIDChange is set when Type is DVBSubtitleEventTypePIDChange.
	PIDChange *DVBSubtitlePIDChange
	// Segment is set when Type is DVBSubtitleEventTypeSegment.
	Segment *DVBSubtitleSegment
	// Type discriminates the event.
	Type DVBSubtitleEventType
}

// DVBSubtitlePIDChange describes a subtitling PID reassignment detected in
// an updated PMT.
type DVBSubtitlePIDChange struct {
	CompositionPageID uint16
	Language          []byte
	NewPID            uint16
	OldPID            uint16
	ProgramNumber     uint16
}

// DVBSubtitleDiscontinuity describes a continuity counter break on a
// subtitling PID.
type DVBSubtitleDiscontinuity struct {
	PID uint16
}

// dvbSubtitleStream describes one subtitling entry advertised through a PMT
// subtitling descriptor.
type dvbSubtitleStream struct {
	ancillaryPageID   uint16
	compositionPageID uint16
	language          []byte
	pid               uint16
	programNumber     uint16
}

// DVBSubtitleExtractor extracts raw DVB bitmap subtitle segments from a
// transport stream. Subtitling streams are selected from the PMT subtitling
// descriptors (tag 0x59) by language and composition page ID.
type DVBSubtitleExtractor struct {
	dmx *Demuxer

	language          []byte
	compositionPageID uint16
	hasPageFilter     bool

	// Registered subtitling streams, indexed by PID. The registration order
	// slice keeps stream selection deterministic.
	streams     map[uint32]*dvbSubtitleStream
	streamOrder []uint16
	// programPIDs tracks the subtitling PIDs currently advertised by each
	// program so an updated PMT can retire PIDs that disappeared.
	programPIDs  map[uint32]map[uint32]struct{}
	activePID    uint16
	hasActivePID bool

	// segmentCounter is a monotonically increasing index assigned to every
	// emitted segment across the whole stream, regardless of PES boundaries.
	segmentCounter int

	events []*DVBSubtitleEvent
}

// DVBSubtitleExtractorOpt configures a DVBSubtitleExtractor.
type DVBSubtitleExtractorOpt func(e *DVBSubtitleExtractor)

// DVBSubtitleOptLanguage restricts subtitling stream selection to the given
// ISO 639-2/B language code (e.g. []byte("eng")).
func DVBSubtitleOptLanguage(language []byte) DVBSubtitleExtractorOpt {
	return func(e *DVBSubtitleExtractor) {
		e.language = bytes.Clone(language)
	}
}

// DVBSubtitleOptCompositionPage restricts subtitling stream selection to the
// given composition page ID advertised in the PMT subtitling descriptor.
func DVBSubtitleOptCompositionPage(compositionPageID uint16) DVBSubtitleExtractorOpt {
	return func(e *DVBSubtitleExtractor) {
		e.compositionPageID = compositionPageID
		e.hasPageFilter = true
	}
}

// NewDVBSubtitleExtractor creates a DVB bitmap subtitle extractor bound to
// the provided demuxer. The extractor installs a discontinuity handler on the
// demuxer; a previously installed handler is preserved and still called.
func NewDVBSubtitleExtractor(dmx *Demuxer, opts ...DVBSubtitleExtractorOpt) *DVBSubtitleExtractor {
	e := &DVBSubtitleExtractor{
		dmx:         dmx,
		streams:     make(map[uint32]*dvbSubtitleStream),
		programPIDs: make(map[uint32]map[uint32]struct{}),
	}
	for _, opt := range opts {
		opt(e)
	}

	previous := dmx.optDiscontinuity
	dmx.optDiscontinuity = func(pid uint16) {
		if _, ok := e.streams[uint32(pid)]; ok {
			e.events = append(e.events, &DVBSubtitleEvent{
				Type:          DVBSubtitleEventTypeDiscontinuity,
				Discontinuity: &DVBSubtitleDiscontinuity{PID: pid},
			})
		}
		if previous != nil {
			previous(pid)
		}
	}
	// The packet pool may already have been created before the extractor.
	if dmx.packetPool != nil {
		dmx.packetPool.onDiscontinuity = dmx.optDiscontinuity
	}
	return e
}

// ActivePID returns the PID of the subtitling stream currently selected from
// the PMT descriptors and whether a stream has been selected yet.
func (e *DVBSubtitleExtractor) ActivePID() (pid uint16, ok bool) {
	return e.activePID, e.hasActivePID
}

// handlePMT reconciles the subtitling streams advertised by a PMT and
// re-evaluates the selected stream. PIDs that the updated PMT no longer
// advertises for this program are retired. A PMT carrying no subtitling
// descriptor at all is ignored: it cannot authoritatively retire a subtitling
// stream (e.g. an unrelated retransmitted table), so known streams are kept.
func (e *DVBSubtitleExtractor) handlePMT(pmt *PMTData) {
	programKey := uint32(pmt.ProgramNumber)
	advertised := make(map[uint32]struct{})
	foundSubtitling := false

	for _, es := range pmt.ElementaryStreams {
		for _, d := range es.ElementaryStreamDescriptors {
			if d.Tag != DescriptorTagSubtitling || d.Subtitling == nil {
				continue
			}
			foundSubtitling = true
			for _, item := range d.Subtitling.Items {
				pidKey := uint32(es.ElementaryPID)
				advertised[pidKey] = struct{}{}
				e.register(&dvbSubtitleStream{
					ancillaryPageID:   item.AncillaryPageID,
					compositionPageID: item.CompositionPageID,
					language:          bytes.Clone(item.Language),
					pid:               es.ElementaryPID,
					programNumber:     pmt.ProgramNumber,
				})
			}
		}
	}

	// Only reconcile (retire PIDs) when this PMT actually describes
	// subtitling streams.
	if foundSubtitling {
		for pidKey := range e.programPIDs[programKey] {
			if _, stillThere := advertised[pidKey]; stillThere {
				continue
			}
			e.unregister(uint16(pidKey))
		}
		e.programPIDs[programKey] = advertised
	}

	e.selectStream()
}

// register adds or updates a subtitling stream.
func (e *DVBSubtitleExtractor) register(s *dvbSubtitleStream) {
	key := uint32(s.pid)
	if _, ok := e.streams[key]; !ok {
		e.streamOrder = append(e.streamOrder, s.pid)
	}
	e.streams[key] = s
}

// unregister removes a subtitling stream.
func (e *DVBSubtitleExtractor) unregister(pid uint16) {
	key := uint32(pid)
	if _, ok := e.streams[key]; !ok {
		return
	}
	delete(e.streams, key)
	for i, p := range e.streamOrder {
		if p == pid {
			e.streamOrder = append(e.streamOrder[:i], e.streamOrder[i+1:]...)
			break
		}
	}
}

// matches reports whether a stream passes the language and composition page
// filters.
func (e *DVBSubtitleExtractor) matches(s *dvbSubtitleStream) bool {
	if e.language != nil && !bytes.Equal(s.language, e.language) {
		return false
	}
	if e.hasPageFilter && s.compositionPageID != e.compositionPageID {
		return false
	}
	return true
}

// selectStream picks the first matching stream in registration order and
// emits a PID change event when the selected PID moves.
func (e *DVBSubtitleExtractor) selectStream() {
	var selected *dvbSubtitleStream
	for _, pid := range e.streamOrder {
		s := e.streams[uint32(pid)]
		if e.matches(s) {
			selected = s
			break
		}
	}

	oldPID, hadOld := e.activePID, e.hasActivePID
	if selected == nil {
		e.activePID, e.hasActivePID = 0, false
		return
	}
	e.activePID, e.hasActivePID = selected.pid, true

	if hadOld && oldPID != selected.pid {
		e.events = append(e.events, &DVBSubtitleEvent{
			Type: DVBSubtitleEventTypePIDChange,
			PIDChange: &DVBSubtitlePIDChange{
				CompositionPageID: selected.compositionPageID,
				Language:          bytes.Clone(selected.language),
				NewPID:            selected.pid,
				OldPID:            oldPID,
				ProgramNumber:     selected.programNumber,
			},
		})
	}
}

// NextEvent returns the next extractor event: a subtitle segment, a PID
// change or a continuity break. ErrNoMorePackets is returned at the end of
// the stream.
func (e *DVBSubtitleExtractor) NextEvent() (ev *DVBSubtitleEvent, err error) {
	for len(e.events) == 0 {
		var d *DemuxerData
		if d, err = e.dmx.NextData(); err != nil {
			return
		}
		if d.PMT != nil {
			e.handlePMT(d.PMT)
			continue
		}
		if d.PES == nil || !e.hasActivePID || d.PID != e.activePID {
			continue
		}
		if err = e.parsePES(d.PES, d.PID); err != nil {
			return
		}
	}
	ev = e.events[0]
	e.events = e.events[1:]
	return
}

// NextSegment returns the next subtitle segment, skipping PID change and
// discontinuity events.
func (e *DVBSubtitleExtractor) NextSegment() (s *DVBSubtitleSegment, err error) {
	for {
		var ev *DVBSubtitleEvent
		if ev, err = e.NextEvent(); err != nil {
			return
		}
		if ev.Type == DVBSubtitleEventTypeSegment {
			return ev.Segment, nil
		}
	}
}

// parsePES parses the DVB subtitling segments carried by one PES packet and
// appends matching segments to the event queue.
func (e *DVBSubtitleExtractor) parsePES(pes *PESData, pid uint16) error {
	stream := e.streams[uint32(pid)]
	if stream == nil {
		// The active PID always references a registered stream; ignore a PES
		// that arrives for an unregistered PID rather than panicking.
		return nil
	}

	// DVB subtitling PES packets must carry a PTS.
	if pes.Header == nil || pes.Header.OptionalHeader == nil || pes.Header.OptionalHeader.PTS == nil {
		return ErrDVBSubtitlePTSMissing
	}
	pts := pes.Header.OptionalHeader.PTS

	payload := pes.Data
	if len(payload) < 1 || payload[0] != dvbSubtitlingDataIdentifier {
		return fmt.Errorf("astits: data_identifier 0x%02x: %w", firstByte(payload), ErrDVBSubtitleSyncByte)
	}

	for offset := 1; offset < len(payload); {
		// Remaining stuffing bytes (0xff) are allowed at the end of the PES
		// payload.
		if payload[offset] == 0xff {
			break
		}

		// sync_byte
		if payload[offset] != dvbSubtitlingSegmentSyncByte {
			return fmt.Errorf("astits: segment sync_byte 0x%02x at offset %d: %w", payload[offset], offset, ErrDVBSubtitleSyncByte)
		}

		// sync_byte + segment_type + page_id (2 bytes) + segment_length
		// (2 bytes) make up the 6-byte segment header.
		const headerLength = 6
		if len(payload)-offset < headerLength {
			return fmt.Errorf("astits: truncated segment header (%d bytes left): %w", len(payload)-offset, ErrDVBSubtitleSegmentLength)
		}

		segmentType := payload[offset+1]
		pageID := uint16(payload[offset+2])<<8 | uint16(payload[offset+3])
		segmentLength := int(uint16(payload[offset+4])<<8 | uint16(payload[offset+5]))

		dataStart := offset + headerLength
		dataEnd := dataStart + segmentLength
		if dataEnd > len(payload) {
			return fmt.Errorf("astits: segment_length %d exceeds %d remaining PES bytes: %w", segmentLength, len(payload)-dataStart, ErrDVBSubtitleSegmentLength)
		}

		// Only emit segments of the composition page selected through the
		// PMT subtitling descriptor.
		if pageID == stream.compositionPageID {
			e.events = append(e.events, &DVBSubtitleEvent{
				Type: DVBSubtitleEventTypeSegment,
				Segment: &DVBSubtitleSegment{
					AncillaryPageID:   stream.ancillaryPageID,
					CompositionPageID: pageID,
					Data:              bytes.Clone(payload[dataStart:dataEnd]),
					Language:          bytes.Clone(stream.language),
					PID:               pid,
					ProgramNumber:     stream.programNumber,
					PTS:               pts,
					SegmentNumber:     e.segmentCounter,
					Type:              DVBSubtitleSegmentType(segmentType),
				},
			})
			e.segmentCounter++
		}
		offset = dataEnd
	}
	return nil
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
