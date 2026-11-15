package astits

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astikit"
)

// Program remuxer errors
var (
	// ErrProgramNotFound is returned when the requested program number is not
	// declared by any PAT seen in the input stream.
	ErrProgramNotFound = errors.New("astits: program not found in PAT")
	// ErrPMTNotFound is returned when the input stream ends without ever
	// carrying a valid PMT for the requested program.
	ErrPMTNotFound = errors.New("astits: PMT for requested program not found")
	// ErrPIDRemapInvalid is returned when an explicit PID remap targets a
	// reserved or otherwise unusable PID.
	ErrPIDRemapInvalid = errors.New("astits: invalid PID remap target")
	// ErrPIDRemapConflict is returned when two input PIDs are remapped to the
	// same output PID.
	ErrPIDRemapConflict = errors.New("astits: PID remap conflict")
	// ErrTableTooLarge is returned when a regenerated PAT/PMT does not fit in
	// a single TS packet.
	ErrTableTooLarge = errors.New("astits: regenerated PSI table exceeds one packet")
)

// RemuxerStats holds counters describing what a ProgramRemuxer did with the
// input packets.
type RemuxerStats struct {
	// PacketsRead is the total amount of 188-byte packets read from the input.
	PacketsRead uint64
	// PacketsWritten is the total amount of packets emitted (including
	// regenerated PAT/PMT packets).
	PacketsWritten uint64
	// PacketsDropped is the amount of input packets deliberately not emitted:
	// other programs' PIDs, unknown PIDs and null packets.
	PacketsDropped uint64
	// CorruptPackets is the amount of packets dropped because their transport
	// error indicator was set.
	CorruptPackets uint64
	// PSIErrors is the amount of PAT/PMT sections that could not be parsed
	// (truncated sections, CRC32 mismatch, ...). The previously valid table is
	// kept in use whenever one exists.
	PSIErrors uint64
	// PMTUpdates is the amount of valid PMT updates applied for the target
	// program, the initial PMT included.
	PMTUpdates uint64
	// TablesWritten is the amount of regenerated PAT/PMT packets emitted.
	// These packets are produced by the remuxer rather than forwarded from the
	// input, so they are not part of the PacketsRead/PacketsDropped balance.
	TablesWritten uint64
}

// ProgramRemuxer filters a (possibly multi-program) transport stream down to a
// single program and rewrites it as a standalone, player-readable transport
// stream.
//
// It works at the TS packet level:
//   - PAT and PMT are regenerated so that they only describe the requested
//     program. Elementary streams, descriptors and the PCR PID relationship
//     are preserved, CRC32 values are recomputed.
//   - Packets belonging to the program's elementary streams are passed through
//     untouched apart from their PID (when remapped) and continuity counter,
//     which is renumbered per output PID. PES payloads, PTS/DTS and PCR values
//     are never modified.
//   - Packets of other programs, unrelated SI tables (SDT, NIT, EIT, ...),
//     unknown PIDs and null packets are dropped.
type ProgramRemuxer struct {
	ctx context.Context
	r   io.Reader
	w   io.Writer

	programNumber uint16
	packetSize    int
	autoRemap     bool
	explicitRemap map[uint16]uint16
	tablesPeriod  int

	packetBuffer *packetBuffer

	// PSI reassembly. pm knows every PMT PID advertised by the latest PAT so
	// that accumulators treat them as PSI streams.
	pm             *programMap
	patAcc         *packetAccumulator
	pmtAccs        map[uint32]*packetAccumulator
	pmtVersionSeen map[uint32]uint8

	// Target program state
	patTSID      uint16
	pmtPID       uint16
	currentPMT   *PMTData
	programKnown bool // a PAT referencing the target program was seen
	pmtKnown     bool // a valid PMT for the target program was seen
	notFoundErr  bool // ErrProgramNotFound already surfaced

	// PID remapping: input PID -> output PID
	remap map[uint16]uint16
	// Output PIDs already assigned, for collision detection / auto allocation
	usedOutput map[uint16]uint16
	nextAuto   uint16

	// Continuity counters per output PID and per emitted table PID
	cc     map[uint16]wrappingCounter
	patCC  wrappingCounter
	pmtCC  wrappingCounter
	patVer wrappingCounter
	pmtVer wrappingCounter

	// Version numbers are bumped only when the table content actually changes,
	// as required by ISO/IEC 13818-1. The dirty flags carry that condition.
	patDirty bool
	pmtDirty bool

	// Cached regenerated table packets
	patPacket []byte
	pmtPacket []byte

	// Tables are re-emitted every tablesPeriod written elementary packets and
	// whenever the PAT/PMT changes.
	tableCounter int

	stats RemuxerStats
}

