package astits

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astikit"
)

// Diagnostic event types reported by ContinuityDiagnoser.
const (
	// DiagnosticEventTypeCCFirstSeen is reported the first time a PID is observed.
	// It carries no error: it documents where the per-PID baseline starts.
	DiagnosticEventTypeCCFirstSeen DiagnosticEventType = "cc_first_seen"
	// DiagnosticEventTypeCCGap is reported when the continuity counter jumps forward,
	// meaning one or more packets of this PID were lost. MissingCount is the number
	// of missing packets (between 1 and 15).
	DiagnosticEventTypeCCGap DiagnosticEventType = "cc_gap"
	// DiagnosticEventTypeCCDuplicate is reported when a payload packet repeats the
	// previous continuity counter with an identical payload. This is a legal MPEG-TS
	// retransmission and is not reported as a gap.
	DiagnosticEventTypeCCDuplicate DiagnosticEventType = "cc_duplicate"
	// DiagnosticEventTypeCCError is reported when the continuity counter does not
	// follow the expected sequence but is not a legal duplicate either, e.g. a
	// repeated counter with a different payload, or an adaptation-only packet whose
	// counter should have stayed constant but did not.
	DiagnosticEventTypeCCError DiagnosticEventType = "cc_error"
	// DiagnosticEventTypeCCMalformed is reported when the 2-bit adaptation field
	// control of the header is the reserved value 0b00 (neither adaptation field nor
	// payload).
	DiagnosticEventTypeCCMalformed DiagnosticEventType = "cc_malformed"
	// DiagnosticEventTypeDiscontinuity is reported when the adaptation field carries
	// a discontinuity_indicator. Both the continuity counter and the timestamps of
	// the PID are re-based at this packet, so no gap or timestamp jump is reported
	// for it.
	DiagnosticEventTypeDiscontinuity DiagnosticEventType = "declared_discontinuity"
	// DiagnosticEventTypeTransportError is reported when the transport_error_indicator
	// bit is set: the packet is corrupt. Continuity state is still advanced using its
	// header so that following packets do not produce cascading false gaps, but its
	// payload and timestamps are not analyzed.
	DiagnosticEventTypeTransportError DiagnosticEventType = "transport_error"
	// DiagnosticEventTypePCRJump is reported when two consecutive PCRs of a PID do
	// not match within the configured tolerances. Delta is in 27 MHz ticks and is
	// negative for a backward jump.
	DiagnosticEventTypePCRJump DiagnosticEventType = "pcr_jump"
	// DiagnosticEventTypePTSJump is reported when two consecutive PES PTS values of a
	// PID do not match within the configured tolerances. Delta is in 90 kHz units and
	// is negative for a backward jump.
	DiagnosticEventTypePTSJump DiagnosticEventType = "pts_jump"
	// DiagnosticEventTypeDTSJump is reported when two consecutive PES DTS values of a
	// PID do not match within the configured tolerances. Delta is in 90 kHz units and
	// is negative for a backward jump.
	DiagnosticEventTypeDTSJump DiagnosticEventType = "dts_jump"
	// DiagnosticEventTypeTimestampWrap is informational: the raw 33-bit timestamp went
	// backwards modulo its modulus, which is a legal wraparound. A forward jump caused
	// by the same wraparound can still be reported alongside it.
	DiagnosticEventTypeTimestampWrap DiagnosticEventType = "timestamp_wrap"
	// DiagnosticEventTypeMissingPTS is reported when a PES packet starts on the PID but
	// its optional header carries no PTS.
	DiagnosticEventTypeMissingPTS DiagnosticEventType = "missing_pts"
	// DiagnosticEventTypePESMalformed is reported when a payload announced by the PES
	// start code prefix (0x000001) cannot be parsed as a PES packet.
	DiagnosticEventTypePESMalformed DiagnosticEventType = "pes_malformed"
)

