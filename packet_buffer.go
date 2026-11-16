package astits

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astikit"
)

// packetBuffer represents a packet buffer
type packetBuffer struct {
	packetSize       int
	s                PacketSkipper
	r                io.Reader
	packetReadBuffer []byte
}

// newPacketBuffer creates a new packet buffer
func newPacketBuffer(r io.Reader, packetSize int, s PacketSkipper) (pb *packetBuffer, err error) {
	// Init
	pb = &packetBuffer{
		packetSize: packetSize,
		s:          s,
		r:          r,
	}

	// Packet size is not set
	if pb.packetSize == 0 {
		// Auto detect packet size
		if pb.packetSize, err = autoDetectPacketSize(r); err != nil {
			err = fmt.Errorf("astits: auto detecting packet size failed: %w", err)
			return
		}
	}
	return
}

// autoDetectPacketSize updates the packet size based on the first bytes
// Minimum packet size is 188 and is bounded by 2 sync bytes.
//
// Two fixed layouts are recognised by their sync-byte positions: plain
// MPEG-TS (sync at offsets 0 and 188) and M2TS/BDAV, whose 4-byte extra
// header puts the sync bytes at offsets 4 and 196. The fixed check runs
// first because an M2TS header may itself contain a 0x47 byte, which would
// confuse a naive "first sync byte" scan. Data matching neither layout falls
// back to the historical scan.
func autoDetectPacketSize(r io.Reader) (packetSize int, err error) {
	// Read first bytes. We only need index 196 to see both the second plain-TS
	// sync byte (offset 188) and the second M2TS sync byte (offset 196).
	const l = 197
	var b = make([]byte, l)
	shouldRewind, rerr := peek(r, b)
	if rerr != nil {
		err = fmt.Errorf("astits: reading first %d bytes failed: %w", l, rerr)
		return
	}

	// Fixed-layout detection: two sync bytes at the expected spacing prove the
	// packet size without relying on what the other bytes contain.
	switch {
	case len(b) > 196 && b[4] == syncByte && b[196] == syncByte:
		packetSize = M2TSPacketSize
	case len(b) > 188 && b[0] == syncByte && b[188] == syncByte:
		packetSize = MpegTsPacketSize
	default:
		// Legacy heuristic for data that does not expose two full records.
		return autoDetectPacketSizeLegacy(b, shouldRewind, r)
	}

	if err = syncReaderAfterPeek(r, shouldRewind, packetSize, l); err != nil {
		return 0, err
	}
	return packetSize, nil
}

// autoDetectPacketSizeLegacy keeps the original single-sync scan for inputs
// that do not contain two full records (short readers, unusual alignments).
func autoDetectPacketSizeLegacy(b []byte, shouldRewind bool, r io.Reader) (packetSize int, err error) {
	// Locate the first sync byte. Plain MPEG-TS keeps it at index 0; an M2TS
	// header pushes it to index 4.
	firstSync := -1
	for idx, v := range b {
		if v == syncByte {
			firstSync = idx
			break
		}
	}
	if firstSync != 0 && firstSync != m2tsHeaderSize {
		// Keep the historical error for unaligned data.
		err = ErrPacketMustStartWithASyncByte
		return
	}

	// Look for the next sync byte to derive the packet size.
	for idx := firstSync + MpegTsPacketSize; idx < len(b); idx++ {
		if b[idx] != syncByte {
			continue
		}
		packetSize = idx - firstSync
		if packetSize != MpegTsPacketSize && packetSize != M2TSPacketSize {
			err = fmt.Errorf("astits: unsupported packet size %d", packetSize)
			return
		}
		if err = syncReaderAfterPeek(r, shouldRewind, packetSize, len(b)); err != nil {
			return 0, err
		}
		return packetSize, nil
	}
	err = fmt.Errorf("astits: only one sync byte detected in first %d bytes", len(b))
	return
}

// syncReaderAfterPeek restores the reader position after peeking: rewind it
// when supported, otherwise skip past the bytes the peek consumed.
func syncReaderAfterPeek(r io.Reader, shouldRewind bool, packetSize, peekLen int) error {
	if !shouldRewind {
		return nil
	}
	n, err := rewind(r)
	if err != nil {
		return fmt.Errorf("astits: rewinding failed: %w", err)
	}
	if n != -1 {
		return nil
	}
	ls := packetSize - (peekLen - packetSize)
	if _, err = r.Read(make([]byte, ls)); err != nil {
		return fmt.Errorf("astits: reading %d bytes to sync reader failed: %w", ls, err)
	}
	return nil
}

// bufio.Reader can't be rewinded, which leads to packet loss on packet size autodetection
// but it has handy Peek() method
// so what we do here is peeking bytes for bufio.Reader and falling back to rewinding/syncing for all other readers
func peek(r io.Reader, b []byte) (shouldRewind bool, err error) {
	if br, ok := r.(*bufio.Reader); ok {
		var bs []byte
		bs, err = br.Peek(len(b))
		if err != nil {
			return
		}
		copy(b, bs)
		return false, nil
	}

	_, err = r.Read(b)
	shouldRewind = true
	return
}

// rewind rewinds the reader if possible, otherwise n = -1
func rewind(r io.Reader) (n int64, err error) {
	if s, ok := r.(io.Seeker); ok {
		if n, err = s.Seek(0, 0); err != nil {
			err = fmt.Errorf("astits: seeking to 0 failed: %w", err)
			return
		}
		return
	}
	n = -1
	return
}

// next fetches the next packet from the buffer
func (pb *packetBuffer) next() (p *Packet, err error) {
	// Read
	if pb.packetReadBuffer == nil || len(pb.packetReadBuffer) != pb.packetSize {
		pb.packetReadBuffer = make([]byte, pb.packetSize)
	}

	// Loop to make sure we return a packet even if first packets are skipped
	for p == nil {
		if _, err = io.ReadFull(pb.r, pb.packetReadBuffer); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				err = ErrNoMorePackets
			} else {
				err = fmt.Errorf("astits: reading %d bytes failed: %w", pb.packetSize, err)
			}
			return
		}

		// Parse packet
		if p, err = parsePacket(astikit.NewBytesIterator(pb.packetReadBuffer), pb.s); err != nil {
			if !errors.Is(err, errSkippedPacket) {
				err = fmt.Errorf("astits: building packet failed: %w", err)
				return
			}
		}
	}

	return
}