// RemuxerOpt is a ProgramRemuxer option.
type RemuxerOpt func(*ProgramRemuxer)

// RemuxerOptPacketSize sets the input packet size (188 by default, 192-byte
// packets are supported: the 4 leading bytes are discarded).
func RemuxerOptPacketSize(packetSize int) RemuxerOpt {
	return func(m *ProgramRemuxer) {
		m.packetSize = packetSize
	}
}

// RemuxerOptPIDRemap remaps an input PID to a different output PID. The
// remapping applies both to elementary stream packets and to the regenerated
// PMT (and PAT when the PMT PID itself is remapped). It may be called multiple
// times, once per remapped PID. Remapping a PID that does not belong to the
// target program is allowed but has no effect.
func RemuxerOptPIDRemap(inputPID, outputPID uint16) RemuxerOpt {
	return func(m *ProgramRemuxer) {
		m.explicitRemap[inputPID] = outputPID
	}
}

// RemuxerOptAutoRemapPIDs assigns fresh, collision-free PIDs to every
// elementary stream of the target program (starting at 0x0100) instead of
// keeping the input PIDs. Explicit RemuxerOptPIDRemap entries take precedence.
func RemuxerOptAutoRemapPIDs() RemuxerOpt {
	return func(m *ProgramRemuxer) {
		m.autoRemap = true
	}
}

// RemuxerOptTablesRetransmitPeriod sets how many elementary stream packets are
// emitted between two periodic PAT/PMT retransmissions. Defaults to 40.
func RemuxerOptTablesRetransmitPeriod(period int) RemuxerOpt {
	return func(m *ProgramRemuxer) {
		if period > 0 {
			m.tablesPeriod = period
		}
	}
}

