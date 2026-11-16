package astits

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/asticode/go-astikit"
)

const (
	startPID           uint16 = 0x0100
	pmtStartPID        uint16 = 0x1000
	programNumberStart uint16 = 1

	// The event_name_length and text_length fields of a short event descriptor are 8 bits,
	// and so is the descriptor length. The latter leaves 250 bytes for the event name.
	maxEITEventNameLength = 250
)

var (
	ErrPIDNotFound      = errors.New("astits: PID not found")
	ErrPIDAlreadyExists = errors.New("astits: PID already exists")
	ErrPCRPIDInvalid    = errors.New("astits: PCR PID invalid")

	ErrEITDuplicateEventID     = errors.New("astits: EIT event IDs must be unique")
	ErrEITEventsOutOfOrder     = errors.New("astits: EIT next event must start after current event")
	ErrEITEventDurationInvalid = errors.New("astits: EIT event duration invalid")
	ErrEITEventLanguageInvalid = errors.New("astits: EIT event language must be a 3-byte ISO 639 code")
	ErrEITEventTitleTooLong    = errors.New("astits: EIT event title is too long")
	ErrEITStartTimeInvalid     = errors.New("astits: EIT event start time cannot be represented")

	eitMaxDuration = 99*time.Hour + 59*time.Minute + 59*time.Second

	// DVB times are 16-bit MJDs followed by BCD hours, minutes and seconds. 16-bit MJDs
	// cover 1858-11-17 through 2038-04-22, but parseDVBTime only round-trips correctly from
	// 1900-03-01 onwards, which is therefore the lower bound accepted by the muxer.
	eitMinStartTime = time.Date(1900, time.March, 1, 0, 0, 0, 0, time.UTC)
	eitMaxStartTime = time.Date(2038, time.April, 22, 23, 59, 59, 0, time.UTC)
)

// EITEvent represents an event advertised through an EIT present/following section.
type EITEvent struct {
	Duration  time.Duration
	EventID   uint16
	Language  string // 3-byte ISO 639 language code, e.g. "eng"
	StartTime time.Time
	Title     string // Short event name
}

type Muxer struct {
	ctx        context.Context
	w          io.Writer
	bitsWriter *astikit.BitsWriter

	packetSize             int
	tablesRetransmitPeriod int // period in PES packets

	transportStreamID uint16
	originalNetworkID uint16
	pmtPID            uint16
	eitPID            uint16

	pm         *programMap // pid -> programNumber
	pmUpdated  bool
	pmt        PMTData
	pmtUpdated bool
	nextPID    uint16
	patVersion wrappingCounter
	pmtVersion wrappingCounter
	patCC      wrappingCounter
	pmtCC      wrappingCounter

	eit        EITData
	eitEnabled bool
	eitUpdated bool
	eitWritten bool
	eitVersion wrappingCounter
	eitCC      wrappingCounter

	patBytes bytes.Buffer
	pmtBytes bytes.Buffer
	eitBytes bytes.Buffer

	buf       bytes.Buffer
	bufWriter *astikit.BitsWriter

	// We use map[uint32] instead map[uint16] as go runtime provide optimized hash functions for (u)int32/64 keys
	esContexts              map[uint32]*esContext
	tablesRetransmitCounter int
}

type esContext struct {
	es *PMTElementaryStream
	cc wrappingCounter
}

func newEsContext(es *PMTElementaryStream) *esContext {
	return &esContext{
		es: es,
		cc: newWrappingCounter(0b1111), // CC is 4 bits
	}
}

func MuxerOptTablesRetransmitPeriod(newPeriod int) func(*Muxer) {
	return func(m *Muxer) {
		m.tablesRetransmitPeriod = newPeriod
	}
}

// WithTransportStreamID sets the transport stream ID written into PAT.
// Default is 0.
func WithTransportStreamID(id uint16) func(*Muxer) {
	return func(m *Muxer) {
		m.transportStreamID = id
	}
}

// WithPMTPID sets the PID used for PMT packets and advertised in PAT.
// Default is 0x1000.
func WithPMTPID(pid uint16) func(*Muxer) {
	return func(m *Muxer) {
		m.pmtPID = pid
	}
}

// WithEITPID sets the PID used for EIT packets.
// Default is 0x12, the PID reserved by EN 300 468 for EIT present/following of the actual
// transport stream.
func WithEITPID(pid uint16) func(*Muxer) {
	return func(m *Muxer) {
		m.eitPID = pid
	}
}