// Diagnostic severities.
const (
	diagnosticSeverityInfo    = "info"
	diagnosticSeverityWarning = "warning"
	diagnosticSeverityError   = "error"
)

// ErrInvalidPacketSize is returned when the configured packet size is neither 188 nor 192.
var ErrInvalidPacketSize = errors.New("astits: packet size must be 188 or 192")

// DiagnosticEventType represents the type of a diagnostic event.
type DiagnosticEventType string

// Severity returns the severity of the event type.
func (t DiagnosticEventType) Severity() string {
	switch t {
	case DiagnosticEventTypeCCGap,
		DiagnosticEventTypeCCError,
		DiagnosticEventTypeCCMalformed,
		DiagnosticEventTypeTransportError,
		DiagnosticEventTypePCRJump,
		DiagnosticEventTypePTSJump,
		DiagnosticEventTypeDTSJump,
		DiagnosticEventTypePESMalformed:
		return diagnosticSeverityError
	case DiagnosticEventTypeMissingPTS:
		return diagnosticSeverityWarning
	default:
		return diagnosticSeverityInfo
	}
}

// DiagnosticEvent represents a single continuity or timestamp observation made on a PID.
// All positions are zero-based: PacketIndex counts every packet handed to the diagnoser
// (including null packets) and ByteOffset is its offset in the underlying byte stream.
type DiagnosticEvent struct {
	// Type is the kind of event, see the DiagnosticEventType constants.
	Type DiagnosticEventType
	// PID is the transport stream PID the event was observed on.
	PID uint16
	// PacketIndex is the zero-based index of the packet in the consumed packet stream.
	PacketIndex int
	// ByteOffset is the zero-based byte offset at which the packet starts. It equals
	// PacketIndex multiplied by the configured packet size (188 or 192).
	ByteOffset int
	// ContinuityCounter is the 4-bit continuity counter carried by the current packet.
	ContinuityCounter uint8
	// PreviousContinuityCounter is the continuity counter of the previous packet of
	// the same PID. It is meaningless for first-seen events.
	PreviousContinuityCounter uint8
	// MissingCount is the number of missing packets for a cc_gap event (1 to 15).
	MissingCount int
	// Timestamp identifies which timestamp an event refers to: "pcr", "pts" or "dts".
	Timestamp string
	// Previous and Current are the raw (modulo-range) timestamp values. PCR is expressed
	// in 27 MHz ticks, PTS/DTS in 90 kHz units.
	Previous, Current int64
	// Delta is the wraparound-aware difference Current - Previous. It is negative for a
	// backward jump.
	Delta int64
}

