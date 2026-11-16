package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedClock is an injectable M2TSClock returning a fixed sequence of
// 90 kHz ticks. It records how often it was sampled and can be made to jump
// backwards to exercise the monotonicity check.
type scriptedClock struct {
	ticks   []uint64
	idx     int
	samples int
}

func (c *scriptedClock) NowM2TSTicks() uint64 {
	c.samples++
	v := c.ticks[c.idx]
	if c.idx < len(c.ticks)-1 {
		c.idx++
	}
	return v
}

// newAVMuxer builds a muxer with one H264 video ES and one AAC audio ES, the
// video PID being the PCR PID. It returns the muxer and the backing buffer.
func newAVMuxer(t *testing.T, opts ...func(*Muxer)) (*Muxer, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	muxer, err := NewMuxer(context.Background(), buf, opts...)
	require.NoError(t, err)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x0234,
		StreamType:    StreamTypeAACAudio,
	}))
	muxer.SetPCRPID(0x1234)
	return muxer, buf
}

func videoPESData(payload []byte, ptsBase int64) *MuxerData {
	pts := ClockReference{Base: ptsBase}
	return &MuxerData{
		PID: 0x1234,
		AdaptationField: &PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &ClockReference{Base: ptsBase + 1, Extension: 341},
			RandomAccessIndicator: true,
		},
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					DTS:             &pts,
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorBothPresent,
				},
			},
		},
	}
}

func audioPESData(payload []byte, ptsBase int64) *MuxerData {
	pts := ClockReference{Base: ptsBase}
	return &MuxerData{
		PID: 0x0234,
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorOnlyPTS,
				},
			},
		},
	}
}

// TestM2TS_OutputIs192BytesWithBDAVPrefix verifies requirement 1 and 2: every
// emitted record is 192 bytes, starts with a 4-byte header whose following
// byte is the TS sync byte, and the embedded 188-byte packet is identical to
// what the plain muxer produces.
func TestM2TS_OutputIs192BytesWithBDAVPrefix(t *testing.T) {
	payload := testPayload() // 256 bytes, spans several TS packets

	clock := &scriptedClock{ticks: []uint64{90000, 180000, 270000, 360000, 450000, 540000, 630000, 720000}}
	muxer, buf := newAVMuxer(t, WithM2TS(clock, M2TSCopyPermissionCopyFree))

	n, err := muxer.WriteData(videoPESData(payload, 5726623060))
	require.NoError(t, err)
	require.NoError(t, muxer.Flush())

	// n counts the 188-byte TS bytes the muxer framed; the on-disk size adds
	// a 4-byte header per packet.
	assert.Equal(t, buf.Len()/M2TSPacketSize*MpegTsPacketSize, n)
	assert.Equal(t, 0, buf.Len()%M2TSPacketSize)
	numPackets := buf.Len() / M2TSPacketSize
	require.Greater(t, numPackets, 2) // PAT + PMT + at least one PES packet

	raw := buf.Bytes()
	for i := 0; i < numPackets; i++ {
		rec := raw[i*M2TSPacketSize : (i+1)*M2TSPacketSize]

		// Sync byte must sit right after the 4-byte BDAV header.
		assert.Equal(t, byte(syncByte), rec[m2tsHeaderSize], "packet %d: sync byte at wrong position", i)

		// Top two bits encode the copy permission indicator (0 = copy free),
		// the remaining 30 bits are the ATS.
		cp, ats := parseM2TSHeader(rec[:m2tsHeaderSize])
		assert.Equal(t, M2TSCopyPermissionCopyFree, cp, "packet %d: copy permission", i)
		assert.Equal(t, uint32(clock.ticks[i]&m2tsATSMask), ats, "packet %d: ATS mismatch", i)
	}

	// One clock sample per emitted packet, deterministic and ordered.
	assert.Equal(t, numPackets, clock.samples)
}

