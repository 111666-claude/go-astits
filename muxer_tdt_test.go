package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMuxerWithStream(t *testing.T, buf *bytes.Buffer) *Muxer {
	t.Helper()
	m := NewMuxer(context.Background(), buf)
	require.NoError(t, m.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	require.NoError(t, m.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x0234,
		StreamType:    StreamTypeAACAudio,
	}))
	m.SetPCRPID(0x1234)
	return m
}

// TestMuxer_WriteTDT_PacketShape verifies the emitted packet structure: PID 0x14, payload unit
// start, monotonic 4-bit continuity counter, correct section header and 188-byte size.
func TestMuxer_WriteTDT_PacketShape(t *testing.T) {
	buf := &bytes.Buffer{}
	m := newTestMuxerWithStream(t, buf)

	when := utcDate("2024-02-29 23:59:59")
	n, err := m.WriteTDT(when)
	require.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, n)

	raw := buf.Bytes()
	require.Len(t, raw, MpegTsPacketSize)

	// Header: sync byte, PUSI set, PID 0x14, payload-only, CC 0.
	assert.Equal(t, byte(syncByte), raw[0])
	assert.Equal(t, byte(0x40), raw[1]&0x40) // payload unit start indicator
	pid := uint16(raw[1]&0x1f)<<8 | uint16(raw[2])
	assert.Equal(t, PIDTDT, pid)
	assert.Equal(t, byte(0x10), raw[3]&0x30) // adaptation_field_control = payload only
	assert.Equal(t, byte(0x00), raw[3]&0x0f) // first continuity counter is 0

	// Payload: pointer field 0, then the TDT section.
	assert.Equal(t, byte(0x00), raw[4])
	assert.Equal(t, byte(PSITableIDTDT), raw[5])
	assert.Equal(t, byte(0x70), raw[6]) // syntax indicator 0, private bit 1, reserved 11, length high nibble 0
	assert.Equal(t, byte(0x05), raw[7]) // section length = 5
	assert.Equal(t, dvbTimeStrictBytes(t, when), raw[8:13])

	// Remaining payload bytes are 0xff stuffing.
	for _, b := range raw[13:] {
		assert.Equal(t, byte(0xff), b)
	}
}

func TestMuxer_WriteTDT_MultipleWritesContinuity(t *testing.T) {
	buf := &bytes.Buffer{}
	m := newTestMuxerWithStream(t, buf)

	times := []time.Time{
		utcDate("2024-02-29 23:59:58"),
		utcDate("2024-02-29 23:59:59"),
		utcDate("2024-03-01 00:00:00"), // crosses midnight
	}
	for _, when := range times {
		n, err := m.WriteTDT(when)
		require.NoError(t, err)
		assert.Equal(t, MpegTsPacketSize, n)
	}

	raw := buf.Bytes()
	require.Len(t, raw, 3*MpegTsPacketSize)

	// Continuity counters increment 0,1,2 and every packet is on the TDT PID.
	for idx := 0; idx < 3; idx++ {
		pkt := raw[idx*MpegTsPacketSize : (idx+1)*MpegTsPacketSize]
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		assert.Equal(t, PIDTDT, pid)
		assert.Equal(t, byte(idx), pkt[3]&0x0f)
	}

	// Full demux read-back returns the three TDTs at second precision.
	dmx := NewDemuxer(context.Background(), bytes.NewReader(raw), DemuxerOptPacketSize(MpegTsPacketSize))
	var got []time.Time
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		if d.TDT != nil {
			got = append(got, d.TDT.UTCTime)
		}
	}
	require.Len(t, got, len(times))
	for i := range times {
		assert.True(t, got[i].Equal(times[i]), "got %s want %s", got[i], times[i])
		assert.Equal(t, time.UTC, got[i].Location())
	}
}