// WithOriginalNetworkID sets the original network ID advertised in EIT.
// Default is 0.
func WithOriginalNetworkID(id uint16) func(*Muxer) {
	return func(m *Muxer) {
		m.originalNetworkID = id
	}
}

// TODO MuxerOptAutodetectPCRPID selecting first video PID for each PMT, falling back to first audio, falling back to any other

func NewMuxer(ctx context.Context, w io.Writer, opts ...func(*Muxer)) *Muxer {
	m := &Muxer{
		ctx: ctx,
		w:   w,

		packetSize:             MpegTsPacketSize, // no 192-byte packet support yet
		tablesRetransmitPeriod: 40,

		pmtPID: pmtStartPID,
		eitPID: PIDEIT,

		pm: newProgramMap(),
		pmt: PMTData{
			ElementaryStreams: []*PMTElementaryStream{},
			ProgramNumber:     programNumberStart,
		},

		// table version is 5-bit field
		patVersion: newWrappingCounter(0b11111),
		pmtVersion: newWrappingCounter(0b11111),
		eitVersion: newWrappingCounter(0b11111),

		patCC: newWrappingCounter(0b1111),
		pmtCC: newWrappingCounter(0b1111),
		eitCC: newWrappingCounter(0b1111),

		esContexts: map[uint32]*esContext{},
	}

	m.bufWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	m.bitsWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m.w})

	for _, opt := range opts {
		opt(m)
	}

	// TODO multiple programs support
	m.pm.setUnlocked(m.pmtPID, programNumberStart)
	m.pmUpdated = true

	// to output tables at the very start
	m.tablesRetransmitCounter = m.tablesRetransmitPeriod

	return m
}

// if es.ElementaryPID is zero, it will be generated automatically
func (m *Muxer) AddElementaryStream(es PMTElementaryStream) error {
	if es.ElementaryPID != 0 {
		for _, oes := range m.pmt.ElementaryStreams {
			if oes.ElementaryPID == es.ElementaryPID {
				return ErrPIDAlreadyExists
			}
		}
	} else {
		es.ElementaryPID = m.nextPID
		m.nextPID++
	}

	m.pmt.ElementaryStreams = append(m.pmt.ElementaryStreams, &es)

	m.esContexts[uint32(es.ElementaryPID)] = newEsContext(&es)
	// invalidate pmt cache
	m.pmtBytes.Reset()
	m.pmtUpdated = true
	return nil
}

func (m *Muxer) RemoveElementaryStream(pid uint16) error {
	foundIdx := -1
	for i, oes := range m.pmt.ElementaryStreams {
		if oes.ElementaryPID == pid {
			foundIdx = i
			break
		}
	}

	if foundIdx == -1 {
		return ErrPIDNotFound
	}

	m.pmt.ElementaryStreams = append(m.pmt.ElementaryStreams[:foundIdx], m.pmt.ElementaryStreams[foundIdx+1:]...)
	delete(m.esContexts, uint32(pid))
	m.pmtBytes.Reset()
	m.pmtUpdated = true
	return nil
}

// SetPCRPID marks pid as one to look PCRs in
func (m *Muxer) SetPCRPID(pid uint16) {
	m.pmt.PCRPID = pid
	m.pmtUpdated = true
}

