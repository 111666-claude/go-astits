package astits

import (
	"sort"
	"time"
)

// EIT aggregation defaults
const (
	// eitVersionModulus is the modulus of the 5-bit PSI version number
	eitVersionModulus = 32
	// DefaultEITAggregatorMaxEventsPerService is the default bound of the per-service event window
	DefaultEITAggregatorMaxEventsPerService = 64
	// DefaultEITAggregatorMaxEventDuration is the default maximum accepted event duration.
	// Events with a non-positive duration or a duration greater than this value are
	// considered invalid and are dropped at ingest time.
	DefaultEITAggregatorMaxEventDuration = 24 * time.Hour
)

// EITAggregatorOptions represents EIT aggregator options
type EITAggregatorOptions struct {
	// MaxEventsPerService bounds the per-service event window. Once expired events
	// have been pruned and events sorted by start time, the window is truncated to
	// this size. Defaults to DefaultEITAggregatorMaxEventsPerService.
	MaxEventsPerService int
	// MaxEventDuration is the maximum accepted event duration. Events with a
	// duration <= 0 or > MaxEventDuration are dropped. Defaults to
	// DefaultEITAggregatorMaxEventDuration.
	MaxEventDuration time.Duration
}

// EITEvent represents an aggregated EIT event
type EITEvent struct {
	Duration       time.Duration
	EventID        uint16
	HasFreeCSAMode bool
	Language       string // ISO 639 language code of the first short event descriptor, if any
	Name           string // Event name of the first short event descriptor, if any
	RunningStatus  uint8
	ServiceID      uint16
	StartTime      time.Time
	TableID        PSITableID
	Text           string // Text of the first short event descriptor, if any
}

// eitSectionState holds the events of one received section
type eitSectionState struct {
	events []*EITEvent
}

// eitTableState holds the current version of one (service, table ID) pair
type eitTableState struct {
	sections   map[uint8]*eitSectionState // Keyed by section number
	version    uint8
	hasVersion bool
}

// EITAggregator merges EIT sections into a per-service, start-time-sorted and
// bounded event window.
//
// Sections are keyed by (service ID, table ID, version, section number):
//   - a section carrying a newer version (mod-32 distance in [1,16]) replaces
//     all previously stored sections of the same (service, table ID) pair,
//   - a section carrying an older version (mod-32 distance in [17,31]) is
//     ignored, which makes version wrap-around deterministic,
//   - a retransmitted section (same version, same section number) is a no-op,
//   - a new section number of the current version is added to the set; missing
//     section numbers simply contribute no events.
//
// The aggregator is not safe for concurrent use.
type EITAggregator struct {
	maxEventsPerService int
	maxEventDuration    time.Duration
	services            map[uint16]map[PSITableID]*eitTableState // Keyed by service ID then table ID
}

// NewEITAggregator creates a new EIT aggregator
func NewEITAggregator(o EITAggregatorOptions) *EITAggregator {
	a := &EITAggregator{
		maxEventsPerService: o.MaxEventsPerService,
		maxEventDuration:    o.MaxEventDuration,
		services:            make(map[uint16]map[PSITableID]*eitTableState),
	}
	if a.maxEventsPerService <= 0 {
		a.maxEventsPerService = DefaultEITAggregatorMaxEventsPerService
	}
	if a.maxEventDuration <= 0 {
		a.maxEventDuration = DefaultEITAggregatorMaxEventDuration
	}
	return a
}

// isNewerEITVersion checks whether v is newer than cur using mod-32 distance
func isNewerEITVersion(v, cur uint8) bool {
	d := (v - cur) % eitVersionModulus
	return d >= 1 && d <= eitVersionModulus/2
}

