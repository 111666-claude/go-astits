package astits

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Blu-ray Disc Audio/Visual (BDAV) transport stream, a.k.a. M2TS, prepends a
// 4-byte header to every 188-byte MPEG-TS packet, giving a constant on-disk
// packet size of 192 bytes.
//
// The 4-byte header is laid out as:
//
//	bit 31 ... 30        copy_permission_indicator (2 bits)
//	bit 29 ...  0        arrival_time_stamp, ATS   (30 bits)
//
// The ATS is a free-running counter on a 90 kHz time base (the same time base
// as PTS/DTS), i.e. one tick is 1/90000 s. It wraps modulo 2^30.
// See "System Description Blu-ray Disc Rewritable Format", part 3, and the
// equivalent m2ts handling in FFmpeg (libavformat/mpegtsenc.c).
const (
	// M2TSPacketSize is the size of an M2TS (BDAV) record: 4-byte header plus
	// a full 188-byte MPEG-TS packet.
	M2TSPacketSize = MpegTsPacketSize + 4

	// m2tsHeaderSize is the size, in bytes, of the BDAV extra header.
	m2tsHeaderSize = 4

	// m2tsATSWrap is the modulus of the 30-bit arrival time stamp.
	m2tsATSWrap = 1 << 30

	// m2tsATSMask keeps the lowest 30 bits of a counter.
	m2tsATSMask = m2tsATSWrap - 1
)

// Copy permission indicator values carried by the 2 most significant bits of
// the BDAV header. Only the four combinations below are legal.
const (
	// M2TSCopyPermissionCopyFree is the Blu-ray default ("copy free").
	M2TSCopyPermissionCopyFree uint8 = 0
	// M2TSCopyPermissionCopyOnce allows a single generation of copies.
	M2TSCopyPermissionCopyOnce uint8 = 1
	// M2TSCopyPermissionReserved is reserved and must not be emitted.
	M2TSCopyPermissionReserved uint8 = 2
	// M2TSCopyPermissionNoMoreCopies forbids further copies.
	M2TSCopyPermissionNoMoreCopies uint8 = 3
)

// Errors returned by the M2TS writer. They are deliberately distinct from the
// plain-TS errors so that callers can tell a corrupted M2TS stream from a
// regular MPEG-TS failure.
var (
	// ErrM2TSClockBackwards is returned when the injected clock produces a
	// tick smaller than the previous one. The ATS field wraps deterministically
	// on its own, so a real clock must be monotonic.
	ErrM2TSClockBackwards = errors.New("astits: m2ts clock must be monotonic but went backwards")
	// ErrM2TSCopyPermissionInvalid is returned when a copy permission
	// indicator outside the 0..3 range is configured.
	ErrM2TSCopyPermissionInvalid = errors.New("astits: m2ts copy permission indicator must be in the range 0..3")
	// ErrM2TSPacketBoundary is returned when data handed to the writer does
	// not align on an 188-byte MPEG-TS packet boundary.
	ErrM2TSPacketBoundary = errors.New("astits: m2ts data does not align on an 188-byte packet boundary")
	// ErrM2TSSyncByte is returned when an MPEG-TS packet handed to the writer
	// does not start with the 0x47 sync byte.
	ErrM2TSSyncByte = errors.New("astits: m2ts packet must start with the 0x47 sync byte")
	// ErrM2TSShortWrite is returned when the underlying writer accepts fewer
	// bytes than a full 192-byte record without reporting an error.
	ErrM2TSShortWrite = errors.New("astits: m2ts underlying writer wrote fewer bytes than requested")
)

// M2TSClock provides arrival time stamps for M2TS packets.
//
// NowM2TSTicks returns the current value of a free-running, monotonic counter
// expressed in 90 kHz units. The implementation may return any uint64 value,
// including values above 2^30: the muxer truncates each value to the lowest
// 30 bits when encoding the on-disk ATS, so wrapping is deterministic.
//
// Consecutive calls MUST return values that are equal or increasing. A
// strictly smaller value makes the muxer fail with ErrM2TSClockBackwards,
// because a backward jump cannot be distinguished from a corrupted clock.
type M2TSClock interface {
	NowM2TSTicks() uint64
}

// systemM2TSClock is the default M2TSClock. It derives 90 kHz ticks from the
// monotonic wall clock. The first tick is used as the epoch so that streams
// start close to ATS 0 regardless of the host uptime.
type systemM2TSClock struct {
	start time.Time
}

func newSystemM2TSClock() *systemM2TSClock {
	return &systemM2TSClock{start: time.Now()}
}

// NowM2TSTicks implements the M2TSClock interface.
func (c *systemM2TSClock) NowM2TSTicks() uint64 {
	return uint64(time.Since(c.start) / (time.Second / 90000))
}

