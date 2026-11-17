package astits

import (
	"sort"
)

// packetAccumulator keeps track of packets for a single PID and decides when to flush them
type packetAccumulator struct {
	pid             uint16
	programMap      *programMap
	onDiscontinuity func(pid uint16)
	q               []*Packet

	// lastCC / hasLastCC remember the continuity counter of the most recent
	// packet even after the queue has been flushed, so a break occurring at a
	// payload unit start following a flushed payload is still detectable.
	lastCC    uint8
	hasLastCC bool
}

// newPacketAccumulator creates a new packet queue for a single PID
func newPacketAccumulator(pid uint16, programMap *programMap, onDiscontinuity func(pid uint16)) *packetAccumulator {
	return &packetAccumulator{
		pid:             pid,
		programMap:      programMap,
		onDiscontinuity: onDiscontinuity,
	}
}

// add adds a new packet for this PID to the queue
func (b *packetAccumulator) add(p *Packet) (ps []*Packet) {
	mps := b.q

	// Empty buffer if we detect a discontinuity. When the queue is empty
	// (the previous payload was already flushed) fall back to the last seen
	// continuity counter so a break occurring at a payload unit start is still
	// detectable. This extended check only applies to non-PSI data PIDs: PSI
	// sections (PAT/PMT/...) are legitimately retransmitted with a continuity
	// counter restarting at zero, which must not be reported as a break.
	discontinuous := hasDiscontinuity(mps, p)
	if !discontinuous && len(mps) == 0 && b.hasLastCC && p.Header.PID != PIDNull &&
		!b.isPSIPID(p.Header.PID) {
		var expectedCC uint8
		if p.Header.HasPayload {
			expectedCC = (b.lastCC + 1) & 0x0f
		} else {
			expectedCC = b.lastCC
		}
		// An adaptation-field discontinuity indicator explicitly marks the
		// counter as intentionally reset, which is not a transport error.
		intentional := p.Header.HasAdaptationField && p.AdaptationField != nil &&
			p.AdaptationField.DiscontinuityIndicator
		discontinuous = expectedCC != p.Header.ContinuityCounter && !intentional
	}
	if discontinuous {
		if b.onDiscontinuity != nil {
			b.onDiscontinuity(b.pid)
		}

		// Reset current slice or make new
		if cap(mps) > 0 {
			mps = mps[:0]
		} else {
			mps = make([]*Packet, 0, 10)
		}
	}

	// Throw away packet if it's the same as the previous one
	if isSameAsPrevious(mps, p) {
		return
	}

	// Flush buffer if new payload starts here
	if p.Header.PayloadUnitStartIndicator {
		ps = mps
		mps = make([]*Packet, 0, cap(mps))
	}

	mps = append(mps, p)
	b.lastCC = p.Header.ContinuityCounter
	b.hasLastCC = true

	// A single call can only return one payload: if this packet already flushed the
	// previous one, keep the new payload queued instead of overwriting the result.
	if len(ps) > 0 {
		b.q = mps
		return
	}

	// Check if PSI payload is complete
	if b.programMap != nil &&
		(b.pid == PIDPAT || b.programMap.existsUnlocked(b.pid)) &&
		isPSIComplete(mps) {
		ps = mps
		mps = nil
	} else if isPESPayload(mps[0].Payload) && isPESComplete(mps) { // Check if PES payload is complete
		ps = mps
		mps = nil
	}

	b.q = mps
	return
}

// isPSIPID reports whether the PID carries PSI/SI tables rather than a PES
// elementary stream. It mirrors the PID classification used when parsing data
// but only relies on the program map available to the accumulator.
func (b *packetAccumulator) isPSIPID(pid uint16) bool {
	if pid == PIDPAT {
		return true
	}
	if b.programMap != nil && b.programMap.existsUnlocked(pid) {
		return true
	}
	// DVB reserved SI table PIDs (NIT, SDT, EIT, TDT/TOT, ...).
	if (pid >= 0x10 && pid <= 0x14) || (pid >= 0x1e && pid <= 0x1f) {
		return true
	}
	return false
}

// packetPool represents a queue of packets for each PID in the stream
type packetPool struct {
	// We use map[uint32] instead map[uint16] as go runtime provide optimized hash functions for (u)int32/64 keys
	b map[uint32]*packetAccumulator // Indexed by PID

	programMap      *programMap
	onDiscontinuity func(pid uint16)
}

// newPacketPool creates a new packet pool with an optional parser and programMap
func newPacketPool(programMap *programMap, onDiscontinuity ...func(pid uint16)) *packetPool {
	pool := &packetPool{
		b: make(map[uint32]*packetAccumulator),

		programMap: programMap,
	}
	if len(onDiscontinuity) > 0 {
		pool.onDiscontinuity = onDiscontinuity[0]
	}
	return pool
}

// addUnlocked adds a new packet to the pool
func (b *packetPool) addUnlocked(p *Packet) (ps []*Packet) {
	// Throw away packet if error indicator
	if p.Header.TransportErrorIndicator {
		return
	}

	// Throw away packets that don't have a payload until we figure out what we're going to do with them
	// TODO figure out what we're going to do with them :D
	if !p.Header.HasPayload {
		return
	}

	// Make sure accumulator exists
	acc, ok := b.b[uint32(p.Header.PID)]
	if !ok {
		acc = newPacketAccumulator(p.Header.PID, b.programMap, b.onDiscontinuity)
		b.b[uint32(p.Header.PID)] = acc
	}

	// Add to the accumulator
	return acc.add(p)
}

// dumpUnlocked dumps the packet pool by looking for the first item with packets inside
func (b *packetPool) dumpUnlocked() (ps []*Packet) {
	var keys []int
	for k := range b.b {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		ps = b.b[uint32(k)].q
		delete(b.b, uint32(k))
		if len(ps) > 0 {
			return
		}
	}
	return
}

// hasDiscontinuity checks whether a packet is discontinuous with a set of packets
func hasDiscontinuity(ps []*Packet, p *Packet) bool {
	if p.Header.PID == PIDNull {
		return false
	}

	isDiscontinuityIndicator := p.Header.HasAdaptationField && p.AdaptationField.DiscontinuityIndicator
	if isDiscontinuityIndicator {
		return false
	}

	l := len(ps)
	if l == 0 {
		return false
	}

	var expectedContinuityCounter uint8
	if p.Header.HasPayload {
		expectedContinuityCounter = (ps[l-1].Header.ContinuityCounter + 1) & 0x0f
	} else {
		expectedContinuityCounter = ps[l-1].Header.ContinuityCounter
	}
	if expectedContinuityCounter == p.Header.ContinuityCounter {
		return false
	}

	return true
}

// isSameAsPrevious checks whether a packet is the same as the last packet of a set of packets
func isSameAsPrevious(ps []*Packet, p *Packet) bool {
	l := len(ps)
	return l > 0 && p.Header.HasPayload && p.Header.ContinuityCounter == ps[l-1].Header.ContinuityCounter
}