// TestMuxer_WriteTDT_Interleaved verifies TDT packets inserted between PAT/PMT and audio/video
// PES packets do not disturb the existing tracks or their continuity counters.
func TestMuxer_WriteTDT_Interleaved(t *testing.T) {
	buf := &bytes.Buffer{}
	m := newTestMuxerWithStream(t, buf)

	payload := testPayload()
	pts := ClockReference{Base: 5726623060}
	pesData := func() *PESData {
		return &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					DTS:             &pts,
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorBothPresent,
				},
			},
		}
	}

	tdt1 := utcDate("2024-02-29 20:00:00")
	tdt2 := utcDate("2024-02-29 20:00:30")
	tdt3 := utcDate("2024-02-29 21:15:45")

	// Video PES (also emits PAT/PMT at stream start), then TDT, then audio PES, then more TDTs.
	_, err := m.WriteData(&MuxerData{PID: 0x1234, PES: pesData()})
	require.NoError(t, err)
	_, err = m.WriteTDT(tdt1)
	require.NoError(t, err)
	_, err = m.WriteData(&MuxerData{PID: 0x0234, PES: pesData()})
	require.NoError(t, err)
	_, err = m.WriteTDT(tdt2)
	require.NoError(t, err)
	_, err = m.WriteTDT(tdt3)
	require.NoError(t, err)

	raw := buf.Bytes()
	require.Equal(t, 0, len(raw)%MpegTsPacketSize)

	// Walk every packet: check PID ordering and per-PID continuity.
	seen := map[uint16]int{}
	tdtCC := -1
	tdtCount := 0
	var pidsInOrder []uint16
	for off := 0; off < len(raw); off += MpegTsPacketSize {
		pkt := raw[off : off+MpegTsPacketSize]
		require.Equal(t, byte(syncByte), pkt[0])
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		cc := int(pkt[3] & 0x0f)
		hasPayload := pkt[3]&0x10 != 0
		require.True(t, hasPayload)

		if pid == PIDTDT {
			tdtCount++
			if tdtCC >= 0 {
				assert.Equal(t, (tdtCC+1)&0x0f, cc, "TDT continuity counter break")
			}
			tdtCC = cc
		} else if prev, ok := seen[pid]; ok {
			assert.Equal(t, (prev+1)&0x0f, cc, "PID %#x continuity counter break", pid)
		}
		seen[pid] = cc
		pidsInOrder = append(pidsInOrder, pid)
	}
	assert.Equal(t, 3, tdtCount)
	assert.Contains(t, pidsInOrder, PIDPAT)
	assert.Contains(t, pidsInOrder, pmtStartPID)
	assert.Contains(t, pidsInOrder, uint16(0x1234))
	assert.Contains(t, pidsInOrder, uint16(0x0234))

	// Demux the whole stream and confirm all track types plus TDT survive.
	dmx := NewDemuxer(context.Background(), bytes.NewReader(raw), DemuxerOptPacketSize(MpegTsPacketSize))
	var tdtTimes []time.Time
	var pat, pmt, pes int
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)
		switch {
		case d.PAT != nil:
			pat++
		case d.PMT != nil:
			pmt++
		case d.PES != nil:
			pes++
		case d.TDT != nil:
			tdtTimes = append(tdtTimes, d.TDT.UTCTime)
		}
	}
	assert.GreaterOrEqual(t, pat, 1)
	assert.GreaterOrEqual(t, pmt, 1)
	assert.GreaterOrEqual(t, pes, 1)
	require.Len(t, tdtTimes, 3)
	assert.True(t, tdtTimes[0].Equal(tdt1))
	assert.True(t, tdtTimes[1].Equal(tdt2))
	assert.True(t, tdtTimes[2].Equal(tdt3))
}

// TestMuxer_WriteTDT_DisabledByDefault ensures a muxer that never calls WriteTDT produces no
// PID 0x14 packets and behaves exactly as before.
func TestMuxer_WriteTDT_DisabledByDefault(t *testing.T) {
	buf := &bytes.Buffer{}
	m := newTestMuxerWithStream(t, buf)

	_, err := m.WriteData(&MuxerData{PID: 0x1234, PES: &PESData{
		Data: testPayload(),
		Header: &PESHeader{OptionalHeader: &PESOptionalHeader{
			PTS:             &ClockReference{Base: 1},
			PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
		}},
	}})
	require.NoError(t, err)

	raw := buf.Bytes()
	for off := 0; off < len(raw); off += MpegTsPacketSize {
		pid := uint16(raw[off+1]&0x1f)<<8 | uint16(raw[off+2])
		assert.NotEqual(t, PIDTDT, pid)
	}
}

func TestMuxer_WriteTDT_UnrepresentableTime(t *testing.T) {
	buf := &bytes.Buffer{}
	m := newTestMuxerWithStream(t, buf)

	before := buf.Len()
	_, err := m.WriteTDT(utcDate("2040-01-01 00:00:00"))
	assert.True(t, errors.Is(err, ErrDVBTimeOutOfRange))
	// Nothing must have been written on failure.
	assert.Equal(t, before, buf.Len())

	// Sub-second precision is also rejected.
	_, err = m.WriteTDT(utcDate("2024-01-01 00:00:00").Add(500 * time.Millisecond))
	assert.True(t, errors.Is(err, ErrDVBTimeSubSecond))
	assert.Equal(t, before, buf.Len())
}
