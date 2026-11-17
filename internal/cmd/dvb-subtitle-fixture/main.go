// Command dvb-subtitle-fixture builds a fixed MPEG-TS fixture containing two
// programs carrying DVB bitmap subtitling streams (EN 300 743). The PSI
// tables (PAT/PMT), subtitling descriptors and every DVB subtitling segment
// are authored by hand so the fixture content does not depend on the parsing
// code under test. The PES packets are packetized through the astits Muxer,
// which guarantees the transport packet layout is exactly the one the astits
// Demuxer reassembles; the segment bytes themselves remain fully controlled.
//
// Layout:
//
//	PID 0x0000 PAT          -> program 1 PMT PID 0x1000, program 2 PMT PID 0x1001
//	PID 0x1000 PMT program 1 -> subtitling PID 0x0400 ("eng", pages 0x0200/0x0100)
//	PID 0x1001 PMT program 2 -> subtitling PID 0x0500 ("deu", pages 0x0300/0x0100)
//	PID 0x0400 PES #1 (multiple TS packets) then PES #2
//	PID 0x0500 PES #3
//
// Each subtitling PES contains several segments, including segments on the
// ancillary page which must be filtered out by the extractor.
package main

import (
	"bytes"
	"context"
	"os"

	"github.com/asticode/go-astits"
)

const (
	patPID         = 0x0000
	pmt1PID        = 0x1000
	pmt2PID        = 0x1001
	subPID1        = 0x0400
	subPID2        = 0x0500
	program1Number = 0x0001
	program2Number = 0x0002
	streamTypePES  = 0x06
	descTagSub     = 0x59
	tsPacketSize   = 188
	syncByte       = 0x47
)

func main() {
	var out bytes.Buffer

	// Hand-authored PSI packets.
	out.Write(patPacket())
	out.Write(pmtPacket(pmt1PID, program1Number, subPID1,
		subtitlingDescriptor("eng", 0x10, 0x0200, 0x0100)))
	out.Write(pmtPacket(pmt2PID, program2Number, subPID2,
		subtitlingDescriptor("deu", 0x10, 0x0300, 0x0100)))

	// One Muxer per subtitling PID keeps continuity counters independent and
	// monotonic across the PES packets of each PID. Both write into the shared
	// output buffer; table retransmission is disabled because PAT/PMT packets
	// are hand-authored.
	mux1 := newSubMuxer(&out, subPID1)
	mux2 := newSubMuxer(&out, subPID2)

	// Program 1, PES #1: three composition-page segments plus one ancillary
	// page segment (filtered by the extractor). Large enough to span several
	// TS packets.
	pes1Payload := buildSubtitlePayload([][]byte{
		segment(0x10, 0x0200, repeat(0xa1, 60)),  // page composition
		segment(0x11, 0x0200, repeat(0xa2, 60)),  // region composition
		segment(0x13, 0x0200, repeat(0xa3, 120)), // object data, first
		segment(0x12, 0x0100, repeat(0xa4, 40)),  // ancillary CLUT, filtered
	})
	writeSubPES(mux1, subPID1, 0x123456789, pes1Payload)

	// Program 1, PES #2.
	pes2Payload := buildSubtitlePayload([][]byte{
		segment(0x14, 0x0200, repeat(0xb1, 30)), // object data, last
		segment(0xff, 0x0200, repeat(0xb2, 10)), // end of display set
	})
	writeSubPES(mux1, subPID1, 0x123456fff, pes2Payload)

	// Program 2, PES #3.
	pes3Payload := buildSubtitlePayload([][]byte{
		segment(0x10, 0x0300, repeat(0xc1, 50)),
		segment(0x11, 0x0300, repeat(0xc2, 50)),
		segment(0x12, 0x0100, repeat(0xc3, 20)), // ancillary CLUT, filtered
	})
	writeSubPES(mux2, subPID2, 0x03456789a, pes3Payload)

	if err := os.WriteFile(outputPath(), out.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

func outputPath() string {
	if len(os.Args) > 1 {
		return os.Args[1]
	}
	return "testdata/ts/dvb_subtitles.ts"
}

// newSubMuxer creates a Muxer configured for one private-data subtitling PID.
// Tables retransmission is disabled because PAT/PMT packets are hand-authored.
func newSubMuxer(out *bytes.Buffer, subPID uint16) *astits.Muxer {
	mx := astits.NewMuxer(context.Background(), out,
		astits.MuxerOptTablesRetransmitPeriod(1<<30),
		astits.WithPMTPID(pmt1PID))
	if err := mx.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: subPID,
		StreamType:    astits.StreamType(streamTypePES),
	}); err != nil {
		panic(err)
	}
	mx.SetPCRPID(subPID)
	return mx
}

