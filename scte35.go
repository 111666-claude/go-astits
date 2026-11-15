package astits

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/asticode/go-astikit"
)

// SCTE-35 (ANSI/SCTE 35) splice_info_section support.
// Reference: ANSI/SCTE 35-2020, sections 9 (splice commands) and 10 (descriptors).

const (
	// scte35TableID is the only legal splice_info_section table_id.
	scte35TableID uint8 = 0xfc
	// scte35ProtocolVersion is the only protocol version currently defined.
	scte35ProtocolVersion uint8 = 0x00
	// scte35MaxSectionLength is the maximum legal section_length value.
	scte35MaxSectionLength uint16 = 4093
	// scte35MinSectionLength bounds a section containing a command type byte,
	// an empty descriptor loop and a CRC32.
	scte35MinSectionLength uint16 = 17
	// scte35PTSModulo is 2^33, the wrap period of every SCTE-35 PTS field.
	scte35PTSModulo uint64 = 1 << 33
	// scte35PTSMask keeps the 33 least significant bits of a PTS value.
	scte35PTSMask uint64 = scte35PTSModulo - 1
	// scte35CUEIIdentifier is the "CUEI" identifier carried by SCTE-35 splice descriptors.
	scte35CUEIIdentifier uint32 = 0x43554549
)

// SCTE-35 splice command types (ANSI/SCTE 35-2020 table 9).
const (
	SCTE35SpliceCommandTypeSpliceNull           SCTE35SpliceCommandType = 0x00
	SCTE35SpliceCommandTypeSpliceSchedule       SCTE35SpliceCommandType = 0x04
	SCTE35SpliceCommandTypeSpliceInsert         SCTE35SpliceCommandType = 0x05
	SCTE35SpliceCommandTypeTimeSignal           SCTE35SpliceCommandType = 0x06
	SCTE35SpliceCommandTypeBandwidthReservation SCTE35SpliceCommandType = 0x07
	SCTE35SpliceCommandTypePrivateCommand       SCTE35SpliceCommandType = 0xff
)

// SCTE-35 splice descriptor tags (ANSI/SCTE 35-2020 table 14).
const (
	SCTE35DescriptorTagAvail        uint8 = 0x00
	SCTE35DescriptorTagDTMF         uint8 = 0x01
	SCTE35DescriptorTagSegmentation uint8 = 0x02
	SCTE35DescriptorTagTime         uint8 = 0x03
	SCTE35DescriptorTagAudio        uint8 = 0x04
)

// Common segmentation_type_id values (ANSI/SCTE 35-2020 table 24).
const (
	SCTE35SegmentationTypeProgramStart               uint8 = 0x10
	SCTE35SegmentationTypeProgramEnd                 uint8 = 0x11
	SCTE35SegmentationTypeProgramEarlyTermination    uint8 = 0x12
	SCTE35SegmentationTypeProgramBreakStart          uint8 = 0x13
	SCTE35SegmentationTypeProgramBreakEnd            uint8 = 0x14
	SCTE35SegmentationTypeProgramRunoverPlanned      uint8 = 0x17
	SCTE35SegmentationTypeProgramRunoverImpromptu    uint8 = 0x18
	SCTE35SegmentationTypeChapterStart               uint8 = 0x20
	SCTE35SegmentationTypeChapterEnd                 uint8 = 0x21
	SCTE35SegmentationTypeProviderAdvertisementStart uint8 = 0x30
	SCTE35SegmentationTypeProviderAdvertisementEnd   uint8 = 0x32
	SCTE35SegmentationTypeUnscheduledEventStart      uint8 = 0x34
	SCTE35SegmentationTypeUnscheduledEventEnd        uint8 = 0x36
	SCTE35SegmentationTypeNetworkAdvertisementStart  uint8 = 0x38
	SCTE35SegmentationTypeNetworkAdvertisementEnd    uint8 = 0x3a
)

// Errors
var (
	ErrSCTE35InvalidSection = errors.New("astits: invalid SCTE-35 splice_info_section")
)

// SCTE35SpliceCommandType identifies the splice command carried by a section.
type SCTE35SpliceCommandType uint8

// String returns a human readable command type name.
func (t SCTE35SpliceCommandType) String() string {
	switch t {
	case SCTE35SpliceCommandTypeSpliceNull:
		return "splice_null"
	case SCTE35SpliceCommandTypeSpliceSchedule:
		return "splice_schedule"
	case SCTE35SpliceCommandTypeSpliceInsert:
		return "splice_insert"
	case SCTE35SpliceCommandTypeTimeSignal:
		return "time_signal"
	case SCTE35SpliceCommandTypeBandwidthReservation:
		return "bandwidth_reservation"
	case SCTE35SpliceCommandTypePrivateCommand:
		return "private_command"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(t))
	}
}

// SCTE35SpliceInfoSection represents a SCTE-35 splice_info_section.
// All PTS-like fields are stored on their native 33-bit range so every value
// is smaller than 2^33; wrapping is handled with SCTE35PTSModulo.
type SCTE35SpliceInfoSection struct {
	TableID          uint8 // Always 0xFC for a splice_info_section.
	SAPType          uint8 // 2-bit stream_access_point_type, in range 0..3
	PrivateIndicator bool
	SectionLength    uint16 // Parsed from the wire; recomputed when writing.
	ProtocolVersion  uint8

	EncryptedPacket     bool
	EncryptionAlgorithm uint8
	PTSAdjustment       uint64 // 33 bits, 90 kHz units
	CWIndex             uint8
	Tier                uint16 // 12 bits

	SpliceCommandLength uint16 // Parsed from the wire; recomputed when writing.
	SpliceCommand       *SCTE35SpliceCommand

	DescriptorLoopLength uint16 // Parsed from the wire; recomputed when writing.
	Descriptors          []*SCTE35SpliceDescriptor

	CRC32 uint32
}

// SCTE35SpliceCommand represents a splice command. Known commands expose a
// typed pointer; commands unknown to this library are preserved byte for byte
// in Raw so that writing the section back is lossless.
type SCTE35SpliceCommand struct {
	Type           SCTE35SpliceCommandType
	SpliceInsert   *SCTE35SpliceInsert
	TimeSignal     *SCTE35SpliceTime
	SpliceSchedule *SCTE35SpliceSchedule
	Private        *SCTE35PrivateCommand
	// Raw holds the command payload following splice_command_type for command
	// types this library does not model.
	Raw []byte
}