// TestM2TS_TSPayloadIdenticalToPlainMuxer proves the BDAV header is the only
// difference: stripping the 4-byte prefixes yields the exact 188-byte stream
// the plain muxer would have written, including PAT/PMT and continuity counters.
func TestM2TS_TSPayloadIdenticalToPlainMuxer(t *testing.T) {
	videoPayload := testPayload()
	audioPayload := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x11, 0x22}

	// Plain 188-byte reference output.
	plainMuxer, plainBuf := newAVMuxer(t)
	_, err := plainMuxer.WriteData(videoPESData(videoPayload, 5726623060))
	require.NoError(t, err)
	_, err = plainMuxer.WriteData(audioPESData(audioPayload, 5726623060+9000))
	require.NoError(t, err)

	// M2TS output driven by a monotonic clock (values themselves are irrelevant
	// for the payload comparison).
	ticks := make([]uint64, 64)
	for i := range ticks {
		ticks[i] = uint64(i+1) * 90000
	}
	m2tsMuxer, m2tsBuf := newAVMuxer(t, WithM2TS(&scriptedClock{ticks: ticks}, M2TSCopyPermissionNoMoreCopies))
	_, err = m2tsMuxer.WriteData(videoPESData(videoPayload, 5726623060))
	require.NoError(t, err)
	_, err = m2tsMuxer.WriteData(audioPESData(audioPayload, 5726623060+9000))
	require.NoError(t, err)
	require.NoError(t, m2tsMuxer.Flush())

	require.Equal(t, plainBuf.Len()*M2TSPacketSize/MpegTsPacketSize, m2tsBuf.Len())

	// Strip every 4-byte prefix and compare the TS packets byte for byte.
	stripped := make([]byte, 0, plainBuf.Len())
	raw := m2tsBuf.Bytes()
	for i := 0; i < len(raw); i += M2TSPacketSize {
		stripped = append(stripped, raw[i+m2tsHeaderSize:i+M2TSPacketSize]...)
	}
	assert.Equal(t, plainBuf.Bytes(), stripped)
}

// TestM2TS_ATSWrap verifies the 30-bit wrap is deterministic: a tick above
// 2^30 is truncated on the wire while monotonicity is still enforced on the
// full 64-bit clock values.
func TestM2TS_ATSWrap(t *testing.T) {
	base := uint64(m2tsATSWrap) - 2 // ...fffffffe
	clock := &scriptedClock{ticks: []uint64{
		base,     // on-wire ATS = 2^30 - 2
		base + 1, // 2^30 - 1
		base + 2, // wraps to 0 on the wire
		base + 3, // 1
		base + 4, // 2, second wrap region passed deterministically
	}}
	muxer, buf := newAVMuxer(t, WithM2TS(clock, M2TSCopyPermissionCopyFree))
	require.NoError(t, muxer.Flush())

	// Force two table packets plus enough PES packets to consume the script.
	payload := bytes.Repeat([]byte{0x7f}, MpegTsPacketSize*2)
	_, err := muxer.WriteData(videoPESData(payload, 1000))
	require.NoError(t, err)
	require.NoError(t, muxer.Flush())

	raw := buf.Bytes()
	numPackets := len(raw) / M2TSPacketSize
	require.GreaterOrEqual(t, numPackets, 5)

	want := []uint32{
		uint32(base & m2tsATSMask),
		uint32((base + 1) & m2tsATSMask),
		0, // (2^30) & mask
		1,
		2,
	}
	for i, w := range want {
		_, ats := parseM2TSHeader(raw[i*M2TSPacketSize : i*M2TSPacketSize+m2tsHeaderSize])
		assert.Equal(t, w, ats, "packet %d ATS", i)
	}
}

