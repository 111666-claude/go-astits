package astits

import (
	"errors"
	"fmt"
	"time"

	"github.com/asticode/go-astikit"
)

// Errors returned when a DVB date/time cannot be decoded or encoded.
var (
	// ErrDVBTimeInvalidBCD is returned when an hour/minute/second BCD digit is not in the 0-9 range.
	ErrDVBTimeInvalidBCD = errors.New("astits: invalid BCD coded time")
	// ErrDVBTimeInvalidMJD is returned when the MJD date does not encode a valid day in the DVB range.
	ErrDVBTimeInvalidMJD = errors.New("astits: invalid MJD date")
	// ErrDVBTimeOutOfRange is returned when the time cannot be represented by a 16-bit MJD and BCD time.
	ErrDVBTimeOutOfRange = errors.New("astits: time out of DVB range")
	// ErrDVBTimeSubSecond is returned when the time carries a sub-second component that cannot be encoded.
	ErrDVBTimeSubSecond = errors.New("astits: DVB time only supports second precision")
)

// dvbMJDMin and dvbMJDMax bound the Modified Julian Dates representable by a DVB time field:
// a 16-bit MJD. The minimum is set to the MJD of 1900-01-01, the earliest date the DVB
// Annex C date convention can represent.
const (
	dvbMJDMin uint16 = 15020 // 1900-01-01
	dvbMJDMax uint16 = 65535
)

// parseDVBTime parses a DVB time
// This field is coded as 16 bits giving the 16 LSBs of MJD followed by 24 bits coded as 6 digits in 4 - bit Binary
// Coded Decimal (BCD). If the start time is undefined (e.g. for an event in a NVOD reference service) all bits of the
// field are set to "1".
// I apologize for the computation which is really messy but details are given in the documentation
// Page: 160 | Annex C | Link: https://www.dvb.org/resources/public/standards/a38_dvb-si_specification.pdf
// (barbashov) the link above can be broken, alternative: https://dvb.org/wp-content/uploads/2019/12/a038_tm1217r37_en300468v1_17_1_-_rev-134_-_si_specification.pdf
func parseDVBTime(i *astikit.BytesIterator) (t time.Time, err error) {
	// Get next 2 bytes
	var bs []byte
	if bs, err = i.NextBytesNoCopy(2); err != nil {
		err = fmt.Errorf("astits: fetching next bytes failed: %w", err)
		return
	}

	// Date
	var mjd = uint16(bs[0])<<8 | uint16(bs[1])
	var yt = int((float64(mjd) - 15078.2) / 365.25)
	var mt = int((float64(mjd) - 14956.1 - float64(int(float64(yt)*365.25))) / 30.6001)
	var d = int(float64(mjd) - 14956 - float64(int(float64(yt)*365.25)) - float64(int(float64(mt)*30.6001)))
	var k int
	if mt == 14 || mt == 15 {
		k = 1
	}
	var y = 1900 + yt + k
	var m = mt - 1 - k*12
	t = time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)

	// Time
	var s time.Duration
	if s, err = parseDVBDurationSeconds(i); err != nil {
		err = fmt.Errorf("astits: parsing DVB duration seconds failed: %w", err)
		return
	}
	t = t.Add(s)
	return
}

// parseDVBDurationMinutes parses a minutes duration
// 16 bit field containing the duration of the event in hours, minutes. format: 4 digits, 4 - bit BCD = 18 bit
func parseDVBDurationMinutes(i *astikit.BytesIterator) (d time.Duration, err error) {
	var bs []byte
	if bs, err = i.NextBytesNoCopy(2); err != nil {
		err = fmt.Errorf("astits: fetching next bytes failed: %w", err)
		return
	}
	d = parseDVBDurationByte(bs[0])*time.Hour + parseDVBDurationByte(bs[1])*time.Minute
	return
}