// SCTE35SpliceInsert represents a splice_insert() command.
type SCTE35SpliceInsert struct {
	SpliceEventID              uint32
	SpliceEventCancelIndicator bool

	OutOfNetworkIndicator bool
	ProgramSpliceFlag     bool
	DurationFlag          bool
	SpliceImmediateFlag   bool

	// SpliceTime is set when ProgramSpliceFlag is set and SpliceImmediateFlag is clear.
	SpliceTime    *SCTE35SpliceTime
	Components    []*SCTE35SpliceComponent
	BreakDuration *SCTE35BreakDuration

	UniqueProgramID uint16
	AvailNum        uint8
	AvailsExpected  uint8
}

// SCTE35SpliceComponent represents a component-level splice point of a
// splice_insert command.
type SCTE35SpliceComponent struct {
	ComponentTag uint8
	// SpliceTime is nil when the command carries splice_immediate_flag.
	SpliceTime *SCTE35SpliceTime
}

// SCTE35SpliceTime represents a splice_time() structure.
type SCTE35SpliceTime struct {
	TimeSpecified bool
	// PTS holds the raw 33-bit pts_time; only meaningful when TimeSpecified is true.
	PTS uint64
}

// SCTE35BreakDuration represents a break_duration() structure.
type SCTE35BreakDuration struct {
	AutoReturn bool
	// Duration is a 33-bit value expressed in 90 kHz units.
	Duration uint64
}

// SCTE35SpliceSchedule represents a splice_schedule() command.
type SCTE35SpliceSchedule struct {
	Events []*SCTE35SpliceScheduleEvent
}

// SCTE35SpliceScheduleEvent represents one event of a splice_schedule command.
type SCTE35SpliceScheduleEvent struct {
	SpliceEventID              uint32
	SpliceEventCancelIndicator bool

	OutOfNetworkIndicator bool
	ProgramSpliceFlag     bool
	DurationFlag          bool

	// UTCSpliceTime is set when ProgramSpliceFlag is set.
	UTCSpliceTime uint32
	Components    []*SCTE35SpliceScheduleComponent
	BreakDuration *SCTE35BreakDuration

	UniqueProgramID uint16
	AvailNum        uint8
	AvailsExpected  uint8
}

// SCTE35SpliceScheduleComponent represents a component-level scheduled event.
type SCTE35SpliceScheduleComponent struct {
	ComponentTag  uint8
	UTCSpliceTime uint32
}

// SCTE35PrivateCommand represents a private_command.
type SCTE35PrivateCommand struct {
	Identifier uint32
	// Raw holds the private payload following the identifier.
	Raw []byte
}

// SCTE35SpliceDescriptor represents a splice_descriptor. A segmentation
// descriptor is exposed through Segmentation; every other (or malformed)
// descriptor is preserved in Raw without information loss.
type SCTE35SpliceDescriptor struct {
	Tag          uint8
	Length       uint8
	Segmentation *SCTE35SegmentationDescriptor
	// Raw holds the descriptor payload following splice_descriptor_tag and
	// descriptor_length when Segmentation is nil.
	Raw []byte
}

// SCTE35SegmentationDescriptor represents a segmentation_descriptor().
type SCTE35SegmentationDescriptor struct {
	Identifier                       uint32 // 0x43554549 ("CUEI")
	SegmentationEventID              uint32
	SegmentationEventCancelIndicator bool
	// SegmentationEventIDComplianceIndicator mirrors
	// segmentation_event_id_compliance_indicator.
	SegmentationEventIDComplianceIndicator bool

	ProgramSegmentationFlag  bool
	SegmentationDurationFlag bool
	// DeliveryNotRestrictedFlag mirrors delivery_not_restricted_flag. When true,
	// the web/blackout/archive/restriction fields are reserved on the wire.
	DeliveryNotRestrictedFlag bool
	WebDeliveryAllowedFlag    bool
	NoRegionalBlackoutFlag    bool
	ArchiveAllowedFlag        bool
	DeviceRestrictions        uint8

	Components []*SCTE35SegmentationComponent

	// SegmentationDuration is a 33-bit 90 kHz value; zero when
	// SegmentationDurationFlag is clear.
	SegmentationDuration uint64

	SegmentationUPIDType   uint8
	SegmentationUPIDLength uint8
	SegmentationUPID       []byte

	SegmentationTypeID uint8
	SegmentNum         uint8
	SegmentsExpected   uint8

	// SubSegmentNum and SubSegmentsExpected are only present on the wire for
	// segmentation type ids 0x34, 0x36, 0x38 and 0x3A.
	SubSegmentNum       uint8
	SubSegmentsExpected uint8
}

// SCTE35SegmentationComponent represents a component of a segmentation
// descriptor that does not carry program_segmentation_flag.
type SCTE35SegmentationComponent struct {
	ComponentTag               uint8
	SegmentationDurationOffset uint64 // 33-bit signed pts_offset, stored raw
}

// scte35BitReader is a bounds-checked big-endian bit reader. Any truncated
// read is remembered in err and turns into a parse error at the end.
type scte35BitReader struct {
	bs  []byte
	off uint // current bit offset
	err error
}

func newSCTE35BitReader(bs []byte) *scte35BitReader {
	return &scte35BitReader{bs: bs}
}

func (r *scte35BitReader) readBits(n uint) uint64 {
	if r.err != nil || n == 0 {
		return 0
	}
	if r.off+n > uint(len(r.bs))*8 {
		r.err = fmt.Errorf("astits: SCTE-35 truncated input: need %d bits at offset %d, have %d bytes", n, r.off, len(r.bs))
		return 0
	}
	var v uint64
	for i := uint(0); i < n; i++ {
		byteIdx := (r.off + i) / 8
		bitIdx := 7 - ((r.off + i) % 8)
		v = v<<1 | uint64((r.bs[byteIdx]>>bitIdx)&0x1)
	}
	r.off += n
	return v
}

func (r *scte35BitReader) readByte() uint8 {
	return uint8(r.readBits(8))
}

