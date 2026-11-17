package astits

import (
	"time"

	"github.com/asticode/go-astikit"
)

// PCR specifics
const (
	// pcrBaseModulo is the modulo of the 33 bits PCR base, in 90 kHz ticks
	pcrBaseModulo = int64(1) << 33
	// pcrMaxExtension is the maximum valid PCR extension value. The extension
	// counts 27 MHz ticks within one 90 kHz tick, hence 0 to 299
	pcrMaxExtension           = 299
	pcrDefaultJitterTolerance = 40 * time.Millisecond
)

// PCRData represents a PCR sample extracted from a transport stream
type PCRData struct {
	PCR           ClockReference // PCR value as carried in the packet
	Timestamp     time.Duration  // Unwrapped timestamp: the 33 bits PCR base wrap is resolved and the timeline is rebased at each discontinuity so that it never moves backward across segments
	ProgramNumber uint16         // Program the PCR PID belongs to. 0 when the PID was provided manually and no PMT has declared it
	PID           uint16         // PID carrying the PCR
	PacketIndex   uint64         // Index of the packet in the processed packet sequence, starting at 0
	Discontinuity bool           // True when this sample starts a new clock segment (PCR PID switch, discontinuity indicator, invalid PCR dropped before it or backward jump beyond the jitter tolerance)
}

// PCRExtractor extracts PCR samples per program out of a Demuxer packet sequence.
// Feed it every packet returned by Demuxer.NextPacket and collect the returned samples.
//
// Rules:
//   - Tracked PIDs are the PCR PIDs declared by PMTs (discovered through the PAT)
//     plus the PIDs provided with PCRExtractorOptPCRPIDs. Before any PMT is
//     received, only manually provided PIDs produce samples, with ProgramNumber 0.
//   - Programs whose PMT declares PCRPID 0x1FFF have no PCR and are ignored.
//   - The 33 bits PCR base wrap is resolved: forward modular deltas extend an
//     unwrapped 64 bits base used to compute PCRData.Timestamp.
//   - A backward jump smaller than the jitter tolerance is emitted as is (the
//     timestamp may move slightly backward). A backward jump greater than the
//     jitter tolerance is a discontinuity: the timeline is rebased so that the
//     sample timestamp equals the previous one, and PCRData.Discontinuity is set.
//   - A PCR with an extension greater than 299 is invalid: the sample is dropped
//     and the track is reset, the next valid sample is flagged as a discontinuity.
//   - A packet carrying the discontinuity indicator rebases the timeline at this
//     sample and sets PCRData.Discontinuity.
//   - When a PAT/PMT changes the PCR PID of a program, the track switches to the
//     new PID: the first sample on the new PID is flagged as a discontinuity and
//     the timeline carries over from the previous PID. The old PID is not tracked
//     for this program anymore.
//   - Programs are tracked independently. If several programs declare the same
//     PCR PID, samples are attributed to the first program that declared it.
type PCRExtractor struct {
	optJitterTolerance int64 // In 90 kHz ticks
	optPCRPIDs         map[uint16]bool

	packetIndex uint64
	pmtPIDs     map[uint16]uint16 // PMT PID -> program number
	programs    map[uint16]uint16 // program number -> PCR PID
	pcrPrograms map[uint16]uint16 // PCR PID -> program number
	tracks      map[uint16]*pcrTrack
	psiBuffers  map[uint16][]*Packet
}

// pcrTrack represents the unwrapping state of one PCR PID
type pcrTrack struct {
	lastRawBase          int64 // Raw 33 bits base of the last valid PCR
	lastUnwrappedBase    int64 // Unwrapped base of the last emitted sample, in 90 kHz ticks
	initialized          bool
	pendingDiscontinuity bool
}