// SetEIT enables writing an EIT present/following section describing the given events
// alongside PAT and PMT. current is the event being broadcast and next the one following it.
// Both events are carried in a single section for the muxer program, and a short event
// descriptor advertises their language and title.
// The events must have distinct IDs, the next event must start after the current one, and
// their start time and duration must be representable as DVB time and BCD duration.
// Calling SetEIT again with different events updates the advertised events and bumps the EIT
// version number; calling it with the same events leaves the version number untouched.
func (m *Muxer) SetEIT(current, next EITEvent) error {
	if err := validateEITEvent(current, "current"); err != nil {
		return err
	}
	if err := validateEITEvent(next, "next"); err != nil {
		return err
	}
	if current.EventID == next.EventID {
		return fmt.Errorf("%w: %d", ErrEITDuplicateEventID, current.EventID)
	}
	if !next.StartTime.After(current.StartTime) {
		return fmt.Errorf("%w: next starts at %s, current starts at %s",
			ErrEITEventsOutOfOrder,
			next.StartTime.UTC().Format(time.RFC3339),
			current.StartTime.UTC().Format(time.RFC3339),
		)
	}

	eit := EITData{
		// The muxer supports a single program whose number is the service ID.
		ServiceID:         m.pmt.ProgramNumber,
		TransportStreamID: m.transportStreamID,
		OriginalNetworkID: m.originalNetworkID,
		// A present/following EIT has exactly one section.
		SegmentLastSectionNumber: 0,
		LastTableID:              uint8(PSITableIDEITPresentFollowing),
		Events: []*EITDataEvent{
			newEITDataEvent(current, RunningStatusRunning),
			newEITDataEvent(next, RunningStatusNotRunning),
		},
	}

	// Only bump the version number when the advertised content changes compared to what has
	// already been written; repeated identical calls are idempotent. Before the first write
	// the content is always considered updated so that the version is initialized correctly.
	m.eitUpdated = !m.eitWritten || !reflect.DeepEqual(m.eit, eit)
	m.eit = eit
	m.eitEnabled = true
	m.eitBytes.Reset()
	return nil
}

func validateEITEvent(e EITEvent, name string) error {
	if len(e.Language) != 3 {
		return fmt.Errorf("%w: %s has %d bytes", ErrEITEventLanguageInvalid, name, len(e.Language))
	}
	for i := 0; i < 3; i++ {
		if e.Language[i] < 'a' || e.Language[i] > 'z' {
			return fmt.Errorf("%w: %s language %q is not lowercase ASCII",
				ErrEITEventLanguageInvalid, name, e.Language)
		}
	}
	if len(e.Title) > maxEITEventNameLength {
		return fmt.Errorf("%w: %s has %d bytes, max is %d",
			ErrEITEventTitleTooLong, name, len(e.Title), maxEITEventNameLength)
	}
	if e.Duration <= 0 {
		return fmt.Errorf("%w: %s is %s", ErrEITEventDurationInvalid, name, e.Duration)
	}
	if e.Duration > eitMaxDuration {
		return fmt.Errorf("%w: %s is %s, max is %s",
			ErrEITEventDurationInvalid, name, e.Duration, eitMaxDuration)
	}
	if e.Duration%time.Second != 0 {
		return fmt.Errorf("%w: %s is %s, sub-second precision is not representable",
			ErrEITEventDurationInvalid, name, e.Duration)
	}
	if err := validateEITStartTime(e.StartTime, name); err != nil {
		return err
	}
	return nil
}

func validateEITStartTime(t time.Time, name string) error {
	if t.IsZero() {
		return fmt.Errorf("%w: %s start time is zero", ErrEITStartTimeInvalid, name)
	}
	u := t.UTC()
	if u.Nanosecond() != 0 {
		return fmt.Errorf("%w: %s start time %s has sub-second precision",
			ErrEITStartTimeInvalid, name, t.Format(time.RFC3339Nano))
	}
	if u.Before(eitMinStartTime) || u.After(eitMaxStartTime) {
		return fmt.Errorf("%w: %s start time %s is outside [%s, %s]",
			ErrEITStartTimeInvalid,
			name,
			u.Format(time.RFC3339),
			eitMinStartTime.Format(time.RFC3339),
			eitMaxStartTime.Format(time.RFC3339),
		)
	}
	return nil
}

func newEITDataEvent(e EITEvent, runningStatus uint8) *EITDataEvent {
	title := []byte(e.Title)
	return &EITDataEvent{
		StartTime:     e.StartTime.UTC(),
		Duration:      e.Duration,
		EventID:       e.EventID,
		RunningStatus: runningStatus,
		Descriptors: []*Descriptor{
			{
				Tag:    DescriptorTagShortEvent,
				Length: uint8(5 + len(title)),
				ShortEvent: &DescriptorShortEvent{
					Language:  []byte(e.Language),
					EventName: title,
					// Text is optional: advertise the title only.
					Text: []byte{},
				},
			},
		},
	}
}