// TestM2TS_ClockBackwards ensures a non-monotonic clock fails explicitly and
// nothing that could be mistaken for a valid record is produced for the bad
// packet.
func TestM2TS_ClockBackwards(t *testing.T) {
	clock := &scriptedClock{ticks: []uint64{1000, 2000, 1500 /* backwards */, 3000}}
	muxer, buf := newAVMuxer(t, WithM2TS(clock, M2TSCopyPermissionCopyFree))

	payload := bytes.Repeat([]byte{0x01}, MpegTsPacketSize*3)
	_, err := muxer.WriteData(videoPESData(payload, 1000))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrM2TSClockBackwards), "got %v", err)

	// Only packets stamped before the backward jump may be present, and every
	// emitted record must still be a well-formed 192-byte packet.
	assert.Equal(t, 0, buf.Len()%M2TSPacketSize)
	raw := buf.Bytes()
	for i := 0; i < len(raw); i += M2TSPacketSize {
		assert.Equal(t, byte(syncByte), raw[i+m2tsHeaderSize])
	}
}

// TestM2TS_InvalidCopyPermission is rejected at construction time before any
// byte is written.
func TestM2TS_InvalidCopyPermission(t *testing.T) {
	buf := &bytes.Buffer{}
	_, err := NewMuxer(context.Background(), buf, WithM2TS(&scriptedClock{ticks: []uint64{1}}, 4))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrM2TSCopyPermissionInvalid), "got %v", err)
	assert.Equal(t, 0, buf.Len())
}

// TestM2TS_CopyPermissionBits verifies each legal indicator lands in the two
// most significant bits of the first header byte.
func TestM2TS_CopyPermissionBits(t *testing.T) {
	for _, cp := range []uint8{
		M2TSCopyPermissionCopyFree,
		M2TSCopyPermissionCopyOnce,
		M2TSCopyPermissionReserved,
		M2TSCopyPermissionNoMoreCopies,
	} {
		var h [m2tsHeaderSize]byte
		encodeM2TSHeader(h[:], cp, 0x12345678)
		assert.Equal(t, cp, h[0]>>6, "cp %d", cp)
		gotCP, gotATS := parseM2TSHeader(h[:])
		assert.Equal(t, cp, gotCP)
		assert.Equal(t, uint32(0x12345678&m2tsATSMask), gotATS)
	}
}

// failingWriter returns a fixed error after accepting wantOK bytes worth of
// full 192-byte records.
type failingWriter struct {
	buf    bytes.Buffer
	wantOK int
	err    error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.buf.Len()/M2TSPacketSize >= f.wantOK {
		return 0, f.err
	}
	return f.buf.Write(p)
}

// shortWriter silently accepts fewer bytes than requested, modelling a broken
// writer that does not return an error.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

// TestM2TS_WriteErrors checks that underlying writer failures and silent short
// writes surface as explicit errors rather than truncated streams.
func TestM2TS_WriteErrors(t *testing.T) {
	boom := errors.New("disk full")

	// Hard error from the underlying writer.
	fw := &failingWriter{wantOK: 1, err: boom}
	muxer, err := NewMuxer(context.Background(), fw, WithM2TS(&scriptedClock{ticks: []uint64{1, 2, 3, 4, 5}}, M2TSCopyPermissionCopyFree))
	require.NoError(t, err)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{ElementaryPID: 0x1234, StreamType: StreamTypeH264Video}))
	muxer.SetPCRPID(0x1234)
	_, werr := muxer.WriteTables()
	require.Error(t, werr)
	assert.True(t, errors.Is(werr, boom), "got %v", werr)

	// Silent short write must be translated into ErrM2TSShortWrite.
	muxer2, err := NewMuxer(context.Background(), shortWriter{}, WithM2TS(&scriptedClock{ticks: []uint64{1, 2, 3}}, M2TSCopyPermissionCopyFree))
	require.NoError(t, err)
	require.NoError(t, muxer2.AddElementaryStream(PMTElementaryStream{ElementaryPID: 0x1234, StreamType: StreamTypeH264Video}))
	muxer2.SetPCRPID(0x1234)
	_, err = muxer2.WriteTables()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrM2TSShortWrite), "got %v", err)
}

