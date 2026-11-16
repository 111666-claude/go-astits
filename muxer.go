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
)

type Muxer struct {
	ctx        context.Context
	w          io.Writer
	bitsWriter *astikit.BitsWriter

	packetSize             int
	tablesRetransmitPeriod int // period in PES packets

	// m2tsEnabled makes the muxer emit 192-byte BDAV records instead of plain
	// 188-byte MPEG-TS packets. When enabled, m2ts wraps m.w and every packet
	// is prepended with a 4-byte copy-permission/ATS header.
	m2tsEnabled        bool
	m2ts               *m2tsWriter
	m2tsClock          M2TSClock
	m2tsCopyPermission uint8

	transportStreamID uint16
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

	patBytes bytes.Buffer
	pmtBytes bytes.Buffer

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

// WithM2TS enables 192-byte M2TS (BDAV) output. Every 188-byte MPEG-TS packet
// is prepended with a 4-byte header carrying a 2-bit copy permission indicator
// and a 30-bit arrival time stamp on the 90 kHz time base.
//
// clock is injected so that the arrival time stamps are deterministic in
// tests; pass nil to use a wall-clock driven 90 kHz clock. The clock must be
// monotonic, a backward jump makes the next Write fail with
// ErrM2TSClockBackwards. copyPermission must be one of the
// M2TSCopyPermission* constants (0..3), any other value is rejected with
// ErrM2TSCopyPermissionInvalid.
//
// The continuity counters and the PAT/PMT/PES payloads are still computed over
// the plain 188-byte packets; only the on-disk framing changes.
func WithM2TS(clock M2TSClock, copyPermission uint8) func(*Muxer) {
	return func(m *Muxer) {
		m.m2tsEnabled = true
		m.m2tsCopyPermission = copyPermission
		m.m2tsClock = clock
	}
}

// TODO MuxerOptAutodetectPCRPID selecting first video PID for each PMT, falling back to first audio, falling back to any other

// NewMuxer creates a muxer writing to w. It returns an error only for invalid
// options, currently an out-of-range M2TS copy permission indicator
// (ErrM2TSCopyPermissionInvalid).
func NewMuxer(ctx context.Context, w io.Writer, opts ...func(*Muxer)) (*Muxer, error) {
	m := &Muxer{
		ctx: ctx,
		w:   w,

		packetSize:             MpegTsPacketSize, // continuity counters and payloads always use the 188-byte TS layout
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

		patCC: newWrappingCounter(0b1111),
		pmtCC: newWrappingCounter(0b1111),

		esContexts: map[uint32]*esContext{},
	}

	for _, opt := range opts {
		opt(m)
	}

	if m.m2tsEnabled {
		if m.m2tsCopyPermission > 3 {
			return nil, fmt.Errorf("%w: got %d", ErrM2TSCopyPermissionInvalid, m.m2tsCopyPermission)
		}
		if m.m2tsClock == nil {
			m.m2tsClock = newSystemM2TSClock()
		}
		m.m2ts = newM2TSWriter(m.w, m.m2tsClock, m.m2tsCopyPermission)
		// All packet bytes go through the BDAV wrapper, which prepends the
		// 4-byte header and guarantees 192-byte output records.
		m.bitsWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m.m2ts})
	} else {
		m.bitsWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m.w})
	}
	m.bufWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})

	// TODO multiple programs support
	m.pm.setUnlocked(m.pmtPID, programNumberStart)
	m.pmUpdated = true

	// to output tables at the very start
	m.tablesRetransmitCounter = m.tablesRetransmitPeriod

	return m, nil
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

// Flush reports any bytes buffered by the M2TS layer that did not complete a
// final 188-byte packet. In plain TS mode it is a no-op. Call it once after
// the last WriteData/WritePacket/WriteTables to turn a truncated final packet
// into ErrM2TSPacketBoundary instead of silently dropping data.
func (m *Muxer) Flush() error {
	if !m.m2tsEnabled {
		return nil
	}
	return m.m2ts.flush()
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

// packetWriter returns the writer that full 188-byte packets must be handed
// to. In M2TS mode it is the BDAV wrapper that adds the 4-byte header,
// otherwise it is the user-provided writer directly.
func (m *Muxer) packetWriter() io.Writer {
	if m.m2tsEnabled {
		return m.m2ts
	}
	return m.w
}

func (m *Muxer) WriteTables() (int, error) {
	bytesWritten := 0

	if err := m.generatePAT(); err != nil {
		return bytesWritten, err
	}

	if err := m.generatePMT(); err != nil {
		return bytesWritten, err
	}

	for _, table := range [][]byte{m.patBytes.Bytes(), m.pmtBytes.Bytes()} {
		n, err := m.packetWriter().Write(table)
		if err != nil {
			return bytesWritten, err
		}
		// A plain io.Writer may accept fewer bytes without error; treat that
		// as a failure so a truncated table can never be mistaken for a valid
		// packet stream.
		if n != len(table) {
			return bytesWritten, fmt.Errorf("astits: writing table wrote %d of %d bytes", n, len(table))
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
