package astits

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var eitAggregatorBase = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func eitAggregatorEvent(id uint16, start time.Time, dur time.Duration, language, name string) *EITDataEvent {
	e := &EITDataEvent{
		Duration:  dur,
		EventID:   id,
		StartTime: start,
	}
	if language != "" || name != "" {
		e.Descriptors = []*Descriptor{{ShortEvent: &DescriptorShortEvent{
			EventName: []byte(name),
			Language:  []byte(language),
			Text:      []byte("text of " + name),
		}}}
	}
	return e
}

func eitAggregatorHeader(version, sectionNumber, lastSectionNumber uint8) *PSISectionSyntaxHeader {
	return &PSISectionSyntaxHeader{
		CurrentNextIndicator: true,
		LastSectionNumber:    lastSectionNumber,
		SectionNumber:        sectionNumber,
		VersionNumber:        version,
	}
}

func eitAggregatorWindowIDs(es []*EITEvent) (ids []uint16) {
	for _, e := range es {
		ids = append(ids, e.EventID)
	}
	return
}

// Two services receive three rounds of EIT updates, interleaved.
func TestEITAggregatorTwoServicesThreeRounds(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	// Round 1: initial sections for both services
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 1), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(2, eitAggregatorBase.Add(time.Hour), 15*time.Minute, "eng", "Show"),
			eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "eng", "News"),
		},
	}))
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 1, 1), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(3, eitAggregatorBase.Add(2*time.Hour), time.Hour, "fra", "Film"),
		},
	}))
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 0), &EITData{
		ServiceID: 2,
		Events: []*EITDataEvent{
			eitAggregatorEvent(1, eitAggregatorBase.Add(30*time.Minute), 45*time.Minute, "deu", "Sport"),
		},
	}))

	// Service 1 window is sorted by start time and keeps language, name and duration
	w1 := a.Window(1, time.Time{})
	assert.Equal(t, []uint16{1, 2, 3}, eitAggregatorWindowIDs(w1))
	assert.Equal(t, "eng", w1[0].Language)
	assert.Equal(t, "News", w1[0].Name)
	assert.Equal(t, "text of News", w1[0].Text)
	assert.Equal(t, time.Hour, w1[0].Duration)
	assert.Equal(t, uint16(1), w1[0].ServiceID)
	assert.Equal(t, PSITableIDEITStart, w1[0].TableID)

	// Service 2 is isolated from service 1 sections
	assert.Equal(t, []uint16{1}, eitAggregatorWindowIDs(a.Window(2, time.Time{})))

	// Round 2: retransmission is a no-op, then a new version replaces old sections
	assert.False(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 1), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "eng", "News"),
			eitAggregatorEvent(2, eitAggregatorBase.Add(time.Hour), 15*time.Minute, "eng", "Show"),
		},
	}))
	assert.Equal(t, []uint16{1, 2, 3}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))

	// Interleaved update of service 2
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(1, 0, 0), &EITData{
		ServiceID: 2,
		Events: []*EITDataEvent{
			eitAggregatorEvent(1, eitAggregatorBase.Add(30*time.Minute), 45*time.Minute, "deu", "Sport"),
			eitAggregatorEvent(2, eitAggregatorBase.Add(75*time.Minute), 30*time.Minute, "deu", "Talk"),
		},
	}))
	assert.Equal(t, []uint16{1, 2}, eitAggregatorWindowIDs(a.Window(2, time.Time{})))

	// Service 1 version 1: event 1 removed, event 2 kept, event 4 added. Event 3
	// disappears since a new version replaces all sections of the previous one.
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(1, 0, 0), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(2, eitAggregatorBase.Add(time.Hour), 15*time.Minute, "eng", "Show"),
			eitAggregatorEvent(4, eitAggregatorBase.Add(90*time.Minute), time.Hour, "eng", "Movie"),
		},
	}))
	assert.Equal(t, []uint16{2, 4}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
	// Service 2 is unaffected by service 1's version change
	assert.Equal(t, []uint16{1, 2}, eitAggregatorWindowIDs(a.Window(2, time.Time{})))

	// Round 3: prune expired items with an injected current time
	// Event 2 ends at base+1h15, event 4 at base+2h30.
	now := eitAggregatorBase.Add(80 * time.Minute)
	assert.Equal(t, []uint16{4}, eitAggregatorWindowIDs(a.Window(1, now)))
	// Without a current time nothing is pruned
	assert.Equal(t, []uint16{2, 4}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
	// Service 2: event 1 ends at base+1h15 and is pruned, event 2 is still valid
	assert.Equal(t, []uint16{2}, eitAggregatorWindowIDs(a.Window(2, now)))
}

func TestEITAggregatorVersionWrap(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	// Version 31
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(31, 0, 0), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "", "")},
	}))
	// Version 0 wraps around and is therefore newer: it replaces version 31
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 0), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(2, eitAggregatorBase, time.Hour, "", "")},
	}))
	assert.Equal(t, []uint16{2}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
	// Version 31 is now half a cycle behind and is ignored
	assert.False(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(31, 0, 0), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(3, eitAggregatorBase, time.Hour, "", "")},
	}))
	assert.Equal(t, []uint16{2}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
}