func (r *scte35BitReader) readBytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if r.off%8 != 0 {
		r.err = fmt.Errorf("astits: SCTE-35 internal error: byte read on unaligned offset %d", r.off)
		return nil
	}
	if r.off/8+uint(n) > uint(len(r.bs)) {
		r.err = fmt.Errorf("astits: SCTE-35 truncated input: need %d bytes at offset %d, have %d", n, r.off/8, len(r.bs))
		return nil
	}
	start := r.off / 8
	out := make([]byte, n)
	copy(out, r.bs[start:start+uint(n)])
	r.off += uint(n) * 8
	return out
}

func (r *scte35BitReader) byteOffset() int {
	return int(r.off / 8)
}

func (r *scte35BitReader) bitsLeft() int {
	return len(r.bs)*8 - int(r.off)
}

// ParseSCTE35SpliceInfoSectionBytes parses one splice_info_section, from the
// table_id byte through the CRC32. The input must contain exactly one section
// (trailing bytes are rejected).
func ParseSCTE35SpliceInfoSectionBytes(b []byte) (*SCTE35SpliceInfoSection, error) {
	i := astikit.NewBytesIterator(b)
	return parseSCTE35SpliceInfoSection(i, len(b))
}

// parseSCTE35SpliceInfoSection parses a section from an iterator. totalLength
// bounds the section; bytes beyond the declared section_length are rejected.
func parseSCTE35SpliceInfoSection(i *astikit.BytesIterator, totalLength int) (*SCTE35SpliceInfoSection, error) {
	if totalLength < 3 {
		return nil, fmt.Errorf("%w: section shorter than 3 bytes", ErrSCTE35InvalidSection)
	}

	// Table id
	tableID, err := i.NextByte()
	if err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 table_id failed: %w", err)
	}
	if tableID != scte35TableID {
		return nil, fmt.Errorf("%w: table_id 0x%02x is not 0x%02x", ErrSCTE35InvalidSection, tableID, scte35TableID)
	}

	// Section header
	hdr, err := i.NextBytesNoCopy(2)
	if err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 section header failed: %w", err)
	}
	sectionSyntaxIndicator := hdr[0]&0x80 > 0
	privateIndicator := hdr[0]&0x40 > 0
	sapType := (hdr[0] & 0x30) >> 4
	sectionLength := uint16(hdr[0]&0x0f)<<8 | uint16(hdr[1])

	if sectionSyntaxIndicator {
		return nil, fmt.Errorf("%w: section_syntax_indicator must be 0", ErrSCTE35InvalidSection)
	}
	if privateIndicator {
		return nil, fmt.Errorf("%w: private_indicator must be 0", ErrSCTE35InvalidSection)
	}
	if sectionLength < scte35MinSectionLength {
		return nil, fmt.Errorf("%w: section_length %d is smaller than minimum %d", ErrSCTE35InvalidSection, sectionLength, scte35MinSectionLength)
	}
	if sectionLength > scte35MaxSectionLength {
		return nil, fmt.Errorf("%w: section_length %d exceeds maximum %d", ErrSCTE35InvalidSection, sectionLength, scte35MaxSectionLength)
	}
	if int(sectionLength)+3 > totalLength {
		return nil, fmt.Errorf("%w: section_length %d exceeds %d available bytes", ErrSCTE35InvalidSection, sectionLength, totalLength-3)
	}

	// Bound every following read to the declared section so overruns cannot
	// escape into adjacent data.
	sectionBytes, err := i.NextBytes(int(sectionLength))
	if err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 section body failed: %w", err)
	}

	// CRC32 covers table_id, the two header bytes and the whole section body
	// except the trailing CRC32.
	crcOffset := len(sectionBytes) - 4
	if crcOffset < 0 {
		return nil, fmt.Errorf("%w: section too short for CRC32", ErrSCTE35InvalidSection)
	}
	headerAndBody := make([]byte, 0, 3+crcOffset)
	headerAndBody = append(headerAndBody, tableID, hdr[0], hdr[1])
	headerAndBody = append(headerAndBody, sectionBytes[:crcOffset]...)
	wantCRC := computeCRC32(headerAndBody)
	gotCRC := uint32(sectionBytes[crcOffset])<<24 | uint32(sectionBytes[crcOffset+1])<<16 |
		uint32(sectionBytes[crcOffset+2])<<8 | uint32(sectionBytes[crcOffset+3])
	if wantCRC != gotCRC {
		return nil, errors.Join(ErrCRC32Mismatch, fmt.Errorf("astits: SCTE-35 section CRC32 %08x != computed CRC32 %08x", gotCRC, wantCRC))
	}

	d := &SCTE35SpliceInfoSection{
		TableID:       tableID,
		SectionLength: sectionLength,
		SAPType:       sapType,
	}
	r := newSCTE35BitReader(sectionBytes[:crcOffset])

	// protocol_version
	d.ProtocolVersion = r.readByte()
	if r.err == nil && d.ProtocolVersion != scte35ProtocolVersion {
		return nil, fmt.Errorf("%w: unsupported protocol_version %d", ErrSCTE35InvalidSection, d.ProtocolVersion)
	}

	// encrypted_packet(1) + encryption_algorithm(6) + pts_adjustment(33)
	d.EncryptedPacket = r.readBits(1) == 1
	d.EncryptionAlgorithm = uint8(r.readBits(6))
	if r.err == nil && d.EncryptionAlgorithm != 0 {
		return nil, fmt.Errorf("%w: encrypted SCTE-35 sections (encryption_algorithm %d) are not supported", ErrSCTE35InvalidSection, d.EncryptionAlgorithm)
	}
	d.PTSAdjustment = r.readBits(33)

	// cw_index
	d.CWIndex = r.readByte()

	// tier(12) + splice_command_length(12)
	d.Tier = uint16(r.readBits(12))
	d.SpliceCommandLength = uint16(r.readBits(12))
	if r.err == nil && d.SpliceCommandLength == 0x0fff {
		return nil, fmt.Errorf("%w: splice_command_length 0x0fff is reserved", ErrSCTE35InvalidSection)
	}

	// Splice command
	cmdStart := r.byteOffset()
	if d.SpliceCommand, err = parseSCTE35SpliceCommand(r, d.SpliceCommandLength); err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice command failed: %w", r.err)
	}
	// parseSCTE35SpliceCommand consumes the splice_command_type byte followed
	// by exactly splice_command_length payload bytes.
	if r.byteOffset()-cmdStart != int(d.SpliceCommandLength)+1 {
		return nil, fmt.Errorf("%w: splice command consumed %d payload bytes, splice_command_length is %d",
			ErrSCTE35InvalidSection, r.byteOffset()-cmdStart-1, d.SpliceCommandLength)
	}

	// Descriptor loop
	d.DescriptorLoopLength = uint16(r.readBits(16))
	if r.err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 descriptor loop length failed: %w", r.err)
	}
	if r.err == nil && int(d.DescriptorLoopLength) > r.bitsLeft()/8 {
		return nil, fmt.Errorf("%w: descriptor_loop_length %d exceeds section bounds", ErrSCTE35InvalidSection, d.DescriptorLoopLength)
	}
	descEnd := r.byteOffset() + int(d.DescriptorLoopLength)
	for r.err == nil && r.byteOffset() < descEnd {
		var desc *SCTE35SpliceDescriptor
		if desc, err = parseSCTE35SpliceDescriptor(r); err != nil {
			return nil, err
		}
		d.Descriptors = append(d.Descriptors, desc)
	}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 descriptors failed: %w", r.err)
	}
	if r.byteOffset() != descEnd {
		return nil, fmt.Errorf("%w: descriptor loop ended at offset %d, expected %d", ErrSCTE35InvalidSection, r.byteOffset(), descEnd)
	}

	// Everything between the descriptor loop and the CRC must be consumed.
	if r.bitsLeft() != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes before CRC32", ErrSCTE35InvalidSection, r.bitsLeft()/8)
	}
	d.CRC32 = gotCRC
	return d, nil
}

