package astits

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astikit"
)

const (
	startPID           uint16 = 0x0100
	pmtStartPID        uint16 = 0x1000
	programNumberStart uint16 = 1
)

var (
	ErrPIDNotFound      = errors.New("astits: PID not found")
	ErrPIDAlreadyExists = errors.New("astits: PID already exists")
	ErrPCRPIDInvalid    = errors.New("astits: PCR PID invalid")

	ErrSDTServiceNotFound   = errors.New("astits: SDT service ID doesn't match any program")
	ErrSDTNameTooLong       = errors.New("astits: SDT service name is too long")
	ErrSDTProviderTooLong   = errors.New("astits: SDT provider name is too long")
	ErrSDTSectionDoesNotFit = errors.New("astits: SDT section doesn't fit into a single TS packet")
	maxSDTStringLength      = 255 // service name and provider name are length-prefixed with a single byte
)

type Muxer struct {
	ctx        context.Context
	w          io.Writer
	bitsWriter *astikit.BitsWriter

	packetSize             int
	tablesRetransmitPeriod int // period in PES packets

	transportStreamID uint16
	originalNetworkID uint16
	pmtPID            uint16

	pm         *programMap // pid -> programNumber
	pmUpdated  bool
	pmt        PMTData
	pmtUpdated bool
	nextPID    uint16
	patVersion wrappingCounter
	pmtVersion wrappingCounter
	patCC      wrappingCounter
	pmtCC      wrappingCounter

	sdt        SDTData
	sdtEnabled bool
	sdtUpdated bool
	sdtVersion wrappingCounter
	sdtCC      wrappingCounter

	patBytes bytes.Buffer
	pmtBytes bytes.Buffer
	sdtBytes bytes.Buffer

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

// WithOriginalNetworkID sets the original network ID written into SDT.
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

		pm: newProgramMap(),
		pmt: PMTData{
			ElementaryStreams: []*PMTElementaryStream{},
			ProgramNumber:     programNumberStart,
		},

		// table version is 5-bit field
		patVersion: newWrappingCounter(0b11111),
		pmtVersion: newWrappingCounter(0b11111),
		sdtVersion: newWrappingCounter(0b11111),

		patCC: newWrappingCounter(0b1111),
		pmtCC: newWrappingCounter(0b1111),
		sdtCC: newWrappingCounter(0b1111),

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

// SetSDT enables writing an SDT describing the given service alongside PAT and PMT.
// The service ID must match the program number of an existing program so that receivers
// can associate the service with the PMT (the muxer currently supports a single program,
// so it must be 1). The service and provider names are advertised through a service
// descriptor. Calling SetSDT again updates the SDT content and bumps its version number.
func (m *Muxer) SetSDT(serviceID uint16, name, provider string) error {
	if serviceID != m.pmt.ProgramNumber {
		return fmt.Errorf("%w: %d", ErrSDTServiceNotFound, serviceID)
	}
	if len(name) > maxSDTStringLength {
		return fmt.Errorf("%w: %d bytes", ErrSDTNameTooLong, len(name))
	}
	if len(provider) > maxSDTStringLength {
		return fmt.Errorf("%w: %d bytes", ErrSDTProviderTooLong, len(provider))
	}

	serviceDescriptor := &DescriptorService{
		Type:     ServiceTypeDigitalTelevision,
		Name:     []byte(name),
		Provider: []byte(provider),
	}

	sdt := SDTData{
		OriginalNetworkID: m.originalNetworkID,
		TransportStreamID: m.transportStreamID,
		Services: []*SDTDataService{
			{
				Descriptors: []*Descriptor{
					{
						Tag:     DescriptorTagService,
						Length:  calcDescriptorServiceLength(serviceDescriptor),
						Service: serviceDescriptor,
					},
				},
				RunningStatus: RunningStatusRunning,
				ServiceID:     serviceID,
			},
		},
	}

	// The muxer only supports tables spanning a single TS packet:
	// pointer field (1) + section header (3) + syntax header (5) + data + CRC32 (4)
	if 13+int(calcSDTSectionLength(&sdt)) > m.packetSize-mpegTsPacketHeaderSize {
		return ErrSDTSectionDoesNotFit
	}

	m.sdt = sdt
	m.sdtEnabled = true
	m.sdtUpdated = true
	m.sdtBytes.Reset()
	return nil
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

	if m.sdtEnabled {
		if err := m.generateSDT(); err != nil {
			return bytesWritten, err
		}
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

	if m.sdtEnabled {
		n, err = m.w.Write(m.sdtBytes.Bytes())
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

func (m *Muxer) generateSDT() error {
	// Refresh identifiers in case the options were changed after SetSDT
	m.sdt.TransportStreamID = m.transportStreamID
	m.sdt.OriginalNetworkID = m.originalNetworkID

	versionNumber := m.sdtVersion.get()
	if m.sdtUpdated {
		versionNumber = m.sdtVersion.inc()
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{SDT: &m.sdt},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for SDT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: m.sdt.TransportStreamID,
			VersionNumber:    uint8(versionNumber),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcSDTSectionLength(&m.sdt),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDSDTVariant1,
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

	m.sdtBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.sdtBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       PIDSDT,
			ContinuityCounter:         uint8(m.sdtCC.inc()),
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old SDT and rollback to it here maybe?
		return err
	}

	m.sdtUpdated = false

	return nil
}