// TestM2TS_PacketBoundaryDamage ensures data that is not an integer number of
// 188-byte packets cannot be flushed as if it were a valid stream.
func TestM2TS_PacketBoundaryDamage(t *testing.T) {
	mw := newM2TSWriter(&bytes.Buffer{}, &scriptedClock{ticks: []uint64{1, 2}}, M2TSCopyPermissionCopyFree)

	// Feed a full packet followed by a dangling tail.
	full := make([]byte, MpegTsPacketSize)
	full[0] = syncByte
	_, err := mw.Write(full)
	require.NoError(t, err)
	_, err = mw.Write([]byte{0x01, 0x02, 0x03})
	require.NoError(t, err)
	err = mw.flush()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrM2TSPacketBoundary), "got %v", err)

	// A packet missing its sync byte must be rejected outright.
	mw2 := newM2TSWriter(&bytes.Buffer{}, &scriptedClock{ticks: []uint64{1}}, M2TSCopyPermissionCopyFree)
	bad := make([]byte, MpegTsPacketSize)
	bad[0] = 0x00
	_, err = mw2.Write(bad)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrM2TSSyncByte), "got %v", err)
}

// TestM2TS_ChunkedWrites verifies that writes split into arbitrary,
// non-188-aligned chunks are reassembled into the same 192-byte records as a
// single contiguous write, with one monotonic ATS per packet.
func TestM2TS_ChunkedWrites(t *testing.T) {
	numPackets := 3
	packets := make([]byte, 0, numPackets*MpegTsPacketSize)
	for i := 0; i < numPackets; i++ {
		pkt := make([]byte, MpegTsPacketSize)
		pkt[0] = syncByte
		pkt[1] = byte(i + 1) // distinguish packets
		packets = append(packets, pkt...)
	}
	ticks := []uint64{100, 200, 300}

	// Reference: one contiguous write.
	refBuf := &bytes.Buffer{}
	ref := newM2TSWriter(refBuf, &scriptedClock{ticks: ticks}, M2TSCopyPermissionCopyFree)
	_, err := ref.Write(packets)
	require.NoError(t, err)
	require.NoError(t, ref.flush())

	// Same data delivered in awkward chunk sizes.
	gotBuf := &bytes.Buffer{}
	got := newM2TSWriter(gotBuf, &scriptedClock{ticks: ticks}, M2TSCopyPermissionCopyFree)
	chunks := []int{4, 187, 1, 190, 188, 1} // deliberately straddle boundaries
	off := 0
	for _, sz := range chunks {
		if off >= len(packets) {
			break
		}
		end := off + sz
		if end > len(packets) {
			end = len(packets)
		}
		n, werr := got.Write(packets[off:end])
		require.NoError(t, werr)
		assert.Equal(t, end-off, n)
		off = end
	}
	require.NoError(t, got.flush())
	assert.Equal(t, refBuf.Bytes(), gotBuf.Bytes())
	assert.Equal(t, numPackets*M2TSPacketSize, gotBuf.Len())

	// Each record keeps its own ATS in emission order.
	raw := gotBuf.Bytes()
	for i, wantTick := range ticks {
		_, ats := parseM2TSHeader(raw[i*M2TSPacketSize : i*M2TSPacketSize+m2tsHeaderSize])
		assert.Equal(t, uint32(wantTick), ats)
	}
}

