package astits

import (
	"bytes"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
)

func TestAutoDetectPacketSize(t *testing.T) {
	// Packet should start with a sync byte
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write(uint8(2))
	w.Write(byte(syncByte))
	_, err := autoDetectPacketSize(bytes.NewReader(buf.Bytes()))
	assert.EqualError(t, err, ErrPacketMustStartWithASyncByte.Error())

	// Valid packet size
	buf.Reset()
	w.Write(byte(syncByte))
	w.Write(make([]byte, 20))
	w.Write(byte(syncByte))
	w.Write(make([]byte, 166))
	w.Write(byte(syncByte))
	w.Write(make([]byte, 187))
	w.Write([]byte("test"))
	r := bytes.NewReader(buf.Bytes())
	p, err := autoDetectPacketSize(r)
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, p)
	assert.Equal(t, 380, r.Len())

	// Valid M2TS: 4-byte BDAV header, then a sync byte, repeated every 192 bytes.
	m2tsBuf := &bytes.Buffer{}
	mw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m2tsBuf})
	for i := 0; i < 2; i++ {
		var hdr [m2tsHeaderSize]byte
		encodeM2TSHeader(hdr[:], M2TSCopyPermissionCopyFree, uint32(90000*(i+1)))
		mw.Write(hdr[:])
		mw.Write(byte(syncByte))
		mw.Write(make([]byte, MpegTsPacketSize-1))
	}
	r = bytes.NewReader(m2tsBuf.Bytes())
	p, err = autoDetectPacketSize(r)
	assert.NoError(t, err)
	assert.Equal(t, M2TSPacketSize, p)
	assert.Equal(t, 2*M2TSPacketSize, r.Len())
}

// TestAutoDetectPacketSizeM2TSHeaderContainingSync proves detection still
// works when an ATS value puts a 0x47 byte inside the 4-byte BDAV header,
// which would fool a naive "first sync byte" scan.
func TestAutoDetectPacketSizeM2TSHeaderContainingSync(t *testing.T) {
	atsValues := []uint32{
		0x12470000 & m2tsATSMask, // header byte 1 becomes 0x47
		0x34560047 & m2tsATSMask, // header byte 3 becomes 0x47
	}
	m2tsBuf := &bytes.Buffer{}
	mw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m2tsBuf})
	for _, ats := range atsValues {
		var hdr [m2tsHeaderSize]byte
		encodeM2TSHeader(hdr[:], M2TSCopyPermissionCopyFree, ats)
		mw.Write(hdr[:])
		mw.Write(byte(syncByte))
		mw.Write(make([]byte, MpegTsPacketSize-1))
	}

	r := bytes.NewReader(m2tsBuf.Bytes())
	p, err := autoDetectPacketSize(r)
	assert.NoError(t, err)
	assert.Equal(t, M2TSPacketSize, p)
}

// TestParsePacketM2TS ensures a real BDAV record (4-byte header first, sync
// byte at offset 4) parses into the embedded 188-byte TS packet.
func TestParsePacketM2TS(t *testing.T) {
	// Build a plain 188-byte packet first.
	tsPacket, expected := packet(packetHeader, *packetAdaptationField, []byte("payload"), false)
	assert.Len(t, tsPacket, MpegTsPacketSize)

	// Prepend a BDAV header to obtain a 192-byte record.
	record := make([]byte, 0, M2TSPacketSize)
	var hdr [m2tsHeaderSize]byte
	encodeM2TSHeader(hdr[:], M2TSCopyPermissionNoMoreCopies, 0x3deadbee&m2tsATSMask)
	record = append(record, hdr[:]...)
	record = append(record, tsPacket...)
	assert.Len(t, record, M2TSPacketSize)

	got, err := parsePacket(astikit.NewBytesIterator(record), nil)
	assert.NoError(t, err)
	assert.Equal(t, expected, got)

	// A 192-byte buffer whose sync byte is neither at offset 0 nor 4 fails.
	junk := make([]byte, M2TSPacketSize)
	junk[10] = syncByte
	_, err = parsePacket(astikit.NewBytesIterator(junk), nil)
	assert.EqualError(t, err, ErrPacketMustStartWithASyncByte.Error())
}