func parseSCTE35SpliceCommand(r *scte35BitReader, commandLength uint16) (*SCTE35SpliceCommand, error) {
	cmd := &SCTE35SpliceCommand{Type: SCTE35SpliceCommandType(r.readByte())}
	if r.err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 splice_command_type failed: %w", r.err)
	}

	switch cmd.Type {
	case SCTE35SpliceCommandTypeSpliceNull, SCTE35SpliceCommandTypeBandwidthReservation:
		// No payload.
	case SCTE35SpliceCommandTypeTimeSignal:
		t, err := parseSCTE35SpliceTime(r)
		if err != nil {
			return nil, err
		}
		cmd.TimeSignal = t
	case SCTE35SpliceCommandTypeSpliceInsert:
		si, err := parseSCTE35SpliceInsert(r)
		if err != nil {
			return nil, err
		}
		cmd.SpliceInsert = si
	case SCTE35SpliceCommandTypeSpliceSchedule:
		ss, err := parseSCTE35SpliceSchedule(r)
		if err != nil {
			return nil, err
		}
		cmd.SpliceSchedule = ss
	case SCTE35SpliceCommandTypePrivateCommand:
		pc := &SCTE35PrivateCommand{}
		pc.Identifier = uint32(r.readBits(32))
		// splice_command_length counts the identifier and the private payload.
		pc.Raw = r.readBytes(int(commandLength) - 4)
		if r.err != nil {
			return nil, fmt.Errorf("astits: parsing SCTE-35 private command failed: %w", r.err)
		}
		cmd.Private = pc
	default:
		// Unknown command: keep the payload untouched for lossless write-back.
		// The splice_command_type byte was already consumed, so only the
		// remaining commandLength bytes belong to the raw payload.
		cmd.Raw = r.readBytes(int(commandLength))
		if r.err != nil {
			return nil, fmt.Errorf("astits: preserving unknown SCTE-35 command failed: %w", r.err)
		}
	}
	return cmd, nil
}

func parseSCTE35SpliceTime(r *scte35BitReader) (*SCTE35SpliceTime, error) {
	t := &SCTE35SpliceTime{TimeSpecified: r.readBits(1) == 1}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_time failed: %w", r.err)
	}
	if t.TimeSpecified {
		// time_specified_flag(1) + reserved(6) + pts_time(33) = 40 bits
		if r.readBits(6) != 0x3f {
			return nil, fmt.Errorf("%w: reserved bits of splice_time must be 0b111111", ErrSCTE35InvalidSection)
		}
		t.PTS = r.readBits(33)
	} else if r.readBits(7) != 0x7f {
		// No pts_time: time_specified_flag(1) + reserved(7) = 8 bits
		return nil, fmt.Errorf("%w: reserved bits of splice_time without pts_time must be 0b1111111", ErrSCTE35InvalidSection)
	}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_time failed: %w", r.err)
	}
	return t, nil
}

func parseSCTE35BreakDuration(r *scte35BitReader) (*SCTE35BreakDuration, error) {
	bd := &SCTE35BreakDuration{AutoReturn: r.readBits(1) == 1}
	if r.readBits(6) != 0x3f {
		return nil, fmt.Errorf("%w: reserved bits of break_duration must be 0b111111", ErrSCTE35InvalidSection)
	}
	bd.Duration = r.readBits(33)
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 break_duration failed: %w", r.err)
	}
	return bd, nil
}