// parseDVBDurationSeconds parses a seconds duration
// 24 bit field containing the duration of the event in hours, minutes, seconds. format: 6 digits, 4 - bit BCD = 24 bit
func parseDVBDurationSeconds(i *astikit.BytesIterator) (d time.Duration, err error) {
	var bs []byte
	if bs, err = i.NextBytesNoCopy(3); err != nil {
		err = fmt.Errorf("astits: fetching next bytes failed: %w", err)
		return
	}
	d = parseDVBDurationByte(bs[0])*time.Hour + parseDVBDurationByte(bs[1])*time.Minute + parseDVBDurationByte(bs[2])*time.Second
	return
}

// parseDVBDurationByte parses a duration byte
func parseDVBDurationByte(i byte) time.Duration {
	return time.Duration(uint8(i)>>4*10 + uint8(i)&0xf)
}

func writeDVBTime(w *astikit.BitsWriter, t time.Time) (int, error) {
	year := t.Year() - 1900
	month := t.Month()
	day := t.Day()

	l := 0
	if month <= time.February {
		l = 1
	}

	mjd := 14956 + day + int(float64(year-l)*365.25) + int(float64(int(month)+1+l*12)*30.6001)

	d := t.Sub(t.Truncate(24 * time.Hour))

	b := astikit.NewBitsWriterBatch(w)

	b.Write(uint16(mjd))
	bytesWritten, err := writeDVBDurationSeconds(w, d)
	if err != nil {
		return 2, err
	}

	return bytesWritten + 2, b.Err()
}

func writeDVBDurationMinutes(w *astikit.BitsWriter, d time.Duration) (int, error) {
	b := astikit.NewBitsWriterBatch(w)

	hours := uint8(d.Hours())
	minutes := uint8(int(d.Minutes()) % 60)

	b.Write(dvbDurationByteRepresentation(hours))
	b.Write(dvbDurationByteRepresentation(minutes))

	return 2, b.Err()
}

func writeDVBDurationSeconds(w *astikit.BitsWriter, d time.Duration) (int, error) {
	b := astikit.NewBitsWriterBatch(w)

	hours := uint8(d.Hours())
	minutes := uint8(int(d.Minutes()) % 60)
	seconds := uint8(int(d.Seconds()) % 60)

	b.Write(dvbDurationByteRepresentation(hours))
	b.Write(dvbDurationByteRepresentation(minutes))
	b.Write(dvbDurationByteRepresentation(seconds))

	return 3, b.Err()
}

func dvbDurationByteRepresentation(n uint8) uint8 {
	return (n/10)<<4 | n%10
}

// parseDVBDurationByteStrict parses a BCD coded hour/minute/second byte, rejecting non-BCD digits
// or values outside their allowed range.
func parseDVBDurationByteStrict(b byte, max uint8) (uint8, error) {
	hi, lo := b>>4, b&0x0f
	if hi > 9 || lo > 9 {
		return 0, fmt.Errorf("%w: 0x%02x", ErrDVBTimeInvalidBCD, b)
	}
	v := hi*10 + lo
	if v > max {
		return 0, fmt.Errorf("%w: value %d exceeds %d", ErrDVBTimeInvalidBCD, v, max)
	}
	return v, nil
}

// mjdToTime converts a DVB Modified Julian Date to a UTC midnight time using the integer-only
// Fliegel-Van Flandern Gregorian calendar formula (valid for the whole 16-bit MJD range).
func mjdToTime(mjd uint16) (time.Time, error) {
	if mjd < dvbMJDMin || mjd > dvbMJDMax {
		return time.Time{}, fmt.Errorf("%w: MJD %d", ErrDVBTimeInvalidMJD, mjd)
	}

	// Julian Day Number from MJD.
	jdn := int64(mjd) + 2400001

	// Fliegel-Van Flandern: JDN to Gregorian year/month/day.
	a := jdn + 68569
	b := 4 * a / 146097
	a = a - (146097*b+3)/4
	c := 4000 * (a + 1) / 1461001
	d := a - 1461*c/4 + 31
	e := 80 * d / 2447
	day := int(d - 2447*e/80)
	a = e / 11
	month := time.Month(e + 2 - 12*a)
	year := int(100*(b-49) + c + a)

	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	// Round-trip check: only accept dates the DVB encoding can actually represent.
	encoded, err := timeToMJD(t)
	if err != nil || encoded != mjd {
		return time.Time{}, fmt.Errorf("%w: MJD %d", ErrDVBTimeInvalidMJD, mjd)
	}
	return t, nil
}