// writeSubPES packetizes one subtitling PES through the Muxer. The Muxer only
// emits subtitling-PID packets because table retransmission is disabled.
func writeSubPES(mx *astits.Muxer, pid uint16, pts int64, payload []byte) {
	if _, err := mx.WriteData(&astits.MuxerData{
		PID: pid,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				StreamID: 0xbd,
				OptionalHeader: &astits.PESOptionalHeader{
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: pts},
				},
			},
			Data: payload,
		},
	}); err != nil {
		panic(err)
	}
}

func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// segment builds one DVB subtitling segment (sync byte, segment type, page id,
// segment length and the raw segment body).
func segment(segmentType, pageID uint16, body []byte) []byte {
	out := []byte{
		0x0f,
		byte(segmentType),
		byte(pageID >> 8),
		byte(pageID),
		byte(len(body) >> 8),
		byte(len(body)),
	}
	return append(out, body...)
}

// buildSubtitlePayload prefixes segments with the data_identifier byte.
func buildSubtitlePayload(segments [][]byte) []byte {
	out := []byte{0x20}
	for _, s := range segments {
		out = append(out, s...)
	}
	return out
}

// subtitlingDescriptor builds a subtitling descriptor (EN 300 468 6.2.41)
// with one entry.
func subtitlingDescriptor(language string, subType uint8, compositionPage, ancillaryPage uint16) []byte {
	body := append([]byte(language)[:3],
		subType,
		byte(compositionPage>>8), byte(compositionPage),
		byte(ancillaryPage>>8), byte(ancillaryPage),
	)
	return append([]byte{descTagSub, byte(len(body))}, body...)
}

// patPacket builds a one-packet PAT section (table id 0x00).
func patPacket() []byte {
	section := []byte{
		0x00,
		0xb0, 0x11,
		0x00, 0x01, // transport stream id
		0xc1, 0x00, 0x00,
	}
	section = append(section, programEntry(program1Number, pmt1PID)...)
	section = append(section, programEntry(program2Number, pmt2PID)...)
	section = append(section, crc32MPEG2(section)...)
	return psiPacket(patPID, 0, section)
}

func programEntry(programNumber, pid uint16) []byte {
	return []byte{
		byte(programNumber >> 8), byte(programNumber),
		0xe0 | byte(pid>>8), byte(pid),
	}
}

// pmtPacket builds a one-packet PMT section with one subtitling elementary
// stream carrying the provided descriptor bytes.
func pmtPacket(pid, programNumber, subPID uint16, descriptor []byte) []byte {
	esInfoLength := len(descriptor)
	esLoop := []byte{
		streamTypePES,
		0xe0 | byte(subPID>>8), byte(subPID),
		byte(esInfoLength >> 8), byte(esInfoLength),
	}
	esLoop = append(esLoop, descriptor...)

	sectionLength := 5 + 4 + len(esLoop) + 4
	section := []byte{
		0x02,
		0xb0 | byte(sectionLength>>8), byte(sectionLength),
		byte(programNumber >> 8), byte(programNumber),
		0xc1, 0x00, 0x00,
		0xe0 | byte(subPID>>8), byte(subPID),
		0xf0, 0x00,
	}
	section = append(section, esLoop...)
	section = append(section, crc32MPEG2(section)...)
	return psiPacket(pid, 0, section)
}

// psiPacket wraps a PSI section in a single TS packet with pointer field 0
// and 0xff trailing stuffing.
func psiPacket(pid, cc uint16, section []byte) []byte {
	payload := append([]byte{0x00}, section...)
	p := make([]byte, tsPacketSize)
	for i := range p {
		p[i] = 0xff
	}
	p[0] = syncByte
	p[1] = 0x40 | byte(pid>>8) // payload unit start indicator
	p[2] = byte(pid)
	p[3] = 0x10 | byte(cc&0x0f) // payload only
	copy(p[4:], payload)
	return p
}

// crc32MPEG2 computes the MPEG-2 CRC32 used by PSI sections.
func crc32MPEG2(data []byte) []byte {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return []byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)}
}