func parseSCTE35SpliceInsert(r *scte35BitReader) (*SCTE35SpliceInsert, error) {
	si := &SCTE35SpliceInsert{}
	si.SpliceEventID = uint32(r.readBits(32))
	si.SpliceEventCancelIndicator = r.readBits(1) == 1
	if r.readBits(7) != 0x7f {
		return nil, fmt.Errorf("%w: reserved bits of splice_insert must be 0b1111111", ErrSCTE35InvalidSection)
	}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_insert failed: %w", r.err)
	}

	if si.SpliceEventCancelIndicator {
		return si, nil
	}

	si.OutOfNetworkIndicator = r.readBits(1) == 1
	si.ProgramSpliceFlag = r.readBits(1) == 1
	si.DurationFlag = r.readBits(1) == 1
	si.SpliceImmediateFlag = r.readBits(1) == 1
	if r.readBits(4) != 0xf {
		return nil, fmt.Errorf("%w: reserved nibble of splice_insert must be 0b1111", ErrSCTE35InvalidSection)
	}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_insert flags failed: %w", r.err)
	}

	if si.ProgramSpliceFlag && !si.SpliceImmediateFlag {
		t, err := parseSCTE35SpliceTime(r)
		if err != nil {
			return nil, err
		}
		si.SpliceTime = t
	}

	if !si.ProgramSpliceFlag {
		componentCount := int(r.readByte())
		if r.err != nil {
			return nil, fmt.Errorf("astits: parsing SCTE-35 splice_insert component count failed: %w", r.err)
		}
		for j := 0; j < componentCount; j++ {
			c := &SCTE35SpliceComponent{ComponentTag: r.readByte()}
			if !si.SpliceImmediateFlag {
				t, err := parseSCTE35SpliceTime(r)
				if err != nil {
					return nil, err
				}
				c.SpliceTime = t
			}
			si.Components = append(si.Components, c)
		}
	}

	if si.DurationFlag {
		bd, err := parseSCTE35BreakDuration(r)
		if err != nil {
			return nil, err
		}
		si.BreakDuration = bd
	}

	si.UniqueProgramID = uint16(r.readBits(16))
	si.AvailNum = r.readByte()
	si.AvailsExpected = r.readByte()
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_insert tail failed: %w", r.err)
	}
	return si, nil
}

func parseSCTE35SpliceSchedule(r *scte35BitReader) (*SCTE35SpliceSchedule, error) {
	ss := &SCTE35SpliceSchedule{}
	eventCount := int(r.readByte())
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_schedule event count failed: %w", r.err)
	}
	for j := 0; j < eventCount; j++ {
		e := &SCTE35SpliceScheduleEvent{}
		e.SpliceEventID = uint32(r.readBits(32))
		e.SpliceEventCancelIndicator = r.readBits(1) == 1
		if r.readBits(7) != 0x7f {
			return nil, fmt.Errorf("%w: reserved bits of splice_schedule event must be 0b1111111", ErrSCTE35InvalidSection)
		}
		if r.err != nil {
			return nil, fmt.Errorf("astits: parsing SCTE-35 splice_schedule event failed: %w", r.err)
		}

		if !e.SpliceEventCancelIndicator {
			e.OutOfNetworkIndicator = r.readBits(1) == 1
			e.ProgramSpliceFlag = r.readBits(1) == 1
			e.DurationFlag = r.readBits(1) == 1
			if r.readBits(5) != 0x1f {
				return nil, fmt.Errorf("%w: reserved bits of splice_schedule event flags must be 0b11111", ErrSCTE35InvalidSection)
			}
			if e.ProgramSpliceFlag {
				e.UTCSpliceTime = uint32(r.readBits(32))
			} else {
				componentCount := int(r.readByte())
				for k := 0; k < componentCount; k++ {
					c := &SCTE35SpliceScheduleComponent{
						ComponentTag:  r.readByte(),
						UTCSpliceTime: uint32(r.readBits(32)),
					}
					e.Components = append(e.Components, c)
				}
			}
			if e.DurationFlag {
				bd, err := parseSCTE35BreakDuration(r)
				if err != nil {
					return nil, err
				}
				e.BreakDuration = bd
			}
			e.UniqueProgramID = uint16(r.readBits(16))
			e.AvailNum = r.readByte()
			e.AvailsExpected = r.readByte()
		}
		ss.Events = append(ss.Events, e)
	}
	if r.err != nil {
		return nil, fmt.Errorf("astits: parsing SCTE-35 splice_schedule failed: %w", r.err)
	}
	return ss, nil
}

func parseSCTE35SpliceDescriptor(r *scte35BitReader) (*SCTE35SpliceDescriptor, error) {
	d := &SCTE35SpliceDescriptor{Tag: r.readByte()}
	d.Length = r.readByte()
	if r.err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 descriptor header failed: %w", r.err)
	}
	payload := r.readBytes(int(d.Length))
	if r.err != nil {
		return nil, fmt.Errorf("astits: fetching SCTE-35 descriptor payload failed: %w", r.err)
	}

	if d.Tag == SCTE35DescriptorTagSegmentation && int(d.Length) >= 4 {
		identifier := uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
		if identifier == scte35CUEIIdentifier {
			sd, err := parseSCTE35SegmentationDescriptor(payload)
			if err != nil {
				return nil, err
			}
			d.Segmentation = sd
			return d, nil
		}
	}

	// Unknown or unparseable descriptor: preserve verbatim.
	d.Raw = payload
	return d, nil
}

func parseSCTE35SegmentationDescriptor(payload []byte) (*SCTE35SegmentationDescriptor, error) {
	if len(payload) < 13 {
		return nil, fmt.Errorf("%w: segmentation descriptor truncated (%d bytes)", ErrSCTE35InvalidSection, len(payload))
	}
	sd := &SCTE35SegmentationDescriptor{}
	r := newSCTE35BitReader(payload)
	sd.Identifier = uint32(r.readBits(32))
	sd.SegmentationEventID = uint32(r.readBits(32))
	sd.SegmentationEventCancelIndicator = r.readBits(1) == 1
	// segmentation_event_id_compliance_indicator(1) + reserved(6)
	sd.SegmentationEventIDComplianceIndicator = r.readBits(1) == 1
	if r.readBits(6) != 0x3f {
		return nil, fmt.Errorf("%w: reserved bits of segmentation_descriptor must be 0b111111", ErrSCTE35InvalidSection)
	}
	if r.err != nil {
		return nil, r.err
	}

	if sd.SegmentationEventCancelIndicator {
		return sd, nil
	}

	sd.ProgramSegmentationFlag = r.readBits(1) == 1
	sd.SegmentationDurationFlag = r.readBits(1) == 1
	sd.DeliveryNotRestrictedFlag = r.readBits(1) == 1
	if !sd.DeliveryNotRestrictedFlag {
		sd.WebDeliveryAllowedFlag = r.readBits(1) == 1
		sd.NoRegionalBlackoutFlag = r.readBits(1) == 1
		sd.ArchiveAllowedFlag = r.readBits(1) == 1
		sd.DeviceRestrictions = uint8(r.readBits(2))
	} else if r.readBits(5) != 0x1f {
		return nil, fmt.Errorf("%w: reserved delivery flags of segmentation_descriptor must be 0b11111", ErrSCTE35InvalidSection)
	}

	if !sd.ProgramSegmentationFlag {
		componentCount := int(r.readByte())
		for j := 0; j < componentCount; j++ {
			c := &SCTE35SegmentationComponent{ComponentTag: r.readByte()}
			if r.readBits(7) != 0x7f {
				return nil, fmt.Errorf("%w: reserved bits of segmentation component must be 0b1111111", ErrSCTE35InvalidSection)
			}
			c.SegmentationDurationOffset = r.readBits(33)
			sd.Components = append(sd.Components, c)
		}
	}

	if sd.SegmentationDurationFlag {
		if r.readBits(7) != 0x7f {
			return nil, fmt.Errorf("%w: reserved bits before segmentation_duration must be 0b1111111", ErrSCTE35InvalidSection)
		}
		sd.SegmentationDuration = r.readBits(33)
	}

	sd.SegmentationUPIDType = r.readByte()
	sd.SegmentationUPIDLength = r.readByte()
	if r.err != nil {
		return nil, r.err
	}
	sd.SegmentationUPID = r.readBytes(int(sd.SegmentationUPIDLength))
	if r.err != nil {
		return nil, fmt.Errorf("astits: fetching segmentation_upid failed: %w", r.err)
	}

	sd.SegmentationTypeID = r.readByte()
	sd.SegmentNum = r.readByte()
	sd.SegmentsExpected = r.readByte()
	if segmentationTypeHasSubSegments(sd.SegmentationTypeID) {
		sd.SubSegmentNum = r.readByte()
		sd.SubSegmentsExpected = r.readByte()
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.bitsLeft() != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes in segmentation_descriptor", ErrSCTE35InvalidSection, r.bitsLeft()/8)
	}
	return sd, nil
}