// NewProgramRemuxer creates a ProgramRemuxer streaming a single-program
// transport stream from r into w.
func NewProgramRemuxer(ctx context.Context, r io.Reader, w io.Writer, programNumber uint16, opts ...RemuxerOpt) *ProgramRemuxer {
	m := &ProgramRemuxer{
		ctx:           ctx,
		r:             r,
		w:             w,
		programNumber: programNumber,
		packetSize:    MpegTsPacketSize,
		explicitRemap: map[uint16]uint16{},
		tablesPeriod:  40,

		pm:             newProgramMap(),
		pmtAccs:        map[uint32]*packetAccumulator{},
		pmtVersionSeen: map[uint32]uint8{},

		remap:      map[uint16]uint16{},
		usedOutput: map[uint16]uint16{},
		nextAuto:   startPID,

		cc:     map[uint16]wrappingCounter{},
		patCC:  newWrappingCounter(0b1111),
		pmtCC:  newWrappingCounter(0b1111),
		patVer: newWrappingCounter(0b11111),
		pmtVer: newWrappingCounter(0b11111),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// Stats returns a snapshot of the remuxer counters.
func (m *ProgramRemuxer) Stats() RemuxerStats {
	return m.stats
}

// Rewind rewinds the underlying reader (when supported) and resets all
// remuxer state so that the stream can be processed again from the start.
func (m *ProgramRemuxer) Rewind() (n int64, err error) {
	m.packetBuffer = nil
	m.pm = newProgramMap()
	m.patAcc = nil
	m.pmtAccs = map[uint32]*packetAccumulator{}
	m.pmtVersionSeen = map[uint32]uint8{}
	m.currentPMT = nil
	m.programKnown = false
	m.pmtKnown = false
	m.notFoundErr = false
	m.remap = map[uint16]uint16{}
	m.usedOutput = map[uint16]uint16{}
	m.nextAuto = startPID
	m.cc = map[uint16]wrappingCounter{}
	m.patCC = newWrappingCounter(0b1111)
	m.pmtCC = newWrappingCounter(0b1111)
	m.patVer = newWrappingCounter(0b11111)
	m.pmtVer = newWrappingCounter(0b11111)
	m.patPacket = nil
	m.pmtPacket = nil
	m.patDirty = false
	m.pmtDirty = false
	m.tableCounter = 0
	m.stats = RemuxerStats{}
	if n, err = rewind(m.r); err != nil {
		err = fmt.Errorf("astits: rewinding reader failed: %w", err)
		return
	}
	return
}

// RemuxProgram is a convenience wrapper around NewProgramRemuxer that processes
// the whole input in one call.
func RemuxProgram(ctx context.Context, r io.Reader, w io.Writer, programNumber uint16, opts ...RemuxerOpt) (RemuxerStats, error) {
	m := NewProgramRemuxer(ctx, r, w, programNumber, opts...)
	err := m.Remux()
	return m.Stats(), err
}

// Remux processes the whole input stream until io.EOF.
func (m *ProgramRemuxer) Remux() error {
	for {
		done, err := m.ProcessNextPacket()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// ProcessNextPacket reads one input packet and applies the filtering/remapping
// logic. It returns done=true when the input has been fully consumed. On EOF
// the remuxer makes sure a valid PAT/PMT pair was emitted, otherwise
// ErrProgramNotFound / ErrPMTNotFound is returned.
func (m *ProgramRemuxer) ProcessNextPacket() (done bool, err error) {
	// Check ctx error
	if err = m.ctx.Err(); err != nil {
		return
	}

	// Create packet buffer if not exists
	if m.packetBuffer == nil {
		if m.packetBuffer, err = newPacketBuffer(m.r, m.packetSize, nil); err != nil {
			err = fmt.Errorf("astits: creating packet buffer failed: %w", err)
			return
		}
	}

	// Fetch next packet
	var p *Packet
	if p, err = m.packetBuffer.next(); err != nil {
		if errors.Is(err, ErrNoMorePackets) {
			err = m.finalize()
			done = err == nil
			return
		}
		err = fmt.Errorf("astits: fetching next packet failed: %w", err)
		return
	}

	m.stats.PacketsRead++

	if err = m.processPacket(p); err != nil {
		return
	}
	return
}

// finalize is called once the input is exhausted.
func (m *ProgramRemuxer) finalize() error {
	if !m.programKnown {
		return fmt.Errorf("astits: program %d: %w", m.programNumber, ErrProgramNotFound)
	}
	if !m.pmtKnown {
		return fmt.Errorf("astits: program %d: %w", m.programNumber, ErrPMTNotFound)
	}
	return nil
}

func (m *ProgramRemuxer) processPacket(p *Packet) error {
	// Corrupt packets are unusable.
	if p.Header.TransportErrorIndicator {
		m.stats.CorruptPackets++
		m.stats.PacketsDropped++
		return nil
	}

	switch {
	case p.Header.PID == PIDNull:
		// Drop padding
		m.stats.PacketsDropped++
		return nil

	case p.Header.PID == PIDPAT:
		m.stats.PacketsDropped++ // regenerated instead of forwarded
		return m.handlePAT(p)

	case m.pm.existsUnlocked(p.Header.PID):
		// One of the PMT PIDs advertised by the latest PAT
		m.stats.PacketsDropped++ // regenerated instead of forwarded
		return m.handlePMT(p)

	default:
		return m.handleElementary(p)
	}
}

// handlePAT reassembles PAT sections and applies the new program map.
func (m *ProgramRemuxer) handlePAT(p *Packet) error {
	if m.patAcc == nil {
		m.patAcc = newPacketAccumulator(PIDPAT, m.pm)
	}

	ps := m.patAcc.add(p)
	if len(ps) == 0 {
		return nil
	}

	pat, err := m.parsePAT(ps)
	if err != nil {
		m.stats.PSIErrors++
		return nil
	}

	m.patTSID = pat.TransportStreamID

	// Rebuild the PMT PID map used for PSI reassembly and reset accumulators
	// of PMT PIDs that disappeared.
	newPM := newProgramMap()
	found := false
	var targetPMTPID uint16
	for _, prog := range pat.Programs {
		if prog.ProgramNumber == 0 {
			continue // NIT PID
		}
		newPM.setUnlocked(prog.ProgramMapID, prog.ProgramNumber)
		if prog.ProgramNumber == m.programNumber {
			found = true
			targetPMTPID = prog.ProgramMapID
		}
	}
	m.pm = newPM
	for pid, acc := range m.pmtAccs {
		if !m.pm.existsUnlocked(uint16(pid)) {
			delete(m.pmtAccs, pid)
			delete(m.pmtVersionSeen, uint32(pid))
		} else {
			acc.programMap = m.pm
		}
	}

	if !found {
		if !m.notFoundErr {
			m.notFoundErr = true
			return fmt.Errorf("astits: program %d: %w", m.programNumber, ErrProgramNotFound)
		}
		return nil
	}
	m.notFoundErr = false
	m.programKnown = true

	if m.pmtPID != targetPMTPID {
		m.pmtPID = targetPMTPID
		// The PMT PID changed: regenerate the PAT and re-emit tables so players
		// can locate the new PMT.
		m.patDirty = true
		if err := m.regeneratePAT(); err != nil {
			return err
		}
		if m.pmtKnown {
			return m.emitTables()
		}
	}
	return nil
}

// handlePMT reassembles PMT sections and updates the target program state.
func (m *ProgramRemuxer) handlePMT(p *Packet) error {
	pid := p.Header.PID
	acc, ok := m.pmtAccs[uint32(pid)]
	if !ok {
		acc = newPacketAccumulator(pid, m.pm)
		m.pmtAccs[uint32(pid)] = acc
	}

	ps := acc.add(p)
	if len(ps) == 0 {
		return nil
	}

	pmt, version, err := m.parsePMT(ps)
	if err != nil {
		m.stats.PSIErrors++
		return nil
	}

	// Only the requested program's PMT matters.
	if pmt.ProgramNumber != m.programNumber || pid != m.pmtPID {
		return nil
	}

	// Skip sections we already applied (retransmitted tables).
	if m.pmtKnown && m.pmtVersionSeen[uint32(pid)] == version {
		return nil
	}
	m.pmtVersionSeen[uint32(pid)] = version

	if err := m.applyPMT(pmt); err != nil {
		return err
	}
	return nil
}

// applyPMT stores the new PMT, recomputes the PID remap and regenerates the
// output tables.
func (m *ProgramRemuxer) applyPMT(pmt *PMTData) error {
	// Validate the PCR PID. 0x1fff means "no PCR" and is always valid; any
	// other value must reference a declared elementary stream.
	hasPCRPID := pmt.PCRPID == PIDNull
	if !hasPCRPID {
		for _, es := range pmt.ElementaryStreams {
			if es.ElementaryPID == pmt.PCRPID {
				hasPCRPID = true
				break
			}
		}
	}
	if !hasPCRPID {
		// Keep the previous PMT rather than emitting an invalid one.
		m.stats.PSIErrors++
		return nil
	}

	// Build the remap for this generation of the PMT. Existing output PID
	// assignments are kept so that continuity counters stay associated.
	newRemap := make(map[uint16]uint16, len(pmt.ElementaryStreams)+1)
	newUsed := make(map[uint16]uint16, len(pmt.ElementaryStreams)+1)

	// PMT PID first (PAT must advertise the remapped value).
	pmtOut, err := m.allocateOutputPID(m.pmtPID, newRemap, newUsed)
	if err != nil {
		return err
	}

	for _, es := range pmt.ElementaryStreams {
		if _, err := m.allocateOutputPID(es.ElementaryPID, newRemap, newUsed); err != nil {
			return err
		}
	}

	// Drop CC state of output PIDs that are no longer part of the program.
	activeOutput := map[uint16]bool{PIDPAT: true, pmtOut: true}
	for _, out := range newRemap {
		activeOutput[out] = true
	}
	for outPID := range m.cc {
		if !activeOutput[outPID] {
			delete(m.cc, outPID)
		}
	}
	m.remap = newRemap
	m.usedOutput = newUsed

	// Deep-copy the PMT and translate its PIDs.
	newPMT := &PMTData{
		ProgramNumber:      pmt.ProgramNumber,
		ProgramDescriptors: pmt.ProgramDescriptors,
		ElementaryStreams:  make([]*PMTElementaryStream, 0, len(pmt.ElementaryStreams)),
	}
	for _, es := range pmt.ElementaryStreams {
		newPMT.ElementaryStreams = append(newPMT.ElementaryStreams, &PMTElementaryStream{
			ElementaryPID:               newRemap[es.ElementaryPID],
			StreamType:                  es.StreamType,
			ElementaryStreamDescriptors: es.ElementaryStreamDescriptors,
		})
	}
	// Translate the PCR PID, preserving the "no PCR" sentinel.
	if pmt.PCRPID == PIDNull {
		newPMT.PCRPID = PIDNull
	} else {
		newPMT.PCRPID = newRemap[pmt.PCRPID]
	}

	firstPMT := !m.pmtKnown
	m.currentPMT = newPMT
	m.pmtKnown = true
	m.stats.PMTUpdates++

	// A new PMT generation always changes the PMT content. The PAT also has to
	// be regenerated on the first PMT so it references the allocated PMT PID.
	m.pmtDirty = true
	if firstPMT {
		m.patDirty = true
	}

	if err := m.regeneratePMT(); err != nil {
		return err
	}
	if firstPMT {
		// The very first table emission includes a fresh PAT as well.
		if err := m.regeneratePAT(); err != nil {
			return err
		}
	}
	return m.emitTables()
}

// allocateOutputPID reserves an output PID for inputPID, honoring explicit
// remaps and the auto-remap option, and records it in remap/used.
func (m *ProgramRemuxer) allocateOutputPID(inputPID uint16, remap, used map[uint16]uint16) (out uint16, err error) {
	if existing, ok := remap[inputPID]; ok {
		return existing, nil
	}
	// Preserve assignments from previous PMT generations.
	if prev, ok := m.remap[inputPID]; ok {
		if owner, taken := used[prev]; taken && owner != inputPID {
			return 0, fmt.Errorf("astits: input PID %d: %w (output PID %d already used by %d)", inputPID, ErrPIDRemapConflict, prev, owner)
		}
		remap[inputPID] = prev
		used[prev] = inputPID
		return prev, nil
	}

	if target, explicit := m.explicitRemap[inputPID]; explicit {
		out = target
	} else if m.autoRemap && inputPID != m.pmtPID {
		out = m.nextAutoPID()
	} else {
		out = inputPID
	}

	if err = validateOutputPID(out); err != nil {
		return 0, fmt.Errorf("astits: input PID %d: %w", inputPID, err)
	}
	if owner, taken := used[out]; taken && owner != inputPID {
		return 0, fmt.Errorf("astits: input PID %d: %w (output PID %d already used by %d)", inputPID, ErrPIDRemapConflict, out, owner)
	}
	remap[inputPID] = out
	used[out] = inputPID
	return out, nil
}

// nextAutoPID returns the next free user PID in auto-remap mode, skipping
// reserved PIDs and PIDs already taken by explicit remaps or prior allocations.
func (m *ProgramRemuxer) nextAutoPID() uint16 {
	for {
		candidate := m.nextAuto
		m.nextAuto++
		if m.nextAuto >= PIDNull {
			m.nextAuto = startPID
		}
		if validateOutputPID(candidate) != nil {
			continue
		}
		if _, taken := m.usedOutput[candidate]; taken {
			continue
		}
		// Reject a candidate that an explicit remap will want later.
		blocked := false
		for _, target := range m.explicitRemap {
			if target == candidate {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		return candidate
	}
}

// validateOutputPID rejects reserved and out-of-range PIDs.
func validateOutputPID(pid uint16) error {
	switch {
	case pid == PIDPAT, pid == PIDCAT, pid == PIDTSDT:
		return ErrPIDRemapInvalid
	case pid >= 0x0003 && pid <= 0x000f:
		return ErrPIDRemapInvalid
	case pid == PIDNull:
		return ErrPIDRemapInvalid
	}
	return nil
}

// handleElementary forwards packets of PIDs declared by the current PMT,
// rewriting their PID and continuity counter.
func (m *ProgramRemuxer) handleElementary(p *Packet) error {
	// Before the first valid PMT we cannot tell target PIDs from foreign ones.
	if !m.pmtKnown {
		m.stats.PacketsDropped++
		return nil
	}

	outPID, ok := m.remap[p.Header.PID]
	if !ok {
		// Foreign program, unrelated SI PID or unknown stream.
		m.stats.PacketsDropped++
		return nil
	}

	// Periodic table retransmission, like a regular muxer.
	m.tableCounter++
	if m.tableCounter >= m.tablesPeriod {
		m.tableCounter = 0
		if err := m.emitTables(); err != nil {
			return err
		}
	}

	// Adaptation-field-only packets don't increment the continuity counter;
	// every packet carrying a payload does. Duplicate packets (same CC, payload
	// present) keep the previous counter value.
	cc, ok := m.cc[outPID]
	if !ok {
		cc = newWrappingCounter(0b1111)
	}
	var newCC uint8
	if p.Header.HasPayload {
		newCC = uint8(cc.inc())
	} else {
		newCC = uint8(cc.get())
	}
	m.cc[outPID] = cc

	out := *p // shallow copy: payload and adaptation field are reused as-is
	out.Header.PID = outPID
	out.Header.ContinuityCounter = newCC

	if err := m.writePacket(&out); err != nil {
		return err
	}
	m.stats.PacketsWritten++
	return nil
}

// emitTables writes the cached PAT/PMT packets, bumping their CC.
func (m *ProgramRemuxer) emitTables() error {
	if m.patPacket == nil || m.pmtPacket == nil {
		return nil
	}
	patCC := uint8(m.patCC.inc())
	pmtCC := uint8(m.pmtCC.inc())

	pat := make([]byte, MpegTsPacketSize)
	copy(pat, m.patPacket)
	pat[3] = (pat[3] & 0xf0) | patCC

	pmt := make([]byte, MpegTsPacketSize)
	copy(pmt, m.pmtPacket)
	pmt[3] = (pmt[3] & 0xf0) | pmtCC

	if _, err := m.w.Write(pat); err != nil {
		return fmt.Errorf("astits: writing PAT packet failed: %w", err)
	}
	m.stats.PacketsWritten++
	m.stats.TablesWritten++
	if _, err := m.w.Write(pmt); err != nil {
		return fmt.Errorf("astits: writing PMT packet failed: %w", err)
	}
	m.stats.PacketsWritten++
	m.stats.TablesWritten++
	return nil
}

// regeneratePAT builds the single-program PAT packet and caches it. The
// version number is bumped only when the PAT content changed since the last
// generation (or on the very first generation). It is a no-op until the first
// PMT has been applied, since the advertised PMT PID is only final then.
func (m *ProgramRemuxer) regeneratePAT() error {
	if !m.programKnown || !m.pmtKnown {
		return nil
	}
	if m.patPacket == nil || m.patDirty {
		m.patVer.inc()
	}
	version := uint8(m.patVer.get())

	pat := &PATData{
		TransportStreamID: m.patTSID,
		Programs: []*PATProgram{{
			ProgramNumber: m.programNumber,
			ProgramMapID:  m.remap[m.pmtPID],
		}},
	}
	if pat.Programs[0].ProgramMapID == 0 {
		// PMT remap not allocated yet (first PAT before first PMT): keep the
		// advertised PMT PID; the PAT is regenerated again from applyPMT.
		pat.Programs[0].ProgramMapID = m.pmtPID
	}

	b, err := m.buildPSIPacket(PIDPAT, &PSISection{
		Header: &PSISectionHeader{
			TableID:                PSITableIDPAT,
			SectionSyntaxIndicator: true,
			SectionLength:          calcPATSectionLength(pat),
		},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{
				TableIDExtension:     pat.TransportStreamID,
				VersionNumber:        version,
				CurrentNextIndicator: true,
			},
			Data: &PSISectionSyntaxData{PAT: pat},
		},
	})
	if err != nil {
		return err
	}
	m.patPacket = b
	m.patDirty = false
	return nil
}

// regeneratePMT builds the target-program PMT packet and caches it. The
// version number is bumped only when the PMT content changed since the last
// generation (or on the very first generation).
func (m *ProgramRemuxer) regeneratePMT() error {
	if m.currentPMT == nil {
		return nil
	}
	if m.pmtPacket == nil || m.pmtDirty {
		m.pmtVer.inc()
	}
	version := uint8(m.pmtVer.get())

	b, err := m.buildPSIPacket(m.remap[m.pmtPID], &PSISection{
		Header: &PSISectionHeader{
			TableID:                PSITableIDPMT,
			SectionSyntaxIndicator: true,
			SectionLength:          calcPMTSectionLength(m.currentPMT),
		},
		Syntax: &PSISectionSyntax{
			Header: &PSISectionSyntaxHeader{
				TableIDExtension:     m.currentPMT.ProgramNumber,
				VersionNumber:        version,
				CurrentNextIndicator: true,
			},
			Data: &PSISectionSyntaxData{PMT: m.currentPMT},
		},
	})
	if err != nil {
		return err
	}
	m.pmtPacket = b
	m.pmtDirty = false
	return nil
}

// buildPSIPacket serializes a single-section PSI table into one 188-byte packet.
func (m *ProgramRemuxer) buildPSIPacket(pid uint16, section *PSISection) ([]byte, error) {
	var payload bytes.Buffer
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &payload})
	psi := &PSIData{Sections: []*PSISection{section}}
	if _, err := writePSIData(w, psi); err != nil {
		return nil, fmt.Errorf("astits: serializing PSI table failed: %w", err)
	}

	// A section plus pointer field must fit in one packet payload.
	if payload.Len() > MpegTsPacketSize-mpegTsPacketHeaderSize {
		return nil, fmt.Errorf("astits: table on PID %d: %w", pid, ErrTableTooLarge)
	}

	var out bytes.Buffer
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &out})
	pkt := &Packet{
		Header: PacketHeader{
			PID:                       pid,
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			// CC placeholder (low nibble 0); emitTables overwrites it.
		},
		Payload: payload.Bytes(),
	}
	if _, err := writePacket(pw, pkt, MpegTsPacketSize); err != nil {
		return nil, fmt.Errorf("astits: writing PSI packet failed: %w", err)
	}
	return out.Bytes(), nil
}

// writePacket writes one (possibly non-188, e.g. 192-byte input is normalized
// to 188 on output) packet.
func (m *ProgramRemuxer) writePacket(p *Packet) error {
	bw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m.w})
	if _, err := writePacket(bw, p, MpegTsPacketSize); err != nil {
		return fmt.Errorf("astits: writing packet failed: %w", err)
	}
	return nil
}