// m2tsWriter wraps an io.Writer and turns the stream of 188-byte MPEG-TS
// packets emitted by the muxer into 192-byte BDAV records.
//
// It never emits a partial record: every successful Write of complete packets
// results in an integer number of full 192-byte records written to the
// underlying writer. Any failure leaves the caller with an explicit error and
// no ambiguity about the position of the next packet boundary.
type m2tsWriter struct {
	w         io.Writer
	clock     M2TSClock
	copyPerm  uint8
	lastTick  uint64
	haveTick  bool
	headerBuf [m2tsHeaderSize]byte
	packetBuf []byte // buffered tail when Write sizes are not a multiple of 188
}

func newM2TSWriter(w io.Writer, clock M2TSClock, copyPermission uint8) *m2tsWriter {
	return &m2tsWriter{
		w:        w,
		clock:    clock,
		copyPerm: copyPermission,
	}
}

// Write implements io.Writer. p must contain an integer number of 188-byte
// MPEG-TS packets, each starting with the 0x47 sync byte; it may carry a
// trailing partial packet which is buffered until the rest arrives.
//
// On success it reports len(p) as consumed; a failure returns 0 together with
// the error so that callers cannot mistake a partially processed buffer for a
// completed write.
func (mw *m2tsWriter) Write(p []byte) (int, error) {
	consumed := len(p)
	// Fold in any bytes buffered from a previous call.
	if len(mw.packetBuf) > 0 {
		need := MpegTsPacketSize - len(mw.packetBuf)
		if len(p) < need {
			mw.packetBuf = append(mw.packetBuf, p...)
			return consumed, nil
		}
		mw.packetBuf = append(mw.packetBuf, p[:need]...)
		p = p[need:]
		if err := mw.emitPacket(mw.packetBuf); err != nil {
			return 0, err
		}
		mw.packetBuf = mw.packetBuf[:0]
	}

	// Emit every full packet.
	for len(p) >= MpegTsPacketSize {
		if err := mw.emitPacket(p[:MpegTsPacketSize]); err != nil {
			return 0, err
		}
		p = p[MpegTsPacketSize:]
	}

	// Buffer a trailing partial packet.
	if len(p) > 0 {
		mw.packetBuf = append(mw.packetBuf, p...)
	}
	return consumed, nil
}

// emitPacket validates one 188-byte packet, stamps it and writes the resulting
// 192-byte record atomically.
func (mw *m2tsWriter) emitPacket(pkt []byte) error {
	if len(pkt) != MpegTsPacketSize {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrM2TSPacketBoundary, len(pkt), MpegTsPacketSize)
	}
	if pkt[0] != syncByte {
		return fmt.Errorf("%w: got 0x%02x", ErrM2TSSyncByte, pkt[0])
	}

	tick, err := mw.nextTick()
	if err != nil {
		return err
	}
	encodeM2TSHeader(mw.headerBuf[:], mw.copyPerm, tick)

	if err := writeFull(mw.w, mw.headerBuf[:]); err != nil {
		return err
	}
	if err := writeFull(mw.w, pkt); err != nil {
		return err
	}
	return nil
}

// nextTick fetches the clock, enforces monotonicity and returns the 30-bit ATS.
func (mw *m2tsWriter) nextTick() (uint32, error) {
	tick := mw.clock.NowM2TSTicks()
	if mw.haveTick && tick < mw.lastTick {
		return 0, fmt.Errorf("%w: %d < %d", ErrM2TSClockBackwards, tick, mw.lastTick)
	}
	mw.lastTick = tick
	mw.haveTick = true
	return uint32(tick & m2tsATSMask), nil
}

// flush must be called once no more data will be written. It reports any
// bytes that did not complete a final 188-byte packet.
func (mw *m2tsWriter) flush() error {
	if len(mw.packetBuf) > 0 {
		return fmt.Errorf("%w: %d trailing byte(s) after last complete packet", ErrM2TSPacketBoundary, len(mw.packetBuf))
	}
	return nil
}

// encodeM2TSHeader writes the 4-byte BDAV header into dst.
//
// The two copy permission bits occupy the top of the first byte, the 30-bit
// arrival time stamp fills the remaining bits in big-endian order.
func encodeM2TSHeader(dst []byte, copyPermission uint8, ats uint32) {
	word := uint32(copyPermission&0x03)<<30 | (ats & m2tsATSMask)
	dst[0] = byte(word >> 24)
	dst[1] = byte(word >> 16)
	dst[2] = byte(word >> 8)
	dst[3] = byte(word)
}

// parseM2TSHeader decodes a 4-byte BDAV header into its copy permission
// indicator and arrival time stamp.
func parseM2TSHeader(src []byte) (copyPermission uint8, ats uint32) {
	word := uint32(src[0])<<24 | uint32(src[1])<<16 | uint32(src[2])<<8 | uint32(src[3])
	copyPermission = uint8(word >> 30)
	ats = word & m2tsATSMask
	return
}

// writeFull writes every byte of p to w, translating a silent short write
// into ErrM2TSShortWrite so that a corrupted stream can never look valid.
func writeFull(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return fmt.Errorf("%w: wrote %d of %d bytes", ErrM2TSShortWrite, n, len(p))
	}
	return nil
}