// segmentationTypeHasSubSegments reports whether the wire format of the given
// segmentation_type_id carries sub_segment_num and sub_segments_expected.
func segmentationTypeHasSubSegments(typeID uint8) bool {
	switch typeID {
	case SCTE35SegmentationTypeUnscheduledEventStart,
		SCTE35SegmentationTypeUnscheduledEventEnd,
		SCTE35SegmentationTypeNetworkAdvertisementStart,
		SCTE35SegmentationTypeNetworkAdvertisementEnd:
		return true
	}
	return false
}

// parseSCTE35FromPESData extracts the splice_info_section carried by the PES
// packet_data_bytes of an SCTE-35 elementary stream. Some encoders prefix the
// section with a zero pointer byte, which is tolerated.
func parseSCTE35FromPESData(d *PESData) (*SCTE35SpliceInfoSection, error) {
	if d == nil || len(d.Data) == 0 {
		return nil, fmt.Errorf("%w: empty SCTE-35 PES payload", ErrSCTE35InvalidSection)
	}
	payload := d.Data
	if payload[0] != scte35TableID {
		if len(payload) > 1 && payload[0] == 0x00 && payload[1] == scte35TableID {
			payload = payload[1:]
		} else {
			return nil, fmt.Errorf("%w: SCTE-35 PES payload does not start with table_id 0x%02x", ErrSCTE35InvalidSection, scte35TableID)
		}
	}
	s, err := ParseSCTE35SpliceInfoSectionBytes(payload)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// WriteSCTE35SpliceInfoSection serializes a splice_info_section, from the
// table_id byte through the computed CRC32. Length fields and the CRC are
// always derived from the structured content.
func WriteSCTE35SpliceInfoSection(d *SCTE35SpliceInfoSection) ([]byte, error) {
	if d == nil || d.SpliceCommand == nil {
		return nil, fmt.Errorf("%w: missing splice command", ErrSCTE35InvalidSection)
	}
	if d.PTSAdjustment > scte35PTSMask {
		return nil, fmt.Errorf("%w: pts_adjustment %d does not fit on 33 bits", ErrSCTE35InvalidSection, d.PTSAdjustment)
	}
	if d.Tier > 0xfff {
		return nil, fmt.Errorf("%w: tier %d does not fit on 12 bits", ErrSCTE35InvalidSection, d.Tier)
	}
	if d.SAPType > 0x3 {
		return nil, fmt.Errorf("%w: sap_type %d does not fit on 2 bits", ErrSCTE35InvalidSection, d.SAPType)
	}

	cmdBytes, err := writeSCTE35SpliceCommand(d.SpliceCommand)
	if err != nil {
		return nil, err
	}
	descBytes, err := writeSCTE35Descriptors(d.Descriptors)
	if err != nil {
		return nil, err
	}
	if len(descBytes) > 0xffff {
		return nil, fmt.Errorf("%w: descriptor loop too long (%d bytes)", ErrSCTE35InvalidSection, len(descBytes))
	}
	if len(cmdBytes)-1 > 0xffe {
		return nil, fmt.Errorf("%w: splice command too long (%d bytes)", ErrSCTE35InvalidSection, len(cmdBytes)-1)
	}

	// Fixed body: protocol_version(1) + encryption/pts(5) + cw_index(1) +
	// tier/command_length(3) + command + descriptor_loop_length(2) +
	// descriptors + CRC32(4).
	sectionLength := 1 + 5 + 1 + 3 + len(cmdBytes) + 2 + len(descBytes) + 4
	if sectionLength > int(scte35MaxSectionLength) {
		return nil, fmt.Errorf("%w: serialized section_length %d exceeds maximum %d", ErrSCTE35InvalidSection, sectionLength, scte35MaxSectionLength)
	}

	tableID := d.TableID
	if tableID == 0 {
		tableID = scte35TableID
	}
	if tableID != scte35TableID {
		return nil, fmt.Errorf("%w: table_id 0x%02x is not 0x%02x", ErrSCTE35InvalidSection, tableID, scte35TableID)
	}
	sapType := d.SAPType & 0x3

	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	crc := crc32Polynomial
	w.SetWriteCallback(func(bs []byte) {
		crc = updateCRC32(crc, bs)
	})

	b := astikit.NewBitsWriterBatch(w)
	b.Write(tableID)
	b.Write(false) // section_syntax_indicator
	b.Write(d.PrivateIndicator)
	b.WriteN(sapType, 2) // sap_type
	b.WriteN(uint16(sectionLength), 12)

	b.Write(scte35ProtocolVersion)

	b.Write(d.EncryptedPacket)
	b.WriteN(d.EncryptionAlgorithm, 6)
	b.WriteN(d.PTSAdjustment, 33)

	b.Write(d.CWIndex)

	b.WriteN(d.Tier, 12)
	b.WriteN(uint16(len(cmdBytes)-1), 12)

	if err := b.Err(); err != nil {
		return nil, err
	}
	if err := w.Write(cmdBytes); err != nil {
		return nil, err
	}

	b.Write(uint16(len(descBytes)))
	if err := b.Err(); err != nil {
		return nil, err
	}
	if err := w.Write(descBytes); err != nil {
		return nil, err
	}

	// Switch the callback off before emitting the CRC itself.
	w.SetWriteCallback(nil)
	if err := w.Write(crc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeSCTE35SpliceCommand(cmd *SCTE35SpliceCommand) ([]byte, error) {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	b := astikit.NewBitsWriterBatch(w)
	b.Write(uint8(cmd.Type))

	switch cmd.Type {
	case SCTE35SpliceCommandTypeSpliceNull, SCTE35SpliceCommandTypeBandwidthReservation:
	case SCTE35SpliceCommandTypeTimeSignal:
		if cmd.TimeSignal == nil {
			return nil, fmt.Errorf("%w: time_signal command missing splice_time", ErrSCTE35InvalidSection)
		}
		if err := writeSCTE35SpliceTime(w, cmd.TimeSignal); err != nil {
			return nil, err
		}
	case SCTE35SpliceCommandTypeSpliceInsert:
		if cmd.SpliceInsert == nil {
			return nil, fmt.Errorf("%w: splice_insert command missing payload", ErrSCTE35InvalidSection)
		}
		if err := writeSCTE35SpliceInsert(w, cmd.SpliceInsert); err != nil {
			return nil, err
		}
	case SCTE35SpliceCommandTypeSpliceSchedule:
		if cmd.SpliceSchedule == nil {
			return nil, fmt.Errorf("%w: splice_schedule command missing payload", ErrSCTE35InvalidSection)
		}
		if err := writeSCTE35SpliceSchedule(w, cmd.SpliceSchedule); err != nil {
			return nil, err
		}
	case SCTE35SpliceCommandTypePrivateCommand:
		if cmd.Private == nil {
			return nil, fmt.Errorf("%w: private_command missing payload", ErrSCTE35InvalidSection)
		}
		b.Write(cmd.Private.Identifier)
		if err := b.Err(); err != nil {
			return nil, err
		}
		if err := w.Write(cmd.Private.Raw); err != nil {
			return nil, err
		}
	default:
		if err := w.Write(cmd.Raw); err != nil {
			return nil, err
		}
	}
	if err := b.Err(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeSCTE35SpliceTime(w *astikit.BitsWriter, t *SCTE35SpliceTime) error {
	b := astikit.NewBitsWriterBatch(w)
	b.Write(t.TimeSpecified)
	if t.TimeSpecified {
		if t.PTS > scte35PTSMask {
			return fmt.Errorf("%w: pts_time %d does not fit on 33 bits", ErrSCTE35InvalidSection, t.PTS)
		}
		b.WriteN(uint8(0x3f), 6)
		b.WriteN(t.PTS, 33)
	} else {
		b.WriteN(uint8(0x7f), 7)
	}
	return b.Err()
}

func writeSCTE35BreakDuration(w *astikit.BitsWriter, bd *SCTE35BreakDuration) error {
	if bd.Duration > scte35PTSMask {
		return fmt.Errorf("%w: break_duration %d does not fit on 33 bits", ErrSCTE35InvalidSection, bd.Duration)
	}
	b := astikit.NewBitsWriterBatch(w)
	b.Write(bd.AutoReturn)
	b.WriteN(uint8(0x3f), 6) // reserved
	b.WriteN(bd.Duration, 33)
	return b.Err()
}

func writeSCTE35SpliceInsert(w *astikit.BitsWriter, si *SCTE35SpliceInsert) error {
	b := astikit.NewBitsWriterBatch(w)
	b.Write(si.SpliceEventID)
	b.Write(si.SpliceEventCancelIndicator)
	b.WriteN(uint8(0x7f), 7)
	if err := b.Err(); err != nil {
		return err
	}
	if si.SpliceEventCancelIndicator {
		return nil
	}

	b.Write(si.OutOfNetworkIndicator)
	b.Write(si.ProgramSpliceFlag)
	b.Write(si.DurationFlag)
	b.Write(si.SpliceImmediateFlag)
	b.WriteN(uint8(0xf), 4)
	if err := b.Err(); err != nil {
		return err
	}

	if si.ProgramSpliceFlag && !si.SpliceImmediateFlag {
		if si.SpliceTime == nil {
			return fmt.Errorf("%w: program splice_insert missing splice_time", ErrSCTE35InvalidSection)
		}
		if err := writeSCTE35SpliceTime(w, si.SpliceTime); err != nil {
			return err
		}
	}

	if !si.ProgramSpliceFlag {
		b.Write(uint8(len(si.Components)))
		if err := b.Err(); err != nil {
			return err
		}
		for _, c := range si.Components {
			b.Write(c.ComponentTag)
			if err := b.Err(); err != nil {
				return err
			}
			if !si.SpliceImmediateFlag {
				if c.SpliceTime == nil {
					return fmt.Errorf("%w: component splice_insert missing splice_time", ErrSCTE35InvalidSection)
				}
				if err := writeSCTE35SpliceTime(w, c.SpliceTime); err != nil {
					return err
				}
			}
		}
	}

	if si.DurationFlag {
		if si.BreakDuration == nil {
			return fmt.Errorf("%w: splice_insert with duration_flag missing break_duration", ErrSCTE35InvalidSection)
		}
		if err := writeSCTE35BreakDuration(w, si.BreakDuration); err != nil {
			return err
		}
	}

	b.Write(si.UniqueProgramID)
	b.Write(si.AvailNum)
	b.Write(si.AvailsExpected)
	return b.Err()
}

func writeSCTE35SpliceSchedule(w *astikit.BitsWriter, ss *SCTE35SpliceSchedule) error {
	b := astikit.NewBitsWriterBatch(w)
	b.Write(uint8(len(ss.Events)))
	if err := b.Err(); err != nil {
		return err
	}
	for _, e := range ss.Events {
		b.Write(e.SpliceEventID)
		b.Write(e.SpliceEventCancelIndicator)
		b.WriteN(uint8(0x7f), 7)
		if err := b.Err(); err != nil {
			return err
		}
		if e.SpliceEventCancelIndicator {
			continue
		}

		b.Write(e.OutOfNetworkIndicator)
		b.Write(e.ProgramSpliceFlag)
		b.Write(e.DurationFlag)
		b.WriteN(uint8(0x1f), 5)
		if e.ProgramSpliceFlag {
			b.Write(e.UTCSpliceTime)
		} else {
			b.Write(uint8(len(e.Components)))
			for _, c := range e.Components {
				b.Write(c.ComponentTag)
				b.Write(c.UTCSpliceTime)
			}
		}
		if e.DurationFlag {
			if e.BreakDuration == nil {
				return fmt.Errorf("%w: splice_schedule event with duration_flag missing break_duration", ErrSCTE35InvalidSection)
			}
			if err := b.Err(); err != nil {
				return err
			}
			if err := writeSCTE35BreakDuration(w, e.BreakDuration); err != nil {
				return err
			}
		}
		b.Write(e.UniqueProgramID)
		b.Write(e.AvailNum)
		b.Write(e.AvailsExpected)
		if err := b.Err(); err != nil {
			return err
		}
	}
	return b.Err()
}

func writeSCTE35Descriptors(ds []*SCTE35SpliceDescriptor) ([]byte, error) {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	for _, d := range ds {
		var payload []byte
		var err error
		if d.Segmentation != nil {
			if payload, err = writeSCTE35SegmentationDescriptor(d.Segmentation); err != nil {
				return nil, err
			}
		} else {
			payload = d.Raw
		}
		if len(payload) > 0xff {
			return nil, fmt.Errorf("%w: descriptor payload too long (%d bytes)", ErrSCTE35InvalidSection, len(payload))
		}
		b := astikit.NewBitsWriterBatch(w)
		b.Write(d.Tag)
		b.Write(uint8(len(payload)))
		if err := b.Err(); err != nil {
			return nil, err
		}
		if err := w.Write(payload); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func writeSCTE35SegmentationDescriptor(sd *SCTE35SegmentationDescriptor) ([]byte, error) {
	if sd.SegmentationDuration > scte35PTSMask {
		return nil, fmt.Errorf("%w: segmentation_duration %d does not fit on 33 bits", ErrSCTE35InvalidSection, sd.SegmentationDuration)
	}
	if int(sd.SegmentationUPIDLength) != len(sd.SegmentationUPID) {
		return nil, fmt.Errorf("%w: segmentation_upid_length %d does not match %d upid bytes", ErrSCTE35InvalidSection, sd.SegmentationUPIDLength, len(sd.SegmentationUPID))
	}

	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	b := astikit.NewBitsWriterBatch(w)
	identifier := sd.Identifier
	if identifier == 0 {
		identifier = scte35CUEIIdentifier
	}
	b.Write(identifier)
	b.Write(sd.SegmentationEventID)
	b.Write(sd.SegmentationEventCancelIndicator)
	b.Write(sd.SegmentationEventIDComplianceIndicator)
	b.WriteN(uint8(0x3f), 6) // reserved
	if err := b.Err(); err != nil {
		return nil, err
	}
	if sd.SegmentationEventCancelIndicator {
		return buf.Bytes(), nil
	}

	b.Write(sd.ProgramSegmentationFlag)
	b.Write(sd.SegmentationDurationFlag)
	b.Write(sd.DeliveryNotRestrictedFlag)
	if !sd.DeliveryNotRestrictedFlag {
		b.Write(sd.WebDeliveryAllowedFlag)
		b.Write(sd.NoRegionalBlackoutFlag)
		b.Write(sd.ArchiveAllowedFlag)
		b.WriteN(sd.DeviceRestrictions, 2)
	} else {
		b.WriteN(uint8(0x1f), 5)
	}

	if !sd.ProgramSegmentationFlag {
		b.Write(uint8(len(sd.Components)))
		if err := b.Err(); err != nil {
			return nil, err
		}
		for _, c := range sd.Components {
			if c.SegmentationDurationOffset > scte35PTSMask {
				return nil, fmt.Errorf("%w: pts_offset %d does not fit on 33 bits", ErrSCTE35InvalidSection, c.SegmentationDurationOffset)
			}
			b.Write(c.ComponentTag)
			b.WriteN(uint8(0x7f), 7)
			b.WriteN(c.SegmentationDurationOffset, 33)
		}
	}

	if sd.SegmentationDurationFlag {
		b.WriteN(uint8(0x7f), 7) // reserved
		b.WriteN(sd.SegmentationDuration, 33)
	}

	b.Write(sd.SegmentationUPIDType)
	b.Write(sd.SegmentationUPIDLength)
	if err := b.Err(); err != nil {
		return nil, err
	}
	if err := w.Write(sd.SegmentationUPID); err != nil {
		return nil, err
	}

	b.Write(sd.SegmentationTypeID)
	b.Write(sd.SegmentNum)
	b.Write(sd.SegmentsExpected)
	if segmentationTypeHasSubSegments(sd.SegmentationTypeID) {
		b.Write(sd.SubSegmentNum)
		b.Write(sd.SubSegmentsExpected)
	}
	if err := b.Err(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SCTE35NormalizePTS reduces a PTS value to its 33-bit representation.
func SCTE35NormalizePTS(pts uint64) uint64 {
	return pts & scte35PTSMask
}

// SCTE35AdjustedPTS applies pts_adjustment to a pts_time following the
// protocol: the addition is performed modulo 2^33, so wrap-around is handled
// transparently.
func SCTE35AdjustedPTS(ptsAdjustment, pts uint64) uint64 {
	return (ptsAdjustment + pts) & scte35PTSMask
}

// AdjustedPTS returns the adjusted splice time of the section.
func (d *SCTE35SpliceInfoSection) AdjustedPTS(pts uint64) uint64 {
	return SCTE35AdjustedPTS(d.PTSAdjustment, pts)
}
