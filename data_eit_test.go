package astits

import (
	"bytes"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var eit = &EITData{
	Events: []*EITDataEvent{{
		Descriptors:    descriptors,
		Duration:       dvbDurationSeconds,
		EventID:        6,
		HasFreeCSAMode: true,
		RunningStatus:  7,
		StartTime:      dvbTime,
	}},
	LastTableID:              5,
	OriginalNetworkID:        3,
	SegmentLastSectionNumber: 4,
	ServiceID:                1,
	TransportStreamID:        2,
}

func eitBytes() []byte {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write(uint16(2))               // Transport stream ID
	w.Write(uint16(3))               // Original network ID
	w.Write(uint8(4))                // Segment last section number
	w.Write(uint8(5))                // Last table id
	w.Write(uint16(6))               // Event #1 id
	w.Write(dvbTimeBytes)            // Event #1 start time
	w.Write(dvbDurationSecondsBytes) // Event #1 duration
	w.Write("111")                   // Event #1 running status
	w.Write("1")                     // Event #1 free CA mode
	descriptorsBytes(w)              // Event #1 descriptors
	return buf.Bytes()
}

func TestParseEITSection(t *testing.T) {
	var b = eitBytes()
	d, err := parseEITSection(astikit.NewBytesIterator(b), len(b), uint16(1))
	assert.Equal(t, d, eit)
	assert.NoError(t, err)
}

func TestWriteEITSection(t *testing.T) {
	d := &EITData{
		TransportStreamID:        7,
		OriginalNetworkID:        9,
		SegmentLastSectionNumber: 0,
		LastTableID:              uint8(PSITableIDEITPresentFollowing),
		Events: []*EITDataEvent{
			{
				EventID:       100,
				StartTime:     dvbTime,
				Duration:      dvbDurationSeconds,
				RunningStatus: RunningStatusRunning,
				Descriptors: []*Descriptor{{
					Tag: DescriptorTagShortEvent,
					ShortEvent: &DescriptorShortEvent{
						Language:  []byte("eng"),
						EventName: []byte("Event"),
					},
					Length: 10, // language (3) + name length (1) + name (5) + text length (1)
				}},
			},
		},
	}

	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	n, err := writeEITSection(w, d)
	assert.NoError(t, err)
	assert.Equal(t, int(calcEITSectionLength(d)), n)
	assert.Equal(t, n, buf.Len())

	// Round-trip the section data with the parser.
	got, err := parseEITSection(astikit.NewBytesIterator(buf.Bytes()), buf.Len(), d.TransportStreamID)
	assert.NoError(t, err)
	require.Len(t, got.Events, 1)
	assert.Equal(t, uint16(100), got.Events[0].EventID)
	assert.Equal(t, dvbTime, got.Events[0].StartTime)
	assert.Equal(t, dvbDurationSeconds, got.Events[0].Duration)
	assert.Equal(t, uint8(RunningStatusRunning), got.Events[0].RunningStatus)
	require.Len(t, got.Events[0].Descriptors, 1)
	assert.Equal(t, []byte("eng"), got.Events[0].Descriptors[0].ShortEvent.Language)
	assert.Equal(t, []byte("Event"), got.Events[0].Descriptors[0].ShortEvent.EventName)
}

func TestCalcEITSectionLength(t *testing.T) {
	d := &EITData{Events: []*EITDataEvent{
		{Descriptors: []*Descriptor{
			{Tag: DescriptorTagShortEvent, ShortEvent: &DescriptorShortEvent{Language: []byte("eng"), EventName: []byte("A")}},
			{Tag: DescriptorTagShortEvent, ShortEvent: &DescriptorShortEvent{Language: []byte("eng"), EventName: []byte("B")}},
		}},
		{Descriptors: []*Descriptor{
			{Tag: DescriptorTagShortEvent, ShortEvent: &DescriptorShortEvent{Language: []byte("eng"), EventName: []byte("C")}},
		}},
	}}

	// section header + 2 events + 3 short event descriptors (tag+length+payload)
	expected := uint16(eitSectionHeaderBytesSize) +
		2*eitEventHeaderBytesSize +
		(2 + 5 + 1) + (2 + 5 + 1) + (2 + 5 + 1)
	assert.Equal(t, expected, calcEITSectionLength(d))
}