// WriteData writes MuxerData to TS stream
// Currently only PES packets are supported
// Be aware that after successful call WriteData will set d.AdaptationField.StuffingLength value to zero
func (m *Muxer) WriteData(d *MuxerData) (int, error) {
	ctx, ok := m.esContexts[uint32(d.PID)]
	if !ok {
		return 0, ErrPIDNotFound
	}

	bytesWritten := 0

	forceTables := d.AdaptationField != nil &&
		d.AdaptationField.RandomAccessIndicator &&
		d.PID == m.pmt.PCRPID

	n, err := m.retransmitTables(forceTables)
	if err != nil {
		return n, err
	}

	bytesWritten += n

	payloadStart := true
	writeAf := d.AdaptationField != nil
	payloadBytesWritten := 0
	for payloadBytesWritten < len(d.PES.Data) {
		pktLen := 1 + mpegTsPacketHeaderSize // sync byte + header
		pkt := Packet{
			Header: PacketHeader{
				ContinuityCounter:         uint8(ctx.cc.inc()),
				HasAdaptationField:        writeAf,
				HasPayload:                false,
				PayloadUnitStartIndicator: false,
				PID:                       d.PID,
			},
		}

		if writeAf {
			pkt.AdaptationField = d.AdaptationField
			// one byte for adaptation field length field
			pktLen += 1 + int(calcPacketAdaptationFieldLength(d.AdaptationField))
			writeAf = false
		}

		bytesAvailable := m.packetSize - pktLen
		if payloadStart {
			pesHeaderLengthCurrent := pesHeaderLength + int(calcPESOptionalHeaderLength(d.PES.Header.OptionalHeader))
			// d.AdaptationField with pes header are too big, we don't have space to write pes header
			if bytesAvailable < pesHeaderLengthCurrent {
				pkt.Header.HasAdaptationField = true
				if pkt.AdaptationField == nil {
					pkt.AdaptationField = newStuffingAdaptationField(bytesAvailable)
				} else {
					pkt.AdaptationField.StuffingLength = bytesAvailable
				}
			} else {
				pkt.Header.HasPayload = true
				pkt.Header.PayloadUnitStartIndicator = true
			}
		} else {
			pkt.Header.HasPayload = true
		}

		if pkt.Header.HasPayload {
			m.buf.Reset()
			if d.PES.Header.StreamID == 0 {
				d.PES.Header.StreamID = ctx.es.StreamType.ToPESStreamID()
			}

			ntot, npayload, err := writePESData(
				m.bufWriter,
				d.PES.Header,
				d.PES.Data[payloadBytesWritten:],
				payloadStart,
				bytesAvailable,
			)
			if err != nil {
				return bytesWritten, err
			}

			payloadBytesWritten += npayload

			pkt.Payload = m.buf.Bytes()

			bytesAvailable -= ntot
			// if we still have some space in packet, we should stuff it with adaptation field stuffing
			// we can't stuff packets with 0xff at the end of a packet since it's not uncommon for PES payloads to have length unspecified
			if bytesAvailable > 0 {
				pkt.Header.HasAdaptationField = true
				if pkt.AdaptationField == nil {
					pkt.AdaptationField = newStuffingAdaptationField(bytesAvailable)
				} else {
					pkt.AdaptationField.StuffingLength = bytesAvailable
				}
			}

			n, err = writePacket(m.bitsWriter, &pkt, m.packetSize)
			if err != nil {
				return bytesWritten, err
			}

			bytesWritten += n

			payloadStart = false
		}
	}

	if d.AdaptationField != nil {
		d.AdaptationField.StuffingLength = 0
	}

	return bytesWritten, nil
}

// Writes given packet to MPEG-TS stream
// Stuffs with 0xffs if packet turns out to be shorter than target packet length
func (m *Muxer) WritePacket(p *Packet) (int, error) {
	return writePacket(m.bitsWriter, p, m.packetSize)
}

func (m *Muxer) retransmitTables(force bool) (int, error) {
	m.tablesRetransmitCounter++
	if !force && m.tablesRetransmitCounter < m.tablesRetransmitPeriod {
		return 0, nil
	}

	n, err := m.WriteTables()
	if err != nil {
		return n, err
	}

	m.tablesRetransmitCounter = 0
	return n, nil
}

func (m *Muxer) WriteTables() (int, error) {
	bytesWritten := 0

	if err := m.generatePAT(); err != nil {
		return bytesWritten, err
	}

	if err := m.generatePMT(); err != nil {
		return bytesWritten, err
	}

	n, err := m.w.Write(m.patBytes.Bytes())
	if err != nil {
		return bytesWritten, err
	}
	bytesWritten += n

	n, err = m.w.Write(m.pmtBytes.Bytes())
	if err != nil {
		return bytesWritten, err
	}
	bytesWritten += n

	if m.eitEnabled {
		if err := m.generateEIT(); err != nil {
			return bytesWritten, err
		}

		n, err = m.w.Write(m.eitBytes.Bytes())
		if err != nil {
			return bytesWritten, err
		}
		bytesWritten += n
	}

	return bytesWritten, nil
}

