package astits

import (
	"bytes"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
)

func TestParseData(t *testing.T) {
	// Init
	pm := newProgramMap()
	esm := newElementaryStreamMap()
	ps := []*Packet{}

	// Custom parser
	cds := []*DemuxerData{{PID: 1}}
	var c = func(ps []*Packet) (o []*DemuxerData, skip bool, err error) {
		o = cds
		skip = true
		return
	}
	ds, err := parseData(ps, c, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, cds, ds)

	// Do nothing for CAT
	ps = []*Packet{{Header: PacketHeader{PID: PIDCAT}}}
	ds, err = parseData(ps, nil, pm, esm)
	assert.NoError(t, err)
	assert.Empty(t, ds)

	// PES
	p := pesWithHeaderBytes()
	ps = []*Packet{
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[:33],
		},
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[33:],
		},
	}
	ds, err = parseData(ps, nil, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, []*DemuxerData{
		{
			FirstPacket: &Packet{Header: ps[0].Header, AdaptationField: ps[0].AdaptationField},
			PES:         pesWithHeader(),
			PID:         uint16(256),
		}}, ds)

	// PSI
	pm.setUnlocked(uint16(256), uint16(1))
	p = psiBytes()
	ps = []*Packet{
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[:33],
		},
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[33:],
		},
	}
	ds, err = parseData(ps, nil, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, psi.toData(
		&Packet{Header: ps[0].Header, AdaptationField: ps[0].AdaptationField},
		uint16(256),
	), ds)
}

func TestIsPSIPayload(t *testing.T) {
	pm := newProgramMap()
	esm := newElementaryStreamMap()
	var pids []int
	for i := 0; i <= 255; i++ {
		if isPSIPayload(uint16(i), pm, esm) {
			pids = append(pids, i)
		}
	}
	assert.Equal(t, []int{0, 16, 17, 18, 19, 20, 30, 31}, pids)
	pm.setUnlocked(uint16(1), uint16(0))
	assert.True(t, isPSIPayload(uint16(1), pm, esm))

	esm.setUnlocked(uint16(256), uint16(1))
	assert.False(t, isPSIPayload(uint16(256), pm, esm))
	esm.setUnlocked(uint16(0x11), uint16(1))
	assert.False(t, isPSIPayload(uint16(0x11), pm, esm))
}

func TestIsPESPayload(t *testing.T) {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write("0000000000000001")
	assert.False(t, isPESPayload(buf.Bytes()))
	buf.Reset()
	w.Write("000000000000000000000001")
	assert.True(t, isPESPayload(buf.Bytes()))
}

func BenchmarkIsPESComplete(b *testing.B) {
	for _, tc := range []struct {
		name    string
		packets int
		bounded bool
		partial bool
	}{
		{"unbounded_1", 1, false, false},
		{"unbounded_256", 256, false, false},
		{"bounded_partial_256", 256, true, true},
		{"bounded_complete_256", 256, true, false},
		{"bounded_complete_1", 1, true, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			payload := make([]byte, tc.packets*184)
			copy(payload, []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0})
			if tc.bounded {
				length := len(payload) - 6
				if tc.partial {
					length++
				}
				payload[4], payload[5] = byte(length>>8), byte(length)
			}
			packets := make([]*Packet, tc.packets)
			for i := range packets {
				packets[i] = &Packet{Payload: payload[i*184 : (i+1)*184]}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				isPESComplete(packets)
			}
		})
	}
}

func TestIsPESComplete_PreservesHeaderValidation(t *testing.T) {
	for _, payload := range [][]byte{
		{0, 0, 1, 0xbe, 0, 1, 42}, // No optional header.
		{0, 0, 1, 0xe0, 0, 4, 0x80, 0, 0, 42},
		{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0, 42}, // Unbounded video.
		{0, 0, 1, 0xe0, 0, 1, 0x80, 0x80, 5},  // Truncated PTS despite full declared length.
		{0, 0, 1, 0xe0, 0, 8, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1},
		{0, 0, 1, 0xe0, 0, 4, 0x80, 1, 1, 0x80}, // Truncated extension.
	} {
		for end := 0; end <= len(payload); end++ {
			data := payload[:end]
			want := false
			if len(data) >= 3 {
				i := astikit.NewBytesIterator(data)
				i.Seek(3)
				h, _, dataEnd, err := parsePESHeader(i)
				want = err == nil && h.PacketLength != 0 && i.Len() >= dataEnd
			}
			for split := 0; split <= end; split++ {
				packets := []*Packet{{Payload: data[:split]}, {}, {Payload: data[split:]}}
				assert.Equal(t, want, isPESComplete(packets), "payload=%x end=%d split=%d", payload, end, split)
			}
		}
	}
}