func TestEITAggregatorMissingAndOutOfOrderSections(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	// Section 1 arrives before section 0
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 1, 1), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(2, eitAggregatorBase.Add(time.Hour), time.Hour, "", "")},
	}))
	// The window only contains received sections
	assert.Equal(t, []uint16{2}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
	// The missing section is merged once received
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 1), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "", "")},
	}))
	assert.Equal(t, []uint16{1, 2}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
}

func TestEITAggregatorInvalidDuration(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 0), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(1, eitAggregatorBase, 0, "", ""),            // Zero duration
			eitAggregatorEvent(2, eitAggregatorBase, -time.Minute, "", ""), // Negative duration
			eitAggregatorEvent(3, eitAggregatorBase, 25*time.Hour, "", ""), // Above the maximum
			eitAggregatorEvent(4, eitAggregatorBase, time.Hour, "", ""),    // Valid
			nil, // Nil event
		},
	}))
	assert.Equal(t, []uint16{4}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
}

func TestEITAggregatorDuplicateEventID(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	// Same event ID in two sections of the same version: the earliest start time wins
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 1), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(1, eitAggregatorBase.Add(2*time.Hour), time.Hour, "eng", "Late")},
	}))
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 1, 1), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "eng", "Early")},
	}))
	w := a.Window(1, time.Time{})
	assert.Equal(t, []uint16{1}, eitAggregatorWindowIDs(w))
	assert.Equal(t, "Early", w[0].Name)

	// Same event ID in present/following (0x4e) and schedule (0x50) tables:
	// the lowest table ID wins
	assert.True(t, a.Add(PSITableIDEITStart+2, eitAggregatorHeader(0, 0, 0), &EITData{
		ServiceID: 1,
		Events:    []*EITDataEvent{eitAggregatorEvent(1, eitAggregatorBase.Add(3*time.Hour), time.Hour, "eng", "Schedule")},
	}))
	w = a.Window(1, time.Time{})
	assert.Equal(t, []uint16{1}, eitAggregatorWindowIDs(w))
	assert.Equal(t, "Early", w[0].Name)
}

func TestEITAggregatorConflictingStartTimes(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	// Overlapping events are all kept and sorted by start time, ties broken by event ID
	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 0), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(3, eitAggregatorBase.Add(30*time.Minute), time.Hour, "", ""),
			eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "", ""),
			eitAggregatorEvent(2, eitAggregatorBase, 30*time.Minute, "", ""),
		},
	}))
	assert.Equal(t, []uint16{1, 2, 3}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
}

func TestEITAggregatorBoundedWindow(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{MaxEventsPerService: 2})

	assert.True(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 0), &EITData{
		ServiceID: 1,
		Events: []*EITDataEvent{
			eitAggregatorEvent(1, eitAggregatorBase, time.Hour, "", ""),
			eitAggregatorEvent(2, eitAggregatorBase.Add(time.Hour), time.Hour, "", ""),
			eitAggregatorEvent(3, eitAggregatorBase.Add(2*time.Hour), time.Hour, "", ""),
		},
	}))
	// The window keeps the 2 earliest events
	assert.Equal(t, []uint16{1, 2}, eitAggregatorWindowIDs(a.Window(1, time.Time{})))
	// Pruning happens before bounding: event 1 has ended at base+1h30
	assert.Equal(t, []uint16{2, 3}, eitAggregatorWindowIDs(a.Window(1, eitAggregatorBase.Add(90*time.Minute))))
}

func TestEITAggregatorInvalidInput(t *testing.T) {
	a := NewEITAggregator(EITAggregatorOptions{})

	assert.False(t, a.Add(PSITableIDEITStart, nil, &EITData{ServiceID: 1}))
	assert.False(t, a.Add(PSITableIDEITStart, eitAggregatorHeader(0, 0, 0), nil))
	assert.False(t, a.Add(PSITableIDPAT, eitAggregatorHeader(0, 0, 0), &EITData{ServiceID: 1}))
	assert.False(t, a.Add(PSITableIDEITEnd+1, eitAggregatorHeader(0, 0, 0), &EITData{ServiceID: 1}))
	assert.Empty(t, a.Window(1, time.Time{}))
	assert.Empty(t, a.Window(42, time.Time{}))
}