// String turns the event into a human-readable, single-line description.
func (e DiagnosticEvent) String() string {
	switch e.Type {
	case DiagnosticEventTypeCCFirstSeen:
		return fmt.Sprintf("astits: pid %d first seen at packet %d (offset %d), cc=%d",
			e.PID, e.PacketIndex, e.ByteOffset, e.ContinuityCounter)
	case DiagnosticEventTypeCCGap:
		return fmt.Sprintf("astits: pid %d continuity gap at packet %d (offset %d): cc %d -> %d, %d packet(s) missing",
			e.PID, e.PacketIndex, e.ByteOffset, e.PreviousContinuityCounter, e.ContinuityCounter, e.MissingCount)
	case DiagnosticEventTypeCCDuplicate:
		return fmt.Sprintf("astits: pid %d legal retransmission at packet %d (offset %d), cc=%d",
			e.PID, e.PacketIndex, e.ByteOffset, e.ContinuityCounter)
	case DiagnosticEventTypeCCError:
		return fmt.Sprintf("astits: pid %d continuity error at packet %d (offset %d): cc %d -> %d",
			e.PID, e.PacketIndex, e.ByteOffset, e.PreviousContinuityCounter, e.ContinuityCounter)
	case DiagnosticEventTypeCCMalformed:
		return fmt.Sprintf("astits: pid %d malformed packet at packet %d (offset %d): reserved adaptation field control 0b00",
			e.PID, e.PacketIndex, e.ByteOffset)
	case DiagnosticEventTypeDiscontinuity:
		return fmt.Sprintf("astits: pid %d declared discontinuity at packet %d (offset %d), cc=%d",
			e.PID, e.PacketIndex, e.ByteOffset, e.ContinuityCounter)
	case DiagnosticEventTypeTransportError:
		return fmt.Sprintf("astits: pid %d corrupt packet (transport_error_indicator) at packet %d (offset %d), cc=%d",
			e.PID, e.PacketIndex, e.ByteOffset, e.ContinuityCounter)
	case DiagnosticEventTypeTimestampWrap:
		return fmt.Sprintf("astits: pid %d %s wraparound at packet %d (offset %d): %d -> %d",
			e.PID, e.Timestamp, e.PacketIndex, e.ByteOffset, e.Previous, e.Current)
	case DiagnosticEventTypeMissingPTS:
		return fmt.Sprintf("astits: pid %d PES packet without PTS at packet %d (offset %d)",
			e.PID, e.PacketIndex, e.ByteOffset)
	case DiagnosticEventTypePESMalformed:
		return fmt.Sprintf("astits: pid %d malformed PES payload at packet %d (offset %d)",
			e.PID, e.PacketIndex, e.ByteOffset)
	default: // pcr/pts/dts jumps
		return fmt.Sprintf("astits: pid %d %s jump at packet %d (offset %d): %d -> %d (delta %d)",
			e.PID, e.Timestamp, e.PacketIndex, e.ByteOffset, e.Previous, e.Current, e.Delta)
	}
}

// ContinuityDiagnoser consumes transport stream packets and reports continuity counter
// gaps/duplicates and PCR/PES PTS/DTS anomalies per PID.
//
// It is fully streaming: each packet is processed in O(1) time and the only retained state
// is one bounded entry per active PID (plus, when MaxTrackedPIDs is set, an LRU index), so
// memory is bounded by the number of active PIDs rather than the stream length.
//
// PCR/PTS/DTS are 33-bit fields: comparisons are done modulo 2^33 so legal wraparound is
// never mistaken for a jump. A packet whose adaptation field carries the
// discontinuity_indicator re-bases the PID state and suppresses the gap/jump that the
// counter or timestamp change would otherwise imply.
type ContinuityDiagnoser struct {
	optPacketSize       int
	optMaxTrackedPIDs   int
	optPCRBackwardTol   int64
	optPTSBackwardTol   int64
	optDTSBackwardTol   int64
	optPCRMaxForwardGap int64
	optPTSMaxForwardGap int64
	optDTSMaxForwardGap int64

	packetIndex int
	// pidOrder keeps insertion/access order so the oldest entry can be evicted once
	// optMaxTrackedPIDs is reached.
	pidOrder []uint16
	states   map[uint32]*pidDiagnosticState
}

// pidDiagnosticState is the bounded per-PID diagnostic context.
type pidDiagnosticState struct {
	lastCC         uint8
	hasLastCC      bool
	lastHadPayload bool
	lastPayload    []byte
	lastPCR        int64
	hasLastPCR     bool
	lastPTS        int64
	hasLastPTS     bool
	lastDTS        int64
	hasLastDTS     bool
}

// ContinuityDiagnoserOptPacketSize sets the transport packet size used to compute byte
// offsets. Only 188 and 192 are valid. Default is 188.
func ContinuityDiagnoserOptPacketSize(packetSize int) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optPacketSize = packetSize }
}

// ContinuityDiagnoserOptMaxTrackedPIDs bounds the number of PIDs whose state is retained.
// When the limit is reached the least recently seen PID is evicted; its next packet is then
// treated as a first-seen baseline again. 0 (the default) keeps every PID, which is
// inherently bounded by the 8190 possible PIDs.
func ContinuityDiagnoserOptMaxTrackedPIDs(n int) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optMaxTrackedPIDs = n }
}

