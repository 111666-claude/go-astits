package astits

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func utcDate(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestWriteParseDVBTimeStrict_RoundTrip(t *testing.T) {
	// Cross-day, leap day, second-precision and boundary samples. Every encoded field must be
	// decoded back to the same UTC second regardless of the machine timezone.
	cases := []struct {
		name string
		t    time.Time
	}{
		{"EN example", utcDate("1993-10-13 12:45:00")},
		{"midnight start of day", utcDate("2024-02-29 00:00:00")},
		{"leap day last second", utcDate("2024-02-29 23:59:59")},
		{"cross day into march", utcDate("2024-02-29 23:59:59").Add(time.Second)},
		{"cross year", utcDate("2023-12-31 23:59:59").Add(time.Second)},
		{"leap year 2000", utcDate("2000-02-29 08:30:15")},
		{"non-leap century year", utcDate("1900-03-01 06:00:00")},
		{"earliest encodable date", utcDate("1900-01-01 00:00:00")},
		{"latest encodable date", mustMJDDate(t, dvbMJDMax)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
			n, err := writeDVBTimeStrict(w, tc.t)
			require.NoError(t, err)
			assert.Equal(t, 5, n)
			assert.Equal(t, 5, buf.Len())

			got, err := parseDVBTimeStrict(astikit.NewBytesIterator(buf.Bytes()))
			require.NoError(t, err)
			assert.True(t, got.Equal(tc.t), "got %s want %s", got, tc.t)
			assert.Equal(t, time.UTC, got.Location())
		})
	}
}

func mustMJDDate(t *testing.T, mjd uint16) time.Time {
	t.Helper()
	d, err := mjdToTime(mjd)
	require.NoError(t, err)
	return d.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
}

func TestWriteDVBTimeStrict_KnownVector(t *testing.T) {
	// EN 300 468 Annex C: 1993-10-13 12:45:00 -> C0 79 12 45 00
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	n, err := writeDVBTimeStrict(w, utcDate("1993-10-13 12:45:00"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, []byte{0xc0, 0x79, 0x12, 0x45, 0x00}, buf.Bytes())
}

func TestWriteDVBTimeStrict_TimezoneIndependent(t *testing.T) {
	// The same instant expressed in different zones must encode to identical UTC bytes.
	utc := utcDate("2024-02-29 23:59:59")
	zones := []*time.Location{
		time.UTC,
		time.FixedZone("UTC+5", 5*3600),
		time.FixedZone("UTC-8", -8*3600),
	}
	var expected []byte
	for _, z := range zones {
		inZone := utc.In(z)
		buf := &bytes.Buffer{}
		w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
		_, err := writeDVBTimeStrict(w, inZone)
		require.NoError(t, err)
		if expected == nil {
			expected = buf.Bytes()
		} else {
			assert.Equal(t, expected, buf.Bytes())
		}
	}

	// And decoding always yields a UTC time even though no zone information is carried.
	got, err := parseDVBTimeStrict(astikit.NewBytesIterator(expected))
	require.NoError(t, err)
	assert.True(t, got.Equal(utc))
	assert.Equal(t, time.UTC, got.Location())
}

func TestWriteDVBTimeStrict_Errors(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		want error
	}{
		{"sub-second nanoseconds", utcDate("2024-01-01 00:00:00").Add(time.Nanosecond), ErrDVBTimeSubSecond},
		{"sub-second milliseconds", utcDate("2024-01-01 00:00:00").Add(250 * time.Millisecond), ErrDVBTimeSubSecond},
		{"before DVB range", time.Date(1899, 12, 31, 23, 59, 59, 0, time.UTC), ErrDVBTimeOutOfRange},
		{"after 16-bit MJD range", time.Date(2038, 4, 23, 0, 0, 0, 0, time.UTC), ErrDVBTimeOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
			n, err := writeDVBTimeStrict(w, tc.t)
			assert.True(t, errors.Is(err, tc.want))
			assert.Equal(t, 0, n)
			assert.Equal(t, 0, buf.Len())
		})
	}
}

func TestParseDVBTimeStrict_BadBCD(t *testing.T) {
	// Valid MJD (1993-10-13 = C0 79) followed by malformed BCD / out-of-range components.
	cases := []struct {
		name  string
		bytes []byte
	}{
		{"non-BCD hour digit", []byte{0xc0, 0x79, 0x1a, 0x45, 0x00}},
		{"hour out of range", []byte{0xc0, 0x79, 0x24, 0x45, 0x00}},
		{"non-BCD minute digit", []byte{0xc0, 0x79, 0x12, 0x4f, 0x00}},
		{"minute out of range", []byte{0xc0, 0x79, 0x12, 0x60, 0x00}},
		{"non-BCD second digit", []byte{0xc0, 0x79, 0x12, 0x45, 0x0a}},
		{"second out of range", []byte{0xc0, 0x79, 0x12, 0x45, 0x60}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDVBTimeStrict(astikit.NewBytesIterator(tc.bytes))
			assert.True(t, errors.Is(err, ErrDVBTimeInvalidBCD))
		})
	}
}

func TestParseDVBTimeStrict_Truncated(t *testing.T) {
	// Any section shorter than 5 bytes must be reported, not silently parsed.
	for n := 0; n < 5; n++ {
		bs := []byte{0xc0, 0x79, 0x12, 0x45, 0x00}[:n]
		_, err := parseDVBTimeStrict(astikit.NewBytesIterator(bs))
		assert.Error(t, err, "expected error for %d-byte section", n)
	}
}

func TestMJDToTime_InvalidMJD(t *testing.T) {
	// MJDs below the DVB representable range must be rejected explicitly.
	_, err := mjdToTime(dvbMJDMin - 1)
	assert.True(t, errors.Is(err, ErrDVBTimeInvalidMJD))
}

// TestParseDVBTime_LegencyCompatibility ensures the pre-existing lenient parser used by TOT/EIT
// still accepts its documented vector unchanged.
func TestParseDVBTime_LegacyCompatibility(t *testing.T) {
	d, err := parseDVBTime(astikit.NewBytesIterator(dvbTimeBytes))
	assert.NoError(t, err)
	assert.Equal(t, dvbTime, d)

	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	n, err := writeDVBTime(w, dvbTime)
	assert.NoError(t, err)
	assert.Equal(t, dvbTimeBytes, buf.Bytes())
	assert.Equal(t, len(dvbTimeBytes), n)

	// Sanity: both parsers agree on the valid vector.
	strict, err := parseDVBTimeStrict(astikit.NewBytesIterator(dvbTimeBytes))
	require.NoError(t, err)
	assert.True(t, strict.Equal(d))
}

func TestParseDVBDurationByteStrict(t *testing.T) {
	v, err := parseDVBDurationByteStrict(0x59, 59)
	require.NoError(t, err)
	assert.Equal(t, uint8(59), v)

	_, err = parseDVBDurationByteStrict(0x5a, 59)
	assert.True(t, errors.Is(err, ErrDVBTimeInvalidBCD))
	_, err = parseDVBDurationByteStrict(0x60, 59)
	assert.True(t, errors.Is(err, ErrDVBTimeInvalidBCD))
}