// TestM2TS_DemuxRoundTrip is the audio+video PES fixture check: the demuxer
// auto-detects the 192-byte layout, parses the PAT/PMT and reassembles the
// original PES payloads and PTS values.
func TestM2TS_DemuxRoundTrip(t *testing.T) {
	videoPayload := bytes.Repeat([]byte{0x5a}, 400)
	audioPayload := bytes.Repeat([]byte{0xa5}, 100)
	videoPTS := int64(5726623060)
	audioPTS := videoPTS + 9000

	ticks := make([]uint64, 128)
	for i := range ticks {
		ticks[i] = uint64(i+1) * 90000
	}
	muxer, buf := newAVMuxer(t, WithM2TS(&scriptedClock{ticks: ticks}, M2TSCopyPermissionCopyOnce))
	_, err := muxer.WriteData(videoPESData(videoPayload, videoPTS))
	require.NoError(t, err)
	_, err = muxer.WriteData(audioPESData(audioPayload, audioPTS))
	require.NoError(t, err)
	require.NoError(t, muxer.Flush())
	require.Equal(t, 0, buf.Len()%M2TSPacketSize)

	// Auto-detection must recognise the M2TS layout without an explicit size.
	probe := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()))
	probePkt, err := probe.NextPacket()
	require.NoError(t, err)
	require.NotNil(t, probePkt)
	assert.Equal(t, M2TSPacketSize, probe.packetBuffer.packetSize)

	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()))
	var (
		gotPAT *PATData
		gotPMT *PMTData
		gotPES []*DemuxerData
	)
	for {
		d, derr := dmx.NextData()
		if errors.Is(derr, ErrNoMorePackets) {
			break
		}
		require.NoError(t, derr)
		if d.PAT != nil {
			gotPAT = d.PAT
		}
		if d.PMT != nil {
			gotPMT = d.PMT
		}
		if d.PES != nil {
			gotPES = append(gotPES, d)
		}
	}

	require.NotNil(t, gotPAT)
	require.NotNil(t, gotPMT)
	assert.Len(t, gotPMT.ElementaryStreams, 2)

	// Two reassembled PES streams, payloads and PTS preserved.
	require.Len(t, gotPES, 2)
	got := map[uint16]*DemuxerData{}
	for _, d := range gotPES {
		got[d.PID] = d
	}

	video := got[0x1234]
	require.NotNil(t, video)
	assert.Equal(t, videoPayload, video.PES.Data)
	require.NotNil(t, video.PES.Header.OptionalHeader)
	assert.Equal(t, videoPTS, video.PES.Header.OptionalHeader.PTS.Base)

	audio := got[0x0234]
	require.NotNil(t, audio)
	assert.Equal(t, audioPayload, audio.PES.Data)
	require.NotNil(t, audio.PES.Header.OptionalHeader)
	assert.Equal(t, audioPTS, audio.PES.Header.OptionalHeader.PTS.Base)
}

// TestM2TS_DefaultSystemClockSmokeTest makes sure the nil-clock option yields a
// working, monotonic, 90 kHz-derived clock.
func TestM2TS_DefaultSystemClockSmokeTest(t *testing.T) {
	c := newSystemM2TSClock()
	first := c.NowM2TSTicks()
	second := c.NowM2TSTicks()
	assert.GreaterOrEqual(t, second, first)

	muxer, buf, err := func() (*Muxer, *bytes.Buffer, error) {
		b := &bytes.Buffer{}
		m, e := NewMuxer(context.Background(), b, WithM2TS(nil, M2TSCopyPermissionCopyFree))
		return m, b, e
	}()
	require.NoError(t, err)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{ElementaryPID: 0x1234, StreamType: StreamTypeH264Video}))
	muxer.SetPCRPID(0x1234)
	_, err = muxer.WriteTables()
	require.NoError(t, err)
	require.NoError(t, muxer.Flush())
	assert.Equal(t, 2*M2TSPacketSize, buf.Len())
}

// TestM2TS_HeaderEncodeParseRoundTrip locks the 4-byte header bit layout.
func TestM2TS_HeaderEncodeParseRoundTrip(t *testing.T) {
	cases := []struct {
		cp  uint8
		ats uint32
	}{
		{0, 0},
		{3, m2tsATSMask},
		{1, 0x2aaaaaaa},
		{2, 0x15555555},
	}
	for _, tc := range cases {
		var h [m2tsHeaderSize]byte
		encodeM2TSHeader(h[:], tc.cp, tc.ats)
		cp, ats := parseM2TSHeader(h[:])
		assert.Equal(t, tc.cp, cp)
		assert.Equal(t, tc.ats, ats)
	}
}