// ContinuityDiagnoserOptPCRBackwardTolerance sets the maximum tolerated magnitude of a
// backwards PCR drift, in 27 MHz ticks. A backwards delta whose magnitude exceeds the
// tolerance is reported as a jump. The zero default means any backwards step is reported.
func ContinuityDiagnoserOptPCRBackwardTolerance(ticks int64) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optPCRBackwardTol = ticks }
}

// ContinuityDiagnoserOptPTSBackwardTolerance sets the maximum tolerated magnitude of a
// backwards PES PTS drift, in 90 kHz units. The zero default means any backwards step is
// reported.
func ContinuityDiagnoserOptPTSBackwardTolerance(units int64) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optPTSBackwardTol = units }
}

// ContinuityDiagnoserOptDTSBackwardTolerance sets the maximum tolerated magnitude of a
// backwards PES DTS drift, in 90 kHz units. The zero default means any backwards step is
// reported.
func ContinuityDiagnoserOptDTSBackwardTolerance(units int64) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optDTSBackwardTol = units }
}

// ContinuityDiagnoserOptPCRMaxForwardGap enables forward PCR gap checking: a forward delta
// strictly larger than the given number of 27 MHz ticks is reported as a jump. 0 (the
// default) disables forward checking, since without bitrate information a large but legal
// PCR interval cannot be distinguished from a gap.
func ContinuityDiagnoserOptPCRMaxForwardGap(ticks int64) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optPCRMaxForwardGap = ticks }
}

// ContinuityDiagnoserOptPTSMaxForwardGap enables forward PTS gap checking in 90 kHz units.
// 0 disables it.
func ContinuityDiagnoserOptPTSMaxForwardGap(units int64) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optPTSMaxForwardGap = units }
}

// ContinuityDiagnoserOptDTSMaxForwardGap enables forward DTS gap checking in 90 kHz units.
// 0 disables it.
func ContinuityDiagnoserOptDTSMaxForwardGap(units int64) func(*ContinuityDiagnoser) {
	return func(d *ContinuityDiagnoser) { d.optDTSMaxForwardGap = units }
}

