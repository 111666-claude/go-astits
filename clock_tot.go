package astits

import (
	"errors"
	"time"
)

// TOT clock errors
var (
	ErrTOTClockRegionNotFound = errors.New("astits: tot clock region not found")
	ErrTOTClockTimeOverflow   = errors.New("astits: tot clock local time overflow")
)

// maxTOTClockOffset is the maximum absolute local time offset considered valid. Offsets are
// BCD-encoded in the stream and can therefore advertise nonsensical values (up to 99h99) that
// no real timezone uses.
const maxTOTClockOffset = 24 * time.Hour

// TOTClock resolves local times from the local time offset descriptors carried by TOT data.
// It is not safe for concurrent use.
type TOTClock struct {
	regions map[totClockRegionKey]*totClockRegion
}

// totClockRegionKey identifies a region by its 3-letter country code and its country region id
type totClockRegionKey struct {
	countryCode string
	regionID    uint8
}

// totClockRegion holds the offsets of a region. Offsets are signed, the polarity has already
// been applied.
type totClockRegion struct {
	currentOffset time.Duration
	nextOffset    time.Duration
	timeOfChange  time.Time
}

// NewTOTClock creates a new TOT clock
func NewTOTClock() *TOTClock {
	return &TOTClock{regions: make(map[totClockRegionKey]*totClockRegion)}
}

// Update consumes TOT data and refreshes the known local time offsets. Items are processed in
// descriptor order with the following deterministic rules:
//   - items with an invalid country code or an offset bigger than maxTOTClockOffset are skipped
//   - the first item of a region wins, duplicates within the same update are skipped
//   - an item whose time of change is earlier than the one already stored for the region is
//     considered stale and skipped
func (c *TOTClock) Update(d *TOTData) {
	// Nothing to do
	if d == nil {
		return
	}

	// Loop through descriptors
	seen := make(map[totClockRegionKey]bool)
	for _, desc := range d.Descriptors {
		if desc == nil || desc.LocalTimeOffset == nil {
			continue
		}

		// Loop through items
		for _, item := range desc.LocalTimeOffset.Items {
			// Invalid country code
			if item == nil || len(item.CountryCode) != 3 {
				continue
			}
			key := totClockRegionKey{countryCode: string(item.CountryCode), regionID: item.CountryRegionID}

			// Duplicate within this update: first one wins
			if seen[key] {
				continue
			}
			seen[key] = true

			// Invalid offsets
			if item.LocalTimeOffset > maxTOTClockOffset || item.NextTimeOffset > maxTOTClockOffset {
				continue
			}

			// Stale time of change
			if r, ok := c.regions[key]; ok && item.TimeOfChange.Before(r.timeOfChange) {
				continue
			}

			// Store region
			c.regions[key] = &totClockRegion{
				currentOffset: totClockSignedOffset(item.LocalTimeOffset, item.LocalTimeOffsetPolarity),
				nextOffset:    totClockSignedOffset(item.NextTimeOffset, item.LocalTimeOffsetPolarity),
				timeOfChange:  item.TimeOfChange,
			}
		}
	}
}

// Offset returns the effective local time offset of a region at the given UTC time. The next
// offset applies from the time of change on.
func (c *TOTClock) Offset(countryCode string, regionID uint8, t time.Time) (time.Duration, error) {
	// Unknown region
	r, ok := c.regions[totClockRegionKey{countryCode: countryCode, regionID: regionID}]
	if !ok {
		return 0, ErrTOTClockRegionNotFound
	}

	// Switch to the next offset once the time of change has been reached
	if !r.timeOfChange.IsZero() && !t.Before(r.timeOfChange) {
		return r.nextOffset, nil
	}
	return r.currentOffset, nil
}

// LocalTime returns the local time of a region at the given UTC time
func (c *TOTClock) LocalTime(countryCode string, regionID uint8, t time.Time) (time.Time, error) {
	// Get offset
	o, err := c.Offset(countryCode, regionID, t)
	if err != nil {
		return time.Time{}, err
	}

	// Apply offset, guarding against overflows. time.Time.Add saturates on overflow, therefore
	// the round trip difference no longer matches the offset in that case.
	l := t.Add(o)
	if l.Sub(t) != o {
		return time.Time{}, ErrTOTClockTimeOverflow
	}
	return l, nil
}

// totClockSignedOffset applies the polarity to an offset: a set polarity means the local time
// is behind UTC
func totClockSignedOffset(d time.Duration, polarity bool) time.Duration {
	if polarity {
		return -d
	}
	return d
}
