package astits

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasDiscontinuity(t *testing.T) {
	// Normal wrap around: 15 -> 0 with a payload is expected.
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 0, HasPayload: true}}))

	// Explicit discontinuity indicator: the counter jump is expected, no loss.
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{AdaptationField: &PacketAdaptationField{DiscontinuityIndicator: true}, Header: PacketHeader{ContinuityCounter: 0, HasAdaptationField: true, HasPayload: true}}))

	// PIDNull never takes part in the continuity check, whatever its counter.
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{PID: PIDNull, ContinuityCounter: 0, HasPayload: true}}))
	assert.False(t, hasDiscontinuity(nil, &Packet{Header: PacketHeader{PID: PIDNull, ContinuityCounter: 7, HasPayload: true}}))

	// The first packet of an empty pool can't be discontinuous.
	assert.False(t, hasDiscontinuity(nil, &Packet{Header: PacketHeader{ContinuityCounter: 5, HasPayload: true}}))
	assert.False(t, hasDiscontinuity([]*Packet{}, &Packet{Header: PacketHeader{ContinuityCounter: 5, HasPayload: true}}))

	// A packet without payload must repeat the previous counter: same counter is fine,
	// a different one is a loss.
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 15}}))
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 0}}))

	// Undeclared counter jumps are still detected.
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true}}))
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true}}))
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 5}}}, &Packet{Header: PacketHeader{ContinuityCounter: 8, HasPayload: true}}))
}

func TestIsSameAsPrevious(t *testing.T) {
	assert.False(t, isSameAsPrevious([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1}}))
	assert.False(t, isSameAsPrevious([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 2, HasPayload: true}}))
	assert.True(t, isSameAsPrevious([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true}}))
}

func TestPacketPool(t *testing.T) {
	b := newPacketPool(nil)
	ps := b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 0, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1}})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true, PID: 2}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 2, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 3, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1}})
	assert.Len(t, ps, 2)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 5, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 6, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1}})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 7, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 2)
	assert.Equal(t, uint16(1), ps[0].Header.PID)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 1)
	assert.Equal(t, uint16(2), ps[0].Header.PID)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 0)
}

// A continuity counter wrapping from 15 to 0 with a payload is normal and must not
// flush the payload being assembled.
func TestPacketPoolContinuityCounterWrapAround(t *testing.T) {
	b := newPacketPool(nil)
	p0 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 14, HasPayload: true, PayloadUnitStartIndicator: true}}
	p1 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 15, HasPayload: true}}
	p2 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 0, HasPayload: true}}
	p3 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true}}

	assert.Empty(t, b.addUnlocked(p0))
	assert.Empty(t, b.addUnlocked(p1))
	assert.Empty(t, b.addUnlocked(p2))
	assert.Equal(t, []*Packet{p0, p1, p2}, b.addUnlocked(p3))
}

// An explicit discontinuity indicator means the counter jump is expected: the packet
// must not be treated as a loss and the buffer must not be cleared because of it.
func TestPacketPoolDiscontinuityIndicator(t *testing.T) {
	b := newPacketPool(nil)
	af := &PacketAdaptationField{DiscontinuityIndicator: true}
	p0 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 15, HasPayload: true, PayloadUnitStartIndicator: true}}
	p1 := &Packet{AdaptationField: af, Header: PacketHeader{PID: 1, ContinuityCounter: 7, HasAdaptationField: true, HasPayload: true, PayloadUnitStartIndicator: true}}
	p2 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 8, HasPayload: true, PayloadUnitStartIndicator: true}}

	assert.Empty(t, b.addUnlocked(p0))
	// Were the jump treated as a loss, the buffer would be cleared and nothing returned.
	assert.Equal(t, []*Packet{p0}, b.addUnlocked(p1))
	// The track continues from the new counter.
	assert.Equal(t, []*Packet{p1}, b.addUnlocked(p2))
}