// parsePAT reassembles and parses a PAT payload from a set of packets.
func (m *ProgramRemuxer) parsePAT(ps []*Packet) (*PATData, error) {
	payload := m.assemblePayload(ps)
	i := astikit.NewBytesIterator(payload)
	psi, err := parsePSIData(i)
	if err != nil {
		return nil, err
	}
	for _, s := range psi.Sections {
		if s.Header != nil && s.Header.TableID == PSITableIDPAT &&
			s.Syntax != nil && s.Syntax.Data != nil && s.Syntax.Data.PAT != nil {
			return s.Syntax.Data.PAT, nil
		}
	}
	return nil, errors.New("astits: no PAT section found")
}

// parsePMT reassembles and parses a PMT payload, returning the table and its
// version number.
func (m *ProgramRemuxer) parsePMT(ps []*Packet) (*PMTData, uint8, error) {
	payload := m.assemblePayload(ps)
	i := astikit.NewBytesIterator(payload)
	psi, err := parsePSIData(i)
	if err != nil {
		return nil, 0, err
	}
	for _, s := range psi.Sections {
		if s.Header != nil && s.Header.TableID == PSITableIDPMT &&
			s.Syntax != nil && s.Syntax.Data != nil && s.Syntax.Data.PMT != nil {
			return s.Syntax.Data.PMT, s.Syntax.Header.VersionNumber, nil
		}
	}
	return nil, 0, errors.New("astits: no PMT section found")
}

// assemblePayload concatenates the payloads of a reassembled set of packets.
func (m *ProgramRemuxer) assemblePayload(ps []*Packet) []byte {
	var l int
	for _, p := range ps {
		l += len(p.Payload)
	}
	out := make([]byte, 0, l)
	for _, p := range ps {
		out = append(out, p.Payload...)
	}
	return out
}
