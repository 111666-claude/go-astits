package astits

import (
	"errors"
	"fmt"
	"time"

	"github.com/asticode/go-astikit"
)

// PIDTDT is the PID carrying the Time and Date Table (and, when present, the Time Offset Table).
// EN 300 468, section 5.1.3.
const PIDTDT uint16 = 0x14

// tdtSectionLength is the only valid section_length of a TDT section: 5 bytes of UTC time.
const tdtSectionLength = 5

// ErrTDTSectionInvalid is returned when a TDT section is truncated or has an unexpected length.
var ErrTDTSectionInvalid = errors.New("astits: invalid TDT section")

// TDTData represents a Time and Date Table data
// EN 300 468, section 5.2.5: the TDT section only contains a 5-byte UTC time coded as a 16-bit
// MJD date followed by BCD coded hours, minutes and seconds.
type TDTData struct {
	UTCTime time.Time
}

// parseTDTSection parses a TDT section.
// h provides the already parsed section header so that a truncated or wrongly sized section is
// reported explicitly instead of being silently consumed.
func parseTDTSection(i *astikit.BytesIterator, h *PSISectionHeader) (d *TDTData, err error) {
	if h.SectionLength != tdtSectionLength {
		return nil, fmt.Errorf("%w: section_length is %d, expected %d", ErrTDTSectionInvalid, h.SectionLength, tdtSectionLength)
	}

	d = &TDTData{}
	if d.UTCTime, err = parseDVBTimeStrict(i); err != nil {
		err = fmt.Errorf("astits: parsing TDT UTC time failed: %w", err)
		return
	}
	return d, nil
}

// writeTDTSection writes the 5-byte UTC time payload of a TDT section. The table_id and section
// header are written by writePSISection.
func writeTDTSection(w *astikit.BitsWriter, d *TDTData) (int, error) {
	return writeDVBTimeStrict(w, d.UTCTime)
}