// An undeclared counter jump is still a loss and must flush the pending payload.
func TestPacketPoolUndeclaredLossFlushesBuffer(t *testing.T) {
	b := newPacketPool(nil)
	p0 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true}}
	lost := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 3, HasPayload: true}}
	p2 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 4, HasPayload: true, PayloadUnitStartIndicator: true}}

	assert.Empty(t, b.addUnlocked(p0))
	// Counter 2 is missing: p0 must be dropped, not returned.
	assert.Empty(t, b.addUnlocked(lost))
	assert.Equal(t, []*Packet{lost}, b.addUnlocked(p2))
}

// A repeated counter is a duplicate, not a loss: the duplicate is dropped and the
// payload being assembled is preserved.
func TestPacketPoolDuplicateCounterKeepsBuffer(t *testing.T) {
	b := newPacketPool(nil)
	p0 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true}}
	p1 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 2, HasPayload: true}}
	dup := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 2, HasPayload: true}}
	p2 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 3, HasPayload: true, PayloadUnitStartIndicator: true}}

	assert.Empty(t, b.addUnlocked(p0))
	assert.Empty(t, b.addUnlocked(p1))
	assert.Empty(t, b.addUnlocked(dup))
	assert.Equal(t, []*Packet{p0, p1}, b.addUnlocked(p2))
}

// PIDNull packets are padding: they never create nor disturb any track accumulator.
func TestPacketPoolPIDNullIsIgnoredForContinuity(t *testing.T) {
	b := newPacketPool(nil)
	p0 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 15, HasPayload: true, PayloadUnitStartIndicator: true}}
	null0 := &Packet{Header: PacketHeader{PID: PIDNull, ContinuityCounter: 0, HasPayload: true}}
	null1 := &Packet{Header: PacketHeader{PID: PIDNull, ContinuityCounter: 9, HasPayload: true}}
	p1 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 0, HasPayload: true, PayloadUnitStartIndicator: true}}

	assert.Empty(t, b.addUnlocked(p0))
	// Null packets interleaved with a track must neither flush nor take part in it.
	assert.Empty(t, b.addUnlocked(null0))
	assert.Empty(t, b.addUnlocked(null1))
	assert.Equal(t, []*Packet{p0}, b.addUnlocked(p1))
}

// Audio and video PIDs are accumulated independently, so a discontinuity on one
// track must not alter the state of the other.
func TestPacketPoolInterleavedPIDsKeepIndependentState(t *testing.T) {
	b := newPacketPool(nil)
	video0 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 3, HasPayload: true, PayloadUnitStartIndicator: true}}
	audio0 := &Packet{Header: PacketHeader{PID: 2, ContinuityCounter: 7, HasPayload: true, PayloadUnitStartIndicator: true}}
	video1 := &Packet{Header: PacketHeader{PID: 1, ContinuityCounter: 4, HasPayload: true, PayloadUnitStartIndicator: true}}
	audio1 := &Packet{Header: PacketHeader{PID: 2, ContinuityCounter: 8, HasPayload: true, PayloadUnitStartIndicator: true}}
	audioDiscontinuity := &Packet{AdaptationField: &PacketAdaptationField{DiscontinuityIndicator: true}, Header: PacketHeader{PID: 2, ContinuityCounter: 0, HasAdaptationField: true, HasPayload: true}}
	audio2 := &Packet{Header: PacketHeader{PID: 2, ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true}}

	assert.Empty(t, b.addUnlocked(video0))
	assert.Empty(t, b.addUnlocked(audio0))
	assert.Equal(t, []*Packet{video0}, b.addUnlocked(video1))
	assert.Equal(t, []*Packet{audio0}, b.addUnlocked(audio1))
	// The audio discontinuity doesn't affect the video track and joins the pending audio payload.
	assert.Empty(t, b.addUnlocked(audioDiscontinuity))
	assert.Equal(t, []*Packet{audio1, audioDiscontinuity}, b.addUnlocked(audio2))
}
