package astits

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
)

// Fixed TOT sample: two regions, one DST switch
var (
	totClockUTCTime      = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	totClockTimeOfChange = time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)
)

func totClockSample() *TOTData {
	return &TOTData{
		Descriptors: []*Descriptor{{
			Length: 26,
			Tag:    DescriptorTagLocalTimeOffset,
			LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{
				{
					CountryCode:             []byte("AAA"),
					CountryRegionID:         1,
					LocalTimeOffset:         time.Hour,
					LocalTimeOffsetPolarity: false,
					NextTimeOffset:          2 * time.Hour,
					TimeOfChange:            totClockTimeOfChange,
				},
				{
					CountryCode:             []byte("BBB"),
					CountryRegionID:         2,
					LocalTimeOffset:         5 * time.Hour,
					LocalTimeOffsetPolarity: true,
					NextTimeOffset:          4 * time.Hour,
					TimeOfChange:            totClockTimeOfChange,
				},
			}},
		}},
		UTCTime: totClockUTCTime,
	}
}

func totClockSampleBytes() []byte {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	writeDVBTime(w, totClockUTCTime)             // UTC time
	w.Write("0000")                              // Reserved
	w.Write("000000011100")                      // Descriptors overall length (28)
	w.Write(uint8(DescriptorTagLocalTimeOffset)) // Tag
	w.Write(uint8(26))                           // Length
	w.Write([]byte("AAA"))                       // Item #1 country code
	w.Write("000001")                            // Item #1 country region ID
	w.Write("0")                                 // Item #1 reserved
	w.Write("0")                                 // Item #1 local time offset polarity
	writeDVBDurationMinutes(w, time.Hour)        // Item #1 local time offset
	writeDVBTime(w, totClockTimeOfChange)        // Item #1 time of change
	writeDVBDurationMinutes(w, 2*time.Hour)      // Item #1 next time offset
	w.Write([]byte("BBB"))                       // Item #2 country code
	w.Write("000010")                            // Item #2 country region ID
	w.Write("0")                                 // Item #2 reserved
	w.Write("1")                                 // Item #2 local time offset polarity
	writeDVBDurationMinutes(w, 5*time.Hour)      // Item #2 local time offset
	writeDVBTime(w, totClockTimeOfChange)        // Item #2 time of change
	writeDVBDurationMinutes(w, 4*time.Hour)      // Item #2 next time offset
	return buf.Bytes()
}

func TestParseTOTClockSample(t *testing.T) {
	d, err := parseTOTSection(astikit.NewBytesIterator(totClockSampleBytes()))
	assert.NoError(t, err)
	assert.Equal(t, totClockSample(), d)
}

func totClockTestClock(t *testing.T) *TOTClock {
	c := NewTOTClock()
	d, err := parseTOTSection(astikit.NewBytesIterator(totClockSampleBytes()))
	assert.NoError(t, err)
	c.Update(d)
	return c
}

func TestTOTClockLocalTime(t *testing.T) {
	c := totClockTestClock(t)

	// Before the switch the current offsets apply
	before := totClockTimeOfChange.Add(-time.Second)
	o, err := c.Offset("AAA", 1, before)
	assert.NoError(t, err)
	assert.Equal(t, time.Hour, o)
	l, err := c.LocalTime("AAA", 1, before)
	assert.NoError(t, err)
	assert.Equal(t, before.Add(time.Hour), l)
	o, err = c.Offset("BBB", 2, before)
	assert.NoError(t, err)
	assert.Equal(t, -5*time.Hour, o)
	l, err = c.LocalTime("BBB", 2, before)
	assert.NoError(t, err)
	assert.Equal(t, before.Add(-5*time.Hour), l)

	// From the time of change on the next offsets apply
	for _, u := range []time.Time{totClockTimeOfChange, totClockTimeOfChange.Add(time.Second)} {
		o, err = c.Offset("AAA", 1, u)
		assert.NoError(t, err)
		assert.Equal(t, 2*time.Hour, o)
		l, err = c.LocalTime("AAA", 1, u)
		assert.NoError(t, err)
		assert.Equal(t, u.Add(2*time.Hour), l)
		o, err = c.Offset("BBB", 2, u)
		assert.NoError(t, err)
		assert.Equal(t, -4*time.Hour, o)
		l, err = c.LocalTime("BBB", 2, u)
		assert.NoError(t, err)
		assert.Equal(t, u.Add(-4*time.Hour), l)
	}
}

func TestTOTClockRegionNotFound(t *testing.T) {
	c := totClockTestClock(t)

	// Unknown country code
	_, err := c.Offset("ZZZ", 1, totClockUTCTime)
	assert.Equal(t, ErrTOTClockRegionNotFound, err)
	_, err = c.LocalTime("ZZZ", 1, totClockUTCTime)
	assert.Equal(t, ErrTOTClockRegionNotFound, err)

	// Unknown region ID
	_, err = c.Offset("AAA", 2, totClockUTCTime)
	assert.Equal(t, ErrTOTClockRegionNotFound, err)

	// Empty clock
	_, err = NewTOTClock().Offset("AAA", 1, totClockUTCTime)
	assert.Equal(t, ErrTOTClockRegionNotFound, err)
}