// NewPCRExtractor creates a new PCR extractor
func NewPCRExtractor(opts ...func(*PCRExtractor)) (e *PCRExtractor) {
	e = &PCRExtractor{
		optJitterTolerance: pcrDefaultJitterTolerance.Nanoseconds() * 90000 / 1e9,
		optPCRPIDs:         make(map[uint16]bool),
		pmtPIDs:            make(map[uint16]uint16),
		programs:           make(map[uint16]uint16),
		pcrPrograms:        make(map[uint16]uint16),
		tracks:             make(map[uint16]*pcrTrack),
		psiBuffers:         make(map[uint16][]*Packet),
	}
	for _, opt := range opts {
		opt(e)
	}
	return
}

// PCRExtractorOptPCRPIDs returns the option to track PCR PIDs manually, regardless of PMTs
func PCRExtractorOptPCRPIDs(pids ...uint16) func(*PCRExtractor) {
	return func(e *PCRExtractor) {
		for _, pid := range pids {
			e.optPCRPIDs[pid] = true
		}
	}
}

// PCRExtractorOptJitterTolerance returns the option to set the maximum backward
// jump tolerated before a track is considered discontinuous
func PCRExtractorOptJitterTolerance(d time.Duration) func(*PCRExtractor) {
	return func(e *PCRExtractor) {
		e.optJitterTolerance = d.Nanoseconds() * 90000 / 1e9
	}
}

// ProcessPacket processes a packet of the demuxer packet sequence and returns
// the PCR sample it carries, if any
func (e *PCRExtractor) ProcessPacket(p *Packet) (d *PCRData) {
	// Packet index
	idx := e.packetIndex
	e.packetIndex++

	// Track PAT/PMT to discover PCR PIDs
	if p.Header.HasPayload && (p.Header.PID == PIDPAT || e.isPMTPID(p.Header.PID)) {
		e.processPSIPacket(p)
	}

	// PCR
	if p.AdaptationField != nil && p.AdaptationField.HasPCR && p.AdaptationField.PCR != nil && e.isPCRPID(p.Header.PID) {
		d = e.processPCR(p, idx)
	}
	return
}

// isPMTPID checks whether the PID carries a PMT
func (e *PCRExtractor) isPMTPID(pid uint16) bool {
	_, ok := e.pmtPIDs[pid]
	return ok
}

// isPCRPID checks whether the PID is a tracked PCR PID
func (e *PCRExtractor) isPCRPID(pid uint16) bool {
	if e.optPCRPIDs[pid] {
		return true
	}
	_, ok := e.pcrPrograms[pid]
	return ok
}

// processPSIPacket reassembles and parses PAT/PMT sections
func (e *PCRExtractor) processPSIPacket(p *Packet) {
	// A payload unit start indicator means a new section starts: drop any
	// incomplete previous data
	pid := p.Header.PID
	buf := e.psiBuffers[pid]
	if p.Header.PayloadUnitStartIndicator {
		buf = nil
	}
	buf = append(buf, p)
	e.psiBuffers[pid] = buf

	// Wait until the section is complete
	if !isPSIComplete(buf) {
		return
	}
	delete(e.psiBuffers, pid)

	// Assemble payload
	var payload []byte
	for _, pkt := range buf {
		payload = append(payload, pkt.Payload...)
	}

	// Parse PSI data. Malformed sections are ignored: they must not break the
	// PCR tracking of the rest of the stream
	d, err := parsePSIData(astikit.NewBytesIterator(payload))
	if err != nil {
		return
	}
	for _, s := range d.Sections {
		if s.Syntax == nil || s.Syntax.Data == nil {
			continue
		}
		if s.Syntax.Data.PAT != nil {
			e.processPAT(s.Syntax.Data.PAT)
		}
		if s.Syntax.Data.PMT != nil {
			e.processPMT(s.Syntax.Data.PMT)
		}
	}
}