// NewContinuityDiagnoser creates a continuity diagnoser.
func NewContinuityDiagnoser(opts ...func(*ContinuityDiagnoser)) (*ContinuityDiagnoser, error) {
	d := &ContinuityDiagnoser{
		optPacketSize: MpegTsPacketSize,
		states:        make(map[uint32]*pidDiagnosticState),
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.optPacketSize != MpegTsPacketSize && d.optPacketSize != MpegTsPacketSize+4 {
		return nil, ErrInvalidPacketSize
	}
	if d.optMaxTrackedPIDs < 0 {
		d.optMaxTrackedPIDs = 0
	}
	return d, nil
}

// HandlePacket analyzes one parsed packet and returns the events it produces. The returned
// slice is nil for a fully healthy packet. Packets on the null PID (0x1fff) are ignored.
func (d *ContinuityDiagnoser) HandlePacket(p *Packet) []*DiagnosticEvent {
	var events []*DiagnosticEvent
	idx := d.packetIndex
	d.packetIndex++

	// Null packets carry no per-PID semantics and their counters are undefined.
	if p.Header.PID == PIDNull {
		return nil
	}

	st := d.stateFor(p.Header.PID)
	base := func(t DiagnosticEventType) *DiagnosticEvent {
		return &DiagnosticEvent{
			Type:              t,
			PID:               p.Header.PID,
			PacketIndex:       idx,
			ByteOffset:        idx * d.optPacketSize,
			ContinuityCounter: p.Header.ContinuityCounter,
		}
	}

	// Adaptation field control 0b00 is reserved for future use and makes the packet
	// illegal. It is a header-level defect, so it is reported even on the first packet.
	if !p.Header.HasAdaptationField && !p.Header.HasPayload {
		events = append(events, base(DiagnosticEventTypeCCMalformed))
	}

	// A corrupt packet is reported on its own and never participates in baselines:
	// its payload and timestamps are untrustworthy, and a first corrupt packet must
	// not look like a healthy first-seen one.
	if p.Header.TransportErrorIndicator {
		events = append(events, base(DiagnosticEventTypeTransportError))
		return events
	}

	// A declared discontinuity re-bases every baseline of the PID and suppresses the
	// gap/jump that the counter or timestamp change would otherwise imply.
	if p.Header.HasAdaptationField && p.AdaptationField != nil && p.AdaptationField.DiscontinuityIndicator {
		events = append(events, base(DiagnosticEventTypeDiscontinuity))
		d.rebase(st, p)
		return events
	}

	// First packet of the PID: establish the baseline without checking anything.
	if !st.hasLastCC {
		events = append(events, base(DiagnosticEventTypeCCFirstSeen))
		d.rebase(st, p)
		return events
	}

	events = append(events, d.checkContinuity(p, st, base)...)

	// Timestamps are only trustworthy on non-corrupt packets with an adaptation field.
	if p.Header.HasAdaptationField && p.AdaptationField != nil && p.AdaptationField.HasPCR && p.AdaptationField.PCR != nil {
		events = append(events, d.checkTimestamp(p, st, base, "pcr",
			pcrToTicks(p.AdaptationField.PCR), &st.hasLastPCR, &st.lastPCR,
			d.optPCRBackwardTol, d.optPCRMaxForwardGap)...)
	}

	if p.Header.HasPayload && p.Header.PayloadUnitStartIndicator && isPESPayload(p.Payload) {
		events = append(events, d.checkPES(p, st, base)...)
	}

	return events
}

// HandleDemuxer consumes every packet available from the demuxer until ErrNoMorePackets and
// returns all collected events. Any other error is returned alongside the events collected
// up to that point.
func (d *ContinuityDiagnoser) HandleDemuxer(dmx *Demuxer) (events []*DiagnosticEvent, err error) {
	for {
		var p *Packet
		if p, err = dmx.NextPacket(); err != nil {
			if err == ErrNoMorePackets {
				return events, nil
			}
			return events, err
		}
		events = append(events, d.HandlePacket(p)...)
	}
}

// ResetPID drops the retained state of one PID. Use it when the PID is known to have been
// remapped to another elementary stream: the next packet is treated as a fresh baseline
// instead of being compared against the old stream.
func (d *ContinuityDiagnoser) ResetPID(pid uint16) {
	if _, ok := d.states[uint32(pid)]; ok {
		delete(d.states, uint32(pid))
		for i, v := range d.pidOrder {
			if v == pid {
				d.pidOrder = append(d.pidOrder[:i], d.pidOrder[i+1:]...)
				break
			}
		}
	}
}

// Reset drops every retained PID state and restarts packet indexing.
func (d *ContinuityDiagnoser) Reset() {
	d.packetIndex = 0
	d.pidOrder = d.pidOrder[:0]
	for k := range d.states {
		delete(d.states, k)
	}
}

// stateFor returns the per-PID state, creating it (and evicting the least recently seen PID
// when MaxTrackedPIDs is reached) on first use.
func (d *ContinuityDiagnoser) stateFor(pid uint16) *pidDiagnosticState {
	key := uint32(pid)
	if st, ok := d.states[key]; ok {
		d.touch(pid)
		return st
	}

	if d.optMaxTrackedPIDs > 0 && len(d.pidOrder) >= d.optMaxTrackedPIDs {
		oldest := d.pidOrder[0]
		d.pidOrder = d.pidOrder[1:]
		delete(d.states, uint32(oldest))
	}

	st := &pidDiagnosticState{}
	d.states[key] = st
	d.pidOrder = append(d.pidOrder, pid)
	return st
}

// touch moves pid to the end of the LRU index.
func (d *ContinuityDiagnoser) touch(pid uint16) {
	for i, v := range d.pidOrder {
		if v == pid {
			copy(d.pidOrder[i:], d.pidOrder[i+1:])
			d.pidOrder[len(d.pidOrder)-1] = pid
			return
		}
	}
	d.pidOrder = append(d.pidOrder, pid)
}

// rebase resets every baseline of a PID to the given packet.
func (d *ContinuityDiagnoser) rebase(st *pidDiagnosticState, p *Packet) {
	st.hasLastCC = true
	st.lastCC = p.Header.ContinuityCounter
	st.lastHadPayload = p.Header.HasPayload
	if p.Header.HasPayload {
		st.lastPayload = append(st.lastPayload[:0], p.Payload...)
	} else {
		st.lastPayload = nil
	}

	if p.Header.HasAdaptationField && p.AdaptationField != nil && p.AdaptationField.HasPCR && p.AdaptationField.PCR != nil {
		st.hasLastPCR = true
		st.lastPCR = pcrToTicks(p.AdaptationField.PCR)
	} else {
		st.hasLastPCR = false
	}

	// A PES starting here establishes PTS/DTS baselines immediately.
	if p.Header.HasPayload && p.Header.PayloadUnitStartIndicator && isPESPayload(p.Payload) {
		if pes, err := parsePESData(astikit.NewBytesIterator(p.Payload)); err == nil && pes.Header != nil && pes.Header.OptionalHeader != nil {
			oh := pes.Header.OptionalHeader
			if oh.PTS != nil {
				st.hasLastPTS = true
				st.lastPTS = oh.PTS.Base
			} else {
				st.hasLastPTS = false
			}
			if oh.DTS != nil {
				st.hasLastDTS = true
				st.lastDTS = oh.DTS.Base
			} else {
				st.hasLastDTS = false
			}
		} else {
			st.hasLastPTS, st.hasLastDTS = false, false
		}
	} else {
		st.hasLastPTS, st.hasLastDTS = false, false
	}
}

// checkContinuity applies the MPEG-2 continuity counter rules.
func (d *ContinuityDiagnoser) checkContinuity(p *Packet, st *pidDiagnosticState, base func(DiagnosticEventType) *DiagnosticEvent) []*DiagnosticEvent {
	var events []*DiagnosticEvent
	cc := p.Header.ContinuityCounter

	// A packet carrying a payload must increment the counter; an adaptation-only packet
	// must repeat it. This mirrors the stream assembly rule used by the packet pool.
	var expected uint8
	if p.Header.HasPayload {
		expected = (st.lastCC + 1) & 0x0f
	} else {
		expected = st.lastCC
	}

	if cc == st.lastCC && p.Header.HasPayload && st.lastHadPayload {
		// Same counter on two consecutive payload packets: a legal retransmission only
		// when the payload itself is identical.
		if bytesEqual(st.lastPayload, p.Payload) {
			events = append(events, base(DiagnosticEventTypeCCDuplicate))
			// Retransmission does not advance the stream: keep the previous baseline untouched.
			return events
		}
		e := base(DiagnosticEventTypeCCError)
		e.PreviousContinuityCounter = st.lastCC
		events = append(events, e)
	} else if cc != expected {
		// Any other deviation is a gap; modular arithmetic yields 1..15 missing packets.
		// A zero modular distance means the counter repeated where an increment was
		// expected: that is an error, not a gap.
		missing := int((cc - st.lastCC) & 0x0f)
		var e *DiagnosticEvent
		if missing == 0 {
			e = base(DiagnosticEventTypeCCError)
		} else {
			e = base(DiagnosticEventTypeCCGap)
			e.MissingCount = missing
		}
		e.PreviousContinuityCounter = st.lastCC
		events = append(events, e)
	}

	st.lastCC = cc
	st.lastHadPayload = p.Header.HasPayload
	if p.Header.HasPayload {
		st.lastPayload = append(st.lastPayload[:0], p.Payload...)
	} else {
		st.lastPayload = nil
	}
	return events
}

// checkTimestamp compares one timestamp against its per-PID baseline, handling 33-bit
// wraparound and the configured tolerances.
func (d *ContinuityDiagnoser) checkTimestamp(
	p *Packet,
	st *pidDiagnosticState,
	base func(DiagnosticEventType) *DiagnosticEvent,
	kind string,
	current int64,
	hasPrevious *bool,
	previous *int64,
	backwardTolerance, maxForwardGap int64,
) []*DiagnosticEvent {
	if !*hasPrevious {
		*hasPrevious = true
		*previous = current
		return nil
	}

	modulus := int64(1) << 33
	delta := modularDelta(current, *previous, modulus)
	var events []*DiagnosticEvent

	// A decrease of the raw field can only be a wraparound (or an outright reset).
	if current < *previous {
		events = append(events, timestampEvent(base, DiagnosticEventTypeTimestampWrap, kind, *previous, current, delta))
	}

	jump := false
	if delta < 0 && -delta > backwardTolerance {
		jump = true
	} else if delta > 0 && maxForwardGap > 0 && delta > maxForwardGap {
		jump = true
	}
	if jump {
		events = append(events, timestampEvent(base, jumpEventType(kind), kind, *previous, current, delta))
	}

	*previous = current
	return events
}

// checkPES parses the PES payload starting in this packet and validates its PTS/DTS.
func (d *ContinuityDiagnoser) checkPES(p *Packet, st *pidDiagnosticState, base func(DiagnosticEventType) *DiagnosticEvent) []*DiagnosticEvent {
	pes, err := parsePESData(astikit.NewBytesIterator(p.Payload))
	if err != nil || pes.Header == nil {
		return []*DiagnosticEvent{base(DiagnosticEventTypePESMalformed)}
	}

	oh := pes.Header.OptionalHeader
	if oh == nil || oh.PTS == nil {
		// No PTS on a PES packet: the timestamp baseline cannot continue from this packet.
		st.hasLastPTS, st.hasLastDTS = false, false
		return []*DiagnosticEvent{base(DiagnosticEventTypeMissingPTS)}
	}

	var events []*DiagnosticEvent
	events = append(events, d.checkTimestamp(p, st, base, "pts", oh.PTS.Base,
		&st.hasLastPTS, &st.lastPTS, d.optPTSBackwardTol, d.optPTSMaxForwardGap)...)
	if oh.DTS != nil {
		events = append(events, d.checkTimestamp(p, st, base, "dts", oh.DTS.Base,
			&st.hasLastDTS, &st.lastDTS, d.optDTSBackwardTol, d.optDTSMaxForwardGap)...)
	}
	return events
}

// jumpEventType maps a timestamp kind to its jump event type.
func jumpEventType(kind string) DiagnosticEventType {
	switch kind {
	case "pcr":
		return DiagnosticEventTypePCRJump
	case "dts":
		return DiagnosticEventTypeDTSJump
	default:
		return DiagnosticEventTypePTSJump
	}
}

func timestampEvent(base func(DiagnosticEventType) *DiagnosticEvent, t DiagnosticEventType, kind string, previous, current, delta int64) *DiagnosticEvent {
	e := base(t)
	e.Timestamp = kind
	e.Previous = previous
	e.Current = current
	e.Delta = delta
	return e
}

// pcrToTicks converts a PCR to a single monotonically comparable 27 MHz tick count.
func pcrToTicks(cr *ClockReference) int64 {
	return cr.Base*300 + cr.Extension
}

// modularDelta returns current-previous wrapped into (-modulus/2, modulus/2], so a legal
// 33-bit wraparound appears as a positive delta and an actual backwards step stays negative.
func modularDelta(current, previous, modulus int64) int64 {
	half := modulus / 2
	delta := (current - previous) & (modulus - 1)
	if delta > half {
		delta -= modulus
	}
	return delta
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