func TestTOTClockDuplicateDescriptors(t *testing.T) {
	// The first item of a region wins within a single update
	c := NewTOTClock()
	c.Update(&TOTData{Descriptors: []*Descriptor{
		{Tag: DescriptorTagLocalTimeOffset, LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{{
			CountryCode:     []byte("AAA"),
			CountryRegionID: 1,
			LocalTimeOffset: time.Hour,
		}}}},
		{Tag: DescriptorTagLocalTimeOffset, LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{{
			CountryCode:     []byte("AAA"),
			CountryRegionID: 1,
			LocalTimeOffset: 3 * time.Hour,
		}}}},
	}})
	o, err := c.Offset("AAA", 1, totClockUTCTime)
	assert.NoError(t, err)
	assert.Equal(t, time.Hour, o)
}

func TestTOTClockInvalidOffset(t *testing.T) {
	// Items with an offset bigger than 24h are skipped
	for _, item := range []*DescriptorLocalTimeOffsetItem{
		{CountryCode: []byte("AAA"), CountryRegionID: 1, LocalTimeOffset: 45 * time.Hour},
		{CountryCode: []byte("AAA"), CountryRegionID: 1, LocalTimeOffset: time.Hour, NextTimeOffset: 45 * time.Hour},
	} {
		c := NewTOTClock()
		c.Update(&TOTData{Descriptors: []*Descriptor{{Tag: DescriptorTagLocalTimeOffset, LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{item}}}}})
		_, err := c.Offset("AAA", 1, totClockUTCTime)
		assert.Equal(t, ErrTOTClockRegionNotFound, err)
	}

	// An offset of exactly 24h is valid
	c := NewTOTClock()
	c.Update(&TOTData{Descriptors: []*Descriptor{{Tag: DescriptorTagLocalTimeOffset, LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{{
		CountryCode:     []byte("AAA"),
		CountryRegionID: 1,
		LocalTimeOffset: 24 * time.Hour,
	}}}}}})
	o, err := c.Offset("AAA", 1, totClockUTCTime)
	assert.NoError(t, err)
	assert.Equal(t, 24*time.Hour, o)
}

func TestTOTClockBackwardTimeOfChange(t *testing.T) {
	item := func(offset time.Duration, timeOfChange time.Time) *DescriptorLocalTimeOffsetItem {
		return &DescriptorLocalTimeOffsetItem{
			CountryCode:     []byte("AAA"),
			CountryRegionID: 1,
			LocalTimeOffset: offset,
			TimeOfChange:    timeOfChange,
		}
	}
	update := func(c *TOTClock, item *DescriptorLocalTimeOffsetItem) {
		c.Update(&TOTData{Descriptors: []*Descriptor{{Tag: DescriptorTagLocalTimeOffset, LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{item}}}}})
	}

	c := NewTOTClock()
	update(c, item(time.Hour, totClockTimeOfChange))

	// An update with an earlier time of change is stale and skipped
	update(c, item(3*time.Hour, totClockTimeOfChange.Add(-time.Hour)))
	o, err := c.Offset("AAA", 1, totClockUTCTime)
	assert.NoError(t, err)
	assert.Equal(t, time.Hour, o)

	// An update with the same time of change applies
	update(c, item(2*time.Hour, totClockTimeOfChange))
	o, err = c.Offset("AAA", 1, totClockUTCTime)
	assert.NoError(t, err)
	assert.Equal(t, 2*time.Hour, o)
}

func TestTOTClockUpdate(t *testing.T) {
	// Query results follow TOT updates
	c := totClockTestClock(t)
	o, err := c.Offset("AAA", 1, totClockUTCTime)
	assert.NoError(t, err)
	assert.Equal(t, time.Hour, o)

	c.Update(&TOTData{Descriptors: []*Descriptor{{Tag: DescriptorTagLocalTimeOffset, LocalTimeOffset: &DescriptorLocalTimeOffset{Items: []*DescriptorLocalTimeOffsetItem{{
		CountryCode:     []byte("AAA"),
		CountryRegionID: 1,
		LocalTimeOffset: 2 * time.Hour,
		NextTimeOffset:  time.Hour,
		TimeOfChange:    time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC),
	}}}}}})
	o, err = c.Offset("AAA", 1, totClockUTCTime)
	assert.NoError(t, err)
	assert.Equal(t, 2*time.Hour, o)
}

func TestTOTClockTimeOverflow(t *testing.T) {
	c := totClockTestClock(t)

	// A time close enough to the maximum representable time so that adding the
	// offset overflows. 62135596800 is the number of seconds between year 1 and
	// the unix epoch.
	u := time.Unix(math.MaxInt64-62135596800, 0)
	_, err := c.LocalTime("AAA", 1, u)
	assert.Equal(t, ErrTOTClockTimeOverflow, err)

	// The offset itself is still reported
	_, err = c.Offset("AAA", 1, u)
	assert.NoError(t, err)
}