// processPAT updates the PMT PIDs map
func (e *PCRExtractor) processPAT(d *PATData) {
	pmtPIDs := make(map[uint16]uint16)
	for _, pgm := range d.Programs {
		// Program number 0 is reserved to NIT
		if pgm.ProgramNumber == 0 {
			continue
		}
		pmtPIDs[pgm.ProgramMapID] = pgm.ProgramNumber
	}
	e.pmtPIDs = pmtPIDs
}

// processPMT updates the PCR PID of a program
func (e *PCRExtractor) processPMT(d *PMTData) {
	oldPID, tracked := e.programs[d.ProgramNumber]

	// PCRPID 0x1FFF means the program has no PCR
	if d.PCRPID == PIDNull {
		if tracked {
			e.untrackPCRPID(oldPID, d.ProgramNumber)
			delete(e.programs, d.ProgramNumber)
		}
		return
	}

	// No change
	if tracked && oldPID == d.PCRPID {
		return
	}

	// PCR PID switch: carry the timeline over to the new PID and flag a
	// discontinuity on its first sample
	if tracked {
		if old, ok := e.tracks[oldPID]; ok && old.initialized {
			e.tracks[d.PCRPID] = &pcrTrack{
				initialized:          true,
				lastUnwrappedBase:    old.lastUnwrappedBase,
				pendingDiscontinuity: true,
			}
		}
		e.untrackPCRPID(oldPID, d.ProgramNumber)
	}
	e.programs[d.ProgramNumber] = d.PCRPID
	if _, ok := e.pcrPrograms[d.PCRPID]; !ok {
		e.pcrPrograms[d.PCRPID] = d.ProgramNumber
	}
}

// untrackPCRPID removes the program attribution of a PCR PID and drops its
// track if nothing references it anymore
func (e *PCRExtractor) untrackPCRPID(pid, program uint16) {
	if p, ok := e.pcrPrograms[pid]; ok && p == program {
		delete(e.pcrPrograms, pid)
	}
	if _, ok := e.pcrPrograms[pid]; !ok && !e.optPCRPIDs[pid] {
		delete(e.tracks, pid)
	}
}

// processPCR processes a PCR carried by a tracked PID
func (e *PCRExtractor) processPCR(p *Packet, idx uint64) *PCRData {
	pid := p.Header.PID
	pcr := p.AdaptationField.PCR
	t := e.tracks[pid]
	if t == nil {
		t = &pcrTrack{}
		e.tracks[pid] = t
	}

	// Invalid extension: drop the sample and reset the track
	if pcr.Extension > pcrMaxExtension {
		if t.initialized {
			t.pendingDiscontinuity = true
		}
		return nil
	}

	base := pcr.Base
	var unwrapped int64
	var discontinuity bool

	switch {
	case !t.initialized:
		// First sample of the track
		unwrapped = base
		t.initialized = true
	case t.pendingDiscontinuity || p.AdaptationField.DiscontinuityIndicator:
		// Discontinuity: rebase the timeline so that it stays continuous
		unwrapped = t.lastUnwrappedBase
		discontinuity = true
	default:
		delta := (base - t.lastRawBase) & (pcrBaseModulo - 1)
		if delta < pcrBaseModulo/2 {
			// Forward move, resolves the base wrap
			unwrapped = t.lastUnwrappedBase + delta
		} else if back := pcrBaseModulo - delta; back <= e.optJitterTolerance {
			// Small backward jump: jitter, emit as is
			unwrapped = t.lastUnwrappedBase - back
		} else {
			// Large backward jump: discontinuity, rebase the timeline
			unwrapped = t.lastUnwrappedBase
			discontinuity = true
		}
	}

	t.lastRawBase = base
	t.lastUnwrappedBase = unwrapped
	t.pendingDiscontinuity = false

	return &PCRData{
		PCR:           *pcr,
		Timestamp:     ClockReference{Base: unwrapped, Extension: pcr.Extension}.Duration(),
		ProgramNumber: e.pcrPrograms[pid],
		PID:           pid,
		PacketIndex:   idx,
		Discontinuity: discontinuity,
	}
}