func TestIsPESComplete_UnboundedAndPartialDoNotAllocate(t *testing.T) {
	for _, length := range []byte{0, 255} {
		packets := []*Packet{{Payload: []byte{0, 0, 1, 0xe0, 0, length, 0x80, 0, 0}}}
		assert.False(t, isPESComplete(packets))
		assert.Zero(t, testing.AllocsPerRun(100, func() { isPESComplete(packets) }))
	}
}

func TestIsPESPacketsPayload(t *testing.T) {
	// Prefix split across packets
	assert.True(t, isPESPacketsPayload([]*Packet{{Payload: []byte{0}}, {Payload: []byte{0, 1, 0xe0}}}))
	assert.True(t, isPESPacketsPayload([]*Packet{{}, {Payload: []byte{0, 0}}, {}, {Payload: []byte{1}}}))
	// Not enough bytes yet
	assert.False(t, isPESPacketsPayload([]*Packet{{Payload: []byte{0, 0}}}))
	assert.False(t, isPESPacketsPayload([]*Packet{{}}))
	// Invalid start code
	assert.False(t, isPESPacketsPayload([]*Packet{{Payload: []byte{0, 0, 2}}}))
	assert.False(t, isPESPacketsPayload([]*Packet{{Payload: []byte{0}}, {Payload: []byte{1, 0}}}))
}

func TestIsPESComplete_BoundaryLengths(t *testing.T) {
	// 9-byte PES: 6-byte fixed header + 3-byte optional header, no data.
	base := []byte{0, 0, 1, 0xe0, 0, 3, 0x80, 0, 0}
	for _, tc := range []struct {
		name           string
		declaredLength int
		received       int
		want           bool
	}{
		{"exact", 3, 9, true},
		{"one_more_than_received", 4, 9, false},
		{"one_less_than_received", 2, 9, true},
		{"declared_max_received_short", 0xffff, 9, false},
	} {
		payload := make([]byte, tc.received)
		copy(payload, base)
		payload[4], payload[5] = byte(tc.declaredLength>>8), byte(tc.declaredLength)
		// Whole payload in a single packet.
		assert.Equal(t, tc.want, isPESComplete([]*Packet{{Payload: payload}}), tc.name)
		// Same payload fragmented across packets at every split point.
		for split := 0; split <= tc.received; split++ {
			packets := []*Packet{{Payload: payload[:split]}, {Payload: payload[split:]}}
			assert.Equal(t, tc.want, isPESComplete(packets), "%s split=%d", tc.name, split)
		}
	}
}

// Checking a large fragmented PES must not copy the payload on every fragment:
// the incomplete path performs no allocation at all, whatever the fragment count.
func TestIsPESComplete_LargeFragmentedPESDoesNotAllocate(t *testing.T) {
	const count = 6000
	const fragment = 10 // Small fragments so the declared length fits in uint16.
	payload := make([]byte, count*fragment)
	copy(payload, []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0})
	// Declare one byte more than will be received: the PES stays incomplete.
	length := len(payload) - 6 + 1
	payload[4], payload[5] = byte(length>>8), byte(length)
	packets := make([]*Packet, count)
	for i := range packets {
		packets[i] = &Packet{Payload: payload[i*fragment : (i+1)*fragment]}
	}
	assert.False(t, isPESComplete(packets))
	assert.Zero(t, testing.AllocsPerRun(10, func() { isPESComplete(packets) }))

	// Once the declared length is received, the PES is complete.
	payload[4], payload[5] = byte((len(payload)-6)>>8), byte(len(payload)-6)
	assert.True(t, isPESComplete(packets))
}
