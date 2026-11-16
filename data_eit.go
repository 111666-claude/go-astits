package astits

import (
	"fmt"
	"time"

	"github.com/asticode/go-astikit"
)

// EITData represents an EIT data
// Page: 36 | Chapter: 5.2.4 | Link: https://www.dvb.org/resources/public/standards/a38_dvb-si_specification.pdf
// (barbashov) the link above can be broken, alternative: https://dvb.org/wp-content/uploads/2019/12/a038_tm1217r37_en300468v1_17_1_-_rev-134_-_si_specification.pdf
type EITData struct {
	Events                   []*EITDataEvent
	LastTableID              uint8
	OriginalNetworkID        uint16
	SegmentLastSectionNumber uint8
	ServiceID                uint16
	TransportStreamID        uint16
}

// EITDataEvent represents an EIT data event
type EITDataEvent struct {
	Descriptors    []*Descriptor
	Duration       time.Duration
	EventID        uint16
	HasFreeCSAMode bool // When true indicates that access to one or more streams may be controlled by a CA system.
	RunningStatus  uint8
	StartTime      time.Time
}

const (
	eitSectionHeaderBytesSize = 6  // transport_stream_id (16) + original_network_id (16) + segment_last_section_number (8) + last_table_id (8)
	eitEventHeaderBytesSize   = 12 // event_id (16) + start_time (40) + duration (24) + running_status/free_CA_mode/descriptors_loop_length (16)
)

// parseEITSection parses an EIT section
func parseEITSection(i *astikit.BytesIterator, offsetSectionsEnd int, tableIDExtension uint16) (d *EITData, err error) {
	// Create data
	d = &EITData{ServiceID: tableIDExtension}

	// Get next 2 bytes
	var bs []byte
	if bs, err = i.NextBytesNoCopy(2); err != nil {
		err = fmt.Errorf("astits: fetching next bytes failed: %w", err)
		return
	}

	// Transport stream ID
	d.TransportStreamID = uint16(bs[0])<<8 | uint16(bs[1])

	// Get next 2 bytes
	if bs, err = i.NextBytesNoCopy(2); err != nil {
		err = fmt.Errorf("astits: fetching next bytes failed: %w", err)
		return
	}

	// Original network ID
	d.OriginalNetworkID = uint16(bs[0])<<8 | uint16(bs[1])

	// Get next byte
	var b byte
	if b, err = i.NextByte(); err != nil {
		err = fmt.Errorf("astits: fetching next byte failed: %w", err)
		return
	}

	// Segment last section number
	d.SegmentLastSectionNumber = uint8(b)

	// Get next byte
	if b, err = i.NextByte(); err != nil {
		err = fmt.Errorf("astits: fetching next byte failed: %w", err)
		return
	}

	// Last table ID
	d.LastTableID = uint8(b)

	// Loop until end of section data is reached
	for i.Offset() < offsetSectionsEnd {
		// Get next 2 bytes
		if bs, err = i.NextBytesNoCopy(2); err != nil {
			err = fmt.Errorf("astits: fetching next bytes failed: %w", err)
			return
		}

		// Event ID
		var e = &EITDataEvent{}
		e.EventID = uint16(bs[0])<<8 | uint16(bs[1])

		// Start time
		if e.StartTime, err = parseDVBTime(i); err != nil {
			err = fmt.Errorf("astits: parsing DVB time")
			return
		}

		// Duration
		if e.Duration, err = parseDVBDurationSeconds(i); err != nil {
			err = fmt.Errorf("astits: parsing DVB duration seconds failed: %w", err)
			return
		}

		// Get next byte
		if b, err = i.NextByte(); err != nil {
			err = fmt.Errorf("astits: fetching next byte failed: %w", err)
			return
		}

		// Running status
		e.RunningStatus = uint8(b) >> 5

		// Free CA mode
		e.HasFreeCSAMode = uint8(b&0x10) > 0

		// We need to rewind since the current byte is used by the descriptor as well
		i.Skip(-1)

		// Descriptors
		if e.Descriptors, err = parseDescriptors(i); err != nil {
			err = fmt.Errorf("astits: parsing descriptors failed: %w", err)
			return
		}

		// Add event
		d.Events = append(d.Events, e)
	}
	return
}

// calcEITSectionLength returns the length of the EIT section data, i.e. everything that
// follows the PSI syntax header and precedes the CRC32.
func calcEITSectionLength(d *EITData) uint16 {
	ret := uint16(eitSectionHeaderBytesSize)
	for _, e := range d.Events {
		ret += eitEventHeaderBytesSize
		ret += calcDescriptorsLength(e.Descriptors)
	}
	return ret
}

// writeEITSection writes an EIT section data (everything that follows the PSI syntax header
// and precedes the CRC32).
func writeEITSection(w *astikit.BitsWriter, d *EITData) (int, error) {
	b := astikit.NewBitsWriterBatch(w)

	// Section data header
	b.Write(d.TransportStreamID)
	b.Write(d.OriginalNetworkID)
	b.Write(d.SegmentLastSectionNumber)
	b.Write(d.LastTableID)
	bytesWritten := eitSectionHeaderBytesSize

	// Events
	for _, e := range d.Events {
		b.Write(e.EventID)
		bytesWritten += 2

		n, err := writeDVBTime(w, e.StartTime)
		if err != nil {
			return 0, fmt.Errorf("astits: writing DVB time failed: %w", err)
		}
		bytesWritten += n

		n, err = writeDVBDurationSeconds(w, e.Duration)
		if err != nil {
			return 0, fmt.Errorf("astits: writing DVB duration failed: %w", err)
		}
		bytesWritten += n

		b.WriteN(e.RunningStatus, 3)
		b.Write(e.HasFreeCSAMode)
		b.WriteN(calcDescriptorsLength(e.Descriptors), 12)
		bytesWritten += 2

		if n, err = writeDescriptors(w, e.Descriptors); err != nil {
			return 0, err
		}
		bytesWritten += n
	}

	return bytesWritten, b.Err()
}