func (m *Muxer) generatePAT() error {
	d := m.pm.toPATDataUnlocked()
	d.TransportStreamID = m.transportStreamID

	versionNumber := m.patVersion.get()
	if m.pmUpdated {
		versionNumber = m.patVersion.inc()
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{PAT: d},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for PAT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: d.TransportStreamID,
			VersionNumber:    uint8(versionNumber),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPATSectionLength(d),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDPAT,
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.patBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.patBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       PIDPAT,
			ContinuityCounter:         uint8(m.patCC.inc()),
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old PAT and rollback to it here maybe?
		return err
	}

	m.pmUpdated = false

	return nil
}

func (m *Muxer) generatePMT() error {
	hasPCRPID := false
	for _, es := range m.pmt.ElementaryStreams {
		if es.ElementaryPID == m.pmt.PCRPID {
			hasPCRPID = true
			break
		}
	}
	if !hasPCRPID {
		return ErrPCRPIDInvalid
	}

	versionNumber := m.pmtVersion.get()
	if m.pmtUpdated {
		versionNumber = m.pmtVersion.inc()
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{PMT: &m.pmt},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for PMT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: m.pmt.ProgramNumber,
			VersionNumber:    uint8(versionNumber),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPMTSectionLength(&m.pmt),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDPMT,
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.pmtBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.pmtBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       m.pmtPID, // FIXME multiple programs support
			ContinuityCounter:         uint8(m.pmtCC.inc()),
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old PMT and rollback to it here maybe?
		return err
	}

	m.pmtUpdated = false

	return nil
}

func (m *Muxer) generateEIT() error {
	// Refresh identifiers in case the options were changed after SetEIT
	m.eit.ServiceID = m.pmt.ProgramNumber
	m.eit.TransportStreamID = m.transportStreamID
	m.eit.OriginalNetworkID = m.originalNetworkID

	versionNumber := m.eitVersion.get()
	if m.eitUpdated {
		versionNumber = m.eitVersion.inc()
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{EIT: &m.eit},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// A present/following EIT has exactly one section
			SectionNumber:     0,
			LastSectionNumber: 0,
			TableIDExtension:  m.eit.ServiceID,
			VersionNumber:     uint8(versionNumber),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			// Syntax header (5) + section data + CRC32 (4)
			SectionLength:          5 + calcEITSectionLength(&m.eit) + 4,
			SectionSyntaxIndicator: true,
			// EN 300 468 5.2.4: EIT is a long-form section with section_syntax_indicator = 1
			// and private_indicator = 1.
			PrivateBit: true,
			TableID:    PSITableIDEITPresentFollowing,
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.eitBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.eitBytes})
	if _, err := m.writePSITablePackets(wPacket, m.buf.Bytes(), m.eitPID, &m.eitCC); err != nil {
		return err
	}

	m.eitUpdated = false
	m.eitWritten = true

	return nil
}

// writePSITablePackets splits a table payload (pointer field followed by a single section)
// into TS packets. The payload unit start indicator and the pointer field only appear in the
// first packet. The last packet is padded with 0xff bytes, which parsers stop reading once
// section_length has been consumed. The continuity counter is incremented for every packet.
func (m *Muxer) writePSITablePackets(w *astikit.BitsWriter, payload []byte, pid uint16, cc *wrappingCounter) (int, error) {
	bytesWritten := 0
	first := true
	for len(payload) > 0 {
		maxPayload := m.packetSize - 1 - mpegTsPacketHeaderSize // sync byte + header
		chunk := payload
		if len(chunk) > maxPayload {
			chunk = chunk[:maxPayload]
		}
		payload = payload[len(chunk):]

		pkt := Packet{
			Header: PacketHeader{
				ContinuityCounter:         uint8(cc.inc()),
				HasPayload:                true,
				PayloadUnitStartIndicator: first,
				PID:                       pid,
			},
			Payload: chunk,
		}

		n, err := writePacket(w, &pkt, m.packetSize)
		if err != nil {
			return bytesWritten, err
		}
		bytesWritten += n
		first = false
	}
	return bytesWritten, nil
}