// timeToMJD converts a UTC date to a DVB Modified Julian Date using the integer-only
// Fliegel-Van Flandern Gregorian calendar formula. It returns an error when the date is outside
// the 16-bit MJD range.
func timeToMJD(t time.Time) (uint16, error) {
	t = t.UTC()
	year := int64(t.Year())
	month := int64(t.Month())
	day := int64(t.Day())

	a := (14 - month) / 12
	y := year + 4800 - a
	m := month + 12*a - 3

	// Julian Day Number, then MJD = JDN - 2400000.5 rounded down (JDN of an integer date is x.5).
	jdn := day + (153*m+2)/5 + 365*y + y/4 - y/100 + y/400 - 32045
	mjd := jdn - 2400001

	if mjd < int64(dvbMJDMin) || mjd > int64(dvbMJDMax) {
		return 0, fmt.Errorf("%w: %s", ErrDVBTimeOutOfRange, t.Format(time.RFC3339))
	}
	return uint16(mjd), nil
}

// parseDVBTimeStrict parses a DVB UTC time field (16-bit MJD + 3 BCD coded time bytes) and
// rejects bad BCD digits, out-of-range time components, invalid MJDs and truncated sections.
// The returned time is always in UTC regardless of the machine local timezone.
func parseDVBTimeStrict(i *astikit.BytesIterator) (time.Time, error) {
	bs, err := i.NextBytesNoCopy(2)
	if err != nil {
		return time.Time{}, fmt.Errorf("astits: fetching MJD bytes failed: %w", err)
	}
	mjd := uint16(bs[0])<<8 | uint16(bs[1])

	t, err := mjdToTime(mjd)
	if err != nil {
		return time.Time{}, err
	}

	tb, err := i.NextBytesNoCopy(3)
	if err != nil {
		return time.Time{}, fmt.Errorf("astits: fetching BCD time bytes failed: %w", err)
	}

	hours, err := parseDVBDurationByteStrict(tb[0], 23)
	if err != nil {
		return time.Time{}, err
	}
	minutes, err := parseDVBDurationByteStrict(tb[1], 59)
	if err != nil {
		return time.Time{}, err
	}
	seconds, err := parseDVBDurationByteStrict(tb[2], 59)
	if err != nil {
		return time.Time{}, err
	}

	return t.Add(time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(seconds)*time.Second), nil
}

// writeDVBTimeStrict encodes a DVB UTC time field (16-bit MJD + 3 BCD coded time bytes).
// The time is converted to UTC first, so the result does not depend on the machine timezone.
// Times outside the 16-bit MJD range or carrying sub-second precision are rejected.
func writeDVBTimeStrict(w *astikit.BitsWriter, t time.Time) (int, error) {
	t = t.UTC()
	if nanos := t.Nanosecond(); nanos != 0 {
		return 0, fmt.Errorf("%w: %s", ErrDVBTimeSubSecond, t.Format(time.RFC3339Nano))
	}

	mjd, err := timeToMJD(t)
	if err != nil {
		return 0, err
	}

	b := astikit.NewBitsWriterBatch(w)
	b.Write(mjd)
	b.Write(dvbDurationByteRepresentation(uint8(t.Hour())))
	b.Write(dvbDurationByteRepresentation(uint8(t.Minute())))
	b.Write(dvbDurationByteRepresentation(uint8(t.Second())))
	if err := b.Err(); err != nil {
		return 0, err
	}
	return 5, nil
}