// Add ingests one EIT section and reports whether it changed the aggregated
// state. tableID must be in the EIT range [0x4e, 0x6f], h is the PSI section
// syntax header carrying the version and section numbers, and d is the parsed
// EIT data. Sections with a nil header or data, or an out-of-range table ID,
// are ignored.
func (a *EITAggregator) Add(tableID PSITableID, h *PSISectionSyntaxHeader, d *EITData) bool {
	// Validate input
	if h == nil || d == nil || tableID < PSITableIDEITStart || tableID > PSITableIDEITEnd {
		return false
	}

	// Get or create table state
	tables, ok := a.services[d.ServiceID]
	if !ok {
		tables = make(map[PSITableID]*eitTableState)
		a.services[d.ServiceID] = tables
	}
	t, ok := tables[tableID]
	if !ok {
		t = &eitTableState{sections: make(map[uint8]*eitSectionState)}
		tables[tableID] = t
	}

	// Version handling
	if t.hasVersion {
		if h.VersionNumber == t.version {
			// Same version: a retransmitted section is a no-op
			if _, ok := t.sections[h.SectionNumber]; ok {
				return false
			}
		} else if isNewerEITVersion(h.VersionNumber, t.version) {
			// Newer version: replace all previously stored sections
			t.sections = make(map[uint8]*eitSectionState)
			t.version = h.VersionNumber
		} else {
			// Older version (including across wrap-around): ignore
			return false
		}
	} else {
		t.hasVersion = true
		t.version = h.VersionNumber
	}

	// Store section, dropping events with an invalid duration
	s := &eitSectionState{}
	for _, e := range d.Events {
		if e == nil || e.Duration <= 0 || e.Duration > a.maxEventDuration {
			continue
		}
		s.events = append(s.events, newEITEvent(tableID, d.ServiceID, e))
	}
	t.sections[h.SectionNumber] = s
	return true
}

// newEITEvent builds an aggregated event from a parsed EIT event
func newEITEvent(tableID PSITableID, serviceID uint16, e *EITDataEvent) *EITEvent {
	o := &EITEvent{
		Duration:       e.Duration,
		EventID:        e.EventID,
		HasFreeCSAMode: e.HasFreeCSAMode,
		RunningStatus:  e.RunningStatus,
		ServiceID:      serviceID,
		StartTime:      e.StartTime,
		TableID:        tableID,
	}
	for _, d := range e.Descriptors {
		if d != nil && d.ShortEvent != nil {
			o.Language = string(d.ShortEvent.Language)
			o.Name = string(d.ShortEvent.EventName)
			o.Text = string(d.ShortEvent.Text)
			break
		}
	}
	return o
}

// eitEventWins checks whether the stored event a (from section sa) should be
// kept over event b (from section sb) when both share the same event ID
func eitEventWins(a *EITEvent, sa uint8, b *EITEvent, sb uint8) bool {
	if a.TableID != b.TableID {
		return a.TableID < b.TableID
	}
	if !a.StartTime.Equal(b.StartTime) {
		return a.StartTime.Before(b.StartTime)
	}
	return sa <= sb
}

// Window returns the aggregated events of a service, pruned, deduplicated,
// sorted by start time and bounded to MaxEventsPerService.
//
// Events whose end time (start time + duration) is not after now are pruned;
// pass the zero time to disable pruning. Events are deduplicated by event ID:
// on duplicates the event from the lowest table ID wins, then the earliest
// start time, then the lowest section number. The result is sorted by start
// time, ties broken by event ID.
func (a *EITAggregator) Window(serviceID uint16, now time.Time) []*EITEvent {
	// Collect events of the current version of each table
	byID := make(map[uint16]*EITEvent)
	sectionOf := make(map[uint16]uint8)
	for _, t := range a.services[serviceID] {
		for sectionNumber, s := range t.sections {
			for _, e := range s.events {
				// Prune expired events
				if !now.IsZero() && !e.StartTime.Add(e.Duration).After(now) {
					continue
				}
				// Deduplicate by event ID. The comparison makes the choice
				// independent of map iteration and section arrival order.
				if prev, ok := byID[e.EventID]; ok && eitEventWins(prev, sectionOf[e.EventID], e, sectionNumber) {
					continue
				}
				byID[e.EventID] = e
				sectionOf[e.EventID] = sectionNumber
			}
		}
	}

	// Sort by start time, ties broken by event ID
	events := make([]*EITEvent, 0, len(byID))
	for _, e := range byID {
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].StartTime.Equal(events[j].StartTime) {
			return events[i].EventID < events[j].EventID
		}
		return events[i].StartTime.Before(events[j].StartTime)
	})

	// Bound the window
	if len(events) > a.maxEventsPerService {
		events = events[:a.maxEventsPerService]
	}
	return events
}
