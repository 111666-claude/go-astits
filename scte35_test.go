package astits

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refSCTE35Builder is an independent, straightforward bit packer used to
// build splice_info_section fixtures without reusing the production writer,
// so parser and writer cannot share a bug.
type refSCTE35Builder struct {
	buf       bytes.Buffer
	w         *astikit.BitsWriter
	lenOffset int // byte offset of the section_length placeholder
}

func newRefSCTE35Builder() *refSCTE35Builder {
	b := &refSCTE35Builder{}
	b.w = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &b.buf})
	return b
}

// writeHeader writes table_id and the two section header bytes with a
// section_length placeholder that finish() patches.
func (b *refSCTE35Builder) writeHeader() {
	b.writeHeaderWithSAP(0)
}

// writeHeaderWithSAP writes the header with an explicit 2-bit sap_type.
func (b *refSCTE35Builder) writeHeaderWithSAP(sapType uint8) {
	bw := astikit.NewBitsWriterBatch(b.w)
	bw.Write(uint8(0xfc))
	bw.Write(false)              // section_syntax_indicator
	bw.Write(false)              // private_indicator
	bw.WriteN(uint8(sapType), 2) // sap_type
	b.lenOffset = b.buf.Len()
	bw.WriteN(uint16(0), 12) // section_length placeholder
	if err := bw.Err(); err != nil {
		panic(err)
	}
}

// finish patches the section_length, computes the CRC32 over the complete
// section-to-be and returns the section with the CRC appended.
func (b *refSCTE35Builder) finish() []byte {
	// section_length counts every byte after the two section_length bytes,
	// i.e. all currently buffered bytes except the 3-byte table header, plus
	// the 4-byte CRC about to be written.
	bodyLen := b.buf.Len() - 3 + 4
	sectionBytes := b.buf.Bytes()
	// Byte at lenOffset holds section_syntax/private/sap in its high nibble;
	// only patch the 12-bit section_length in the low nibble and next byte.
	sectionBytes[b.lenOffset] = (sectionBytes[b.lenOffset] & 0xf0) | byte(bodyLen>>8)
	sectionBytes[b.lenOffset+1] = byte(bodyLen)

	crc := computeCRC32(sectionBytes)
	if err := b.w.Write(crc); err != nil {
		panic(err)
	}
	return b.buf.Bytes()
}

// refTimeSignalSection builds a time_signal section with a 33-bit
// pts_adjustment and pts_time that exercise PTS values beyond 2^32.
func refTimeSignalSection(t require.TestingT) []byte {
	b := newRefSCTE35Builder()
	w := b.w
	bw := astikit.NewBitsWriterBatch(w)

	// Header
	b.writeHeader()

	// Protocol
	bw.Write(uint8(0))

	// encrypted_packet(1) + encryption_algorithm(6) + pts_adjustment(33)
	bw.Write(false)
	bw.WriteN(uint8(0), 6)
	bw.WriteN(uint64(0x100000001), 33)

	// cw_index
	bw.Write(uint8(0))

	// tier(12) + splice_command_length(12)
	bw.WriteN(uint16(1), 12)
	bw.WriteN(uint16(5), 12)

	// time_signal: splice_command_type
	bw.Write(uint8(0x06))
	// splice_time: time_specified(1) + reserved(6) + pts_time(33) = 40 bits
	bw.Write(true)
	bw.WriteN(uint8(0x3f), 6)
	bw.WriteN(uint64(0x100000002), 33)

	// descriptor loop (empty)
	bw.Write(uint16(0))
	require.NoError(t, bw.Err())

	return b.finish()
}

func TestParseSCTE35TimeSignal_ReferenceFixture(t *testing.T) {
	raw := refTimeSignalSection(t)
	// 3 header + proto(1) + enc/pts(5) + cw(1) + tier/cmdlen(3) +
	// command type(1) + splice_time(5) + loop length(2) + CRC(4) = 25.
	require.Equal(t, 25, len(raw))
	require.Equal(t, 22, int(raw[1]&0x0f)<<8|int(raw[2]))

	d, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)

	assert.Equal(t, uint8(0xfc), d.TableID)
	assert.Equal(t, uint8(0), d.SAPType)
	assert.False(t, d.PrivateIndicator)
	assert.Equal(t, uint16(22), d.SectionLength)
	assert.Equal(t, uint8(0), d.ProtocolVersion)
	assert.False(t, d.EncryptedPacket)
	assert.Equal(t, uint8(0), d.EncryptionAlgorithm)
	assert.Equal(t, uint64(0x100000001), d.PTSAdjustment)
	assert.Equal(t, uint8(0), d.CWIndex)
	assert.Equal(t, uint16(1), d.Tier)
	assert.Equal(t, uint16(5), d.SpliceCommandLength)
	assert.Empty(t, d.Descriptors)
	assert.NotZero(t, d.CRC32)

	require.NotNil(t, d.SpliceCommand)
	assert.Equal(t, SCTE35SpliceCommandTypeTimeSignal, d.SpliceCommand.Type)
	require.NotNil(t, d.SpliceCommand.TimeSignal)
	assert.True(t, d.SpliceCommand.TimeSignal.TimeSpecified)
	assert.Equal(t, uint64(0x100000002), d.SpliceCommand.TimeSignal.PTS)

	// Adjusted PTS wraps modulo 2^33.
	assert.Equal(t, uint64(0x00000003), d.AdjustedPTS(d.SpliceCommand.TimeSignal.PTS))
}

func TestParseSCTE35_SAPTypeRoundTrip(t *testing.T) {
	for _, sap := range []uint8{0, 1, 2, 3} {
		b := newRefSCTE35Builder()
		bw := astikit.NewBitsWriterBatch(b.w)
		b.writeHeaderWithSAP(sap)
		bw.Write(uint8(0))       // protocol
		bw.Write(false)          // encrypted
		bw.WriteN(uint8(0), 6)   // algorithm
		bw.WriteN(uint64(0), 33) // pts_adjustment
		bw.Write(uint8(0))       // cw_index
		bw.WriteN(uint16(0), 12) // tier
		bw.WriteN(uint16(0), 12) // splice_command_length (0 for splice_null)
		bw.Write(uint8(0x00))    // splice_null command type
		bw.Write(uint16(0))      // empty descriptor loop
		require.NoError(t, bw.Err())
		raw := b.finish()

		got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
		require.NoError(t, err)
		assert.Equalf(t, sap, got.SAPType, "sap_type %d", sap)

		// The production writer must reproduce the same header bits.
		out, err := WriteSCTE35SpliceInfoSection(got)
		require.NoError(t, err)
		assert.Equalf(t, raw, out, "sap_type %d round-trip", sap)
	}
}

// buildSpliceInsertSection builds a rich splice_insert section using the
// production writer; the bytes are then used to exercise the parser.
func buildSpliceInsertSection(t *testing.T) *SCTE35SpliceInfoSection {
	return &SCTE35SpliceInfoSection{
		TableID:       0xfc,
		Tier:          0x456,
		PTSAdjustment: 0x180000000, // value above 2^32, below 2^33
		CWIndex:       0x42,
		SpliceCommand: &SCTE35SpliceCommand{
			Type: SCTE35SpliceCommandTypeSpliceInsert,
			SpliceInsert: &SCTE35SpliceInsert{
				SpliceEventID:              0x11223344,
				SpliceEventCancelIndicator: false,
				OutOfNetworkIndicator:      true,
				ProgramSpliceFlag:          true,
				DurationFlag:               true,
				SpliceImmediateFlag:        false,
				SpliceTime: &SCTE35SpliceTime{
					TimeSpecified: true,
					PTS:           0x180000123,
				},
				BreakDuration: &SCTE35BreakDuration{
					AutoReturn: true,
					Duration:   90000 * 30, // 30 seconds at 90 kHz
				},
				UniqueProgramID: 1,
				AvailNum:        2,
				AvailsExpected:  3,
			},
		},
		Descriptors: []*SCTE35SpliceDescriptor{
			{
				Tag: SCTE35DescriptorTagSegmentation,
				Segmentation: &SCTE35SegmentationDescriptor{
					SegmentationEventID:              0xaabbccdd,
					SegmentationEventCancelIndicator: false,
					ProgramSegmentationFlag:          true,
					SegmentationDurationFlag:         true,
					DeliveryNotRestrictedFlag:        true,
					SegmentationDuration:             90000 * 15,
					SegmentationUPIDType:             0x0c,
					SegmentationUPIDLength:           4,
					SegmentationUPID:                 []byte{0xde, 0xad, 0xbe, 0xef},
					SegmentationTypeID:               SCTE35SegmentationTypeProviderAdvertisementStart,
					SegmentNum:                       1,
					SegmentsExpected:                 2,
				},
			},
		},
	}
}

func TestParseSCTE35SpliceInsert_FullFields(t *testing.T) {
	d := buildSpliceInsertSection(t)
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)

	assert.Equal(t, uint16(0x456), got.Tier)
	assert.Equal(t, uint64(0x180000000), got.PTSAdjustment)
	assert.Equal(t, uint8(0x42), got.CWIndex)

	require.NotNil(t, got.SpliceCommand)
	si := got.SpliceCommand.SpliceInsert
	require.NotNil(t, si)
	assert.Equal(t, uint32(0x11223344), si.SpliceEventID)
	assert.False(t, si.SpliceEventCancelIndicator)
	assert.True(t, si.OutOfNetworkIndicator)
	assert.True(t, si.ProgramSpliceFlag)
	assert.True(t, si.DurationFlag)
	assert.False(t, si.SpliceImmediateFlag)
	require.NotNil(t, si.SpliceTime)
	assert.True(t, si.SpliceTime.TimeSpecified)
	assert.Equal(t, uint64(0x180000123), si.SpliceTime.PTS)
	require.NotNil(t, si.BreakDuration)
	assert.True(t, si.BreakDuration.AutoReturn)
	assert.Equal(t, uint64(90000*30), si.BreakDuration.Duration)
	assert.Equal(t, uint16(1), si.UniqueProgramID)
	assert.Equal(t, uint8(2), si.AvailNum)
	assert.Equal(t, uint8(3), si.AvailsExpected)

	require.Len(t, got.Descriptors, 1)
	desc := got.Descriptors[0]
	assert.Equal(t, SCTE35DescriptorTagSegmentation, desc.Tag)
	require.NotNil(t, desc.Segmentation)
	sd := desc.Segmentation
	assert.Equal(t, scte35CUEIIdentifier, sd.Identifier)
	assert.Equal(t, uint32(0xaabbccdd), sd.SegmentationEventID)
	assert.True(t, sd.ProgramSegmentationFlag)
	assert.True(t, sd.SegmentationDurationFlag)
	assert.True(t, sd.DeliveryNotRestrictedFlag)
	assert.Equal(t, uint64(90000*15), sd.SegmentationDuration)
	assert.Equal(t, uint8(0x0c), sd.SegmentationUPIDType)
	assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, sd.SegmentationUPID)
	assert.Equal(t, SCTE35SegmentationTypeProviderAdvertisementStart, sd.SegmentationTypeID)
	assert.Equal(t, uint8(1), sd.SegmentNum)
	assert.Equal(t, uint8(2), sd.SegmentsExpected)
	// Type 0x30 carries no sub-segment fields.
	assert.Equal(t, uint8(0), sd.SubSegmentNum)
	assert.Equal(t, uint8(0), sd.SubSegmentsExpected)
}

func TestParseSCTE35SpliceInsert_ComponentSplice(t *testing.T) {
	d := &SCTE35SpliceInfoSection{
		Tier: 0x100,
		SpliceCommand: &SCTE35SpliceCommand{
			Type: SCTE35SpliceCommandTypeSpliceInsert,
			SpliceInsert: &SCTE35SpliceInsert{
				SpliceEventID:         7,
				ProgramSpliceFlag:     false,
				SpliceImmediateFlag:   false,
				OutOfNetworkIndicator: false,
				Components: []*SCTE35SpliceComponent{
					{ComponentTag: 0x10, SpliceTime: &SCTE35SpliceTime{TimeSpecified: true, PTS: 0x11111111}},
					{ComponentTag: 0x20, SpliceTime: &SCTE35SpliceTime{TimeSpecified: false}},
				},
				UniqueProgramID: 1,
			},
		},
	}
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	si := got.SpliceCommand.SpliceInsert
	require.NotNil(t, si)
	assert.False(t, si.ProgramSpliceFlag)
	require.Len(t, si.Components, 2)
	assert.Equal(t, uint8(0x10), si.Components[0].ComponentTag)
	require.NotNil(t, si.Components[0].SpliceTime)
	assert.Equal(t, uint64(0x11111111), si.Components[0].SpliceTime.PTS)
	assert.Equal(t, uint8(0x20), si.Components[1].ComponentTag)
	require.NotNil(t, si.Components[1].SpliceTime)
	assert.False(t, si.Components[1].SpliceTime.TimeSpecified)
}

func TestParseSCTE35SpliceInsert_Cancellation(t *testing.T) {
	d := &SCTE35SpliceInfoSection{
		SpliceCommand: &SCTE35SpliceCommand{
			Type: SCTE35SpliceCommandTypeSpliceInsert,
			SpliceInsert: &SCTE35SpliceInsert{
				SpliceEventID:              0xdeadbeef,
				SpliceEventCancelIndicator: true,
			},
		},
	}
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	si := got.SpliceCommand.SpliceInsert
	require.NotNil(t, si)
	assert.True(t, si.SpliceEventCancelIndicator)
	assert.Equal(t, uint32(0xdeadbeef), si.SpliceEventID)
	assert.Nil(t, si.SpliceTime)
	assert.Nil(t, si.BreakDuration)
}

func TestParseSCTE35SpliceInsert_Immediate(t *testing.T) {
	d := &SCTE35SpliceInfoSection{
		SpliceCommand: &SCTE35SpliceCommand{
			Type: SCTE35SpliceCommandTypeSpliceInsert,
			SpliceInsert: &SCTE35SpliceInsert{
				SpliceEventID:       9,
				ProgramSpliceFlag:   true,
				SpliceImmediateFlag: true,
				UniqueProgramID:     3,
			},
		},
	}
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	si := got.SpliceCommand.SpliceInsert
	require.NotNil(t, si)
	assert.True(t, si.SpliceImmediateFlag)
	assert.Nil(t, si.SpliceTime)
}

func TestParseSCTE35SpliceSchedule(t *testing.T) {
	d := &SCTE35SpliceInfoSection{
		SpliceCommand: &SCTE35SpliceCommand{
			Type: SCTE35SpliceCommandTypeSpliceSchedule,
			SpliceSchedule: &SCTE35SpliceSchedule{
				Events: []*SCTE35SpliceScheduleEvent{
					{
						SpliceEventID:         1,
						ProgramSpliceFlag:     true,
						OutOfNetworkIndicator: true,
						DurationFlag:          true,
						UTCSpliceTime:         0x60000000,
						BreakDuration:         &SCTE35BreakDuration{AutoReturn: false, Duration: 12345},
						UniqueProgramID:       1,
						AvailNum:              1,
						AvailsExpected:        1,
					},
					{
						SpliceEventID:              2,
						SpliceEventCancelIndicator: true,
					},
				},
			},
		},
	}
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	require.NotNil(t, got.SpliceCommand.SpliceSchedule)
	require.Len(t, got.SpliceCommand.SpliceSchedule.Events, 2)

	e0 := got.SpliceCommand.SpliceSchedule.Events[0]
	assert.Equal(t, uint32(1), e0.SpliceEventID)
	assert.True(t, e0.ProgramSpliceFlag)
	assert.Equal(t, uint32(0x60000000), e0.UTCSpliceTime)
	require.NotNil(t, e0.BreakDuration)
	assert.False(t, e0.BreakDuration.AutoReturn)
	assert.Equal(t, uint64(12345), e0.BreakDuration.Duration)

	e1 := got.SpliceCommand.SpliceSchedule.Events[1]
	assert.True(t, e1.SpliceEventCancelIndicator)
}

func TestParseSCTE35SegmentationDescriptor_SubSegmentsAndComponents(t *testing.T) {
	d := &SCTE35SpliceInfoSection{
		SpliceCommand: &SCTE35SpliceCommand{Type: SCTE35SpliceCommandTypeTimeSignal, TimeSignal: &SCTE35SpliceTime{}},
		Descriptors: []*SCTE35SpliceDescriptor{
			{
				Tag: SCTE35DescriptorTagSegmentation,
				Segmentation: &SCTE35SegmentationDescriptor{
					SegmentationEventID:       5,
					ProgramSegmentationFlag:   false,
					DeliveryNotRestrictedFlag: false,
					WebDeliveryAllowedFlag:    true,
					NoRegionalBlackoutFlag:    true,
					ArchiveAllowedFlag:        false,
					DeviceRestrictions:        0x03,
					Components: []*SCTE35SegmentationComponent{
						{ComponentTag: 0x01, SegmentationDurationOffset: 0x00000005},
						{ComponentTag: 0x02, SegmentationDurationOffset: 0x1fffffff},
					},
					SegmentationDurationFlag: false,
					SegmentationUPIDType:     0x01,
					SegmentationUPIDLength:   1,
					SegmentationUPID:         []byte{0x7f},
					SegmentationTypeID:       SCTE35SegmentationTypeNetworkAdvertisementStart, // 0x38 -> sub-segments present
					SegmentNum:               1,
					SegmentsExpected:         4,
					SubSegmentNum:            2,
					SubSegmentsExpected:      8,
				},
			},
		},
	}
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	require.Len(t, got.Descriptors, 1)
	sd := got.Descriptors[0].Segmentation
	require.NotNil(t, sd)
	assert.False(t, sd.ProgramSegmentationFlag)
	assert.False(t, sd.DeliveryNotRestrictedFlag)
	assert.True(t, sd.WebDeliveryAllowedFlag)
	assert.True(t, sd.NoRegionalBlackoutFlag)
	assert.False(t, sd.ArchiveAllowedFlag)
	assert.Equal(t, uint8(0x03), sd.DeviceRestrictions)
	require.Len(t, sd.Components, 2)
	assert.Equal(t, uint8(0x02), sd.Components[1].ComponentTag)
	assert.Equal(t, uint64(0x1fffffff), sd.Components[1].SegmentationDurationOffset)
	assert.Equal(t, SCTE35SegmentationTypeNetworkAdvertisementStart, sd.SegmentationTypeID)
	assert.Equal(t, uint8(2), sd.SubSegmentNum)
	assert.Equal(t, uint8(8), sd.SubSegmentsExpected)
}

func TestParseSCTE35_UnknownCommandAndDescriptorLossless(t *testing.T) {
	// Build a section with an unknown command type 0x40 and an unknown
	// descriptor tag 0x55 with arbitrary payload.
	b := newRefSCTE35Builder()
	bw := astikit.NewBitsWriterBatch(b.w)

	b.writeHeader()

	bw.Write(uint8(0))       // protocol
	bw.Write(false)          // encrypted
	bw.WriteN(uint8(0), 6)   // algorithm
	bw.WriteN(uint64(0), 33) // pts_adjustment
	bw.Write(uint8(0))       // cw_index
	bw.WriteN(uint16(0), 12) // tier
	bw.WriteN(uint16(3), 12) // splice_command_length
	bw.Write(uint8(0x40))    // unknown command type
	bw.Write([]byte{0xaa, 0xbb, 0xcc})

	// Descriptor loop: one unknown descriptor (tag 0x55, 3 bytes payload).
	bw.Write(uint16(5))
	bw.Write(uint8(0x55))
	bw.Write(uint8(3))
	bw.Write([]byte{0x01, 0x02, 0x03})
	require.NoError(t, bw.Err())

	raw := b.finish()

	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	require.NotNil(t, got.SpliceCommand)
	assert.Equal(t, SCTE35SpliceCommandType(0x40), got.SpliceCommand.Type)
	assert.Equal(t, []byte{0xaa, 0xbb, 0xcc}, got.SpliceCommand.Raw)
	require.Len(t, got.Descriptors, 1)
	assert.Equal(t, uint8(0x55), got.Descriptors[0].Tag)
	assert.Equal(t, uint8(3), got.Descriptors[0].Length)
	assert.Equal(t, []byte{0x01, 0x02, 0x03}, got.Descriptors[0].Raw)
	assert.Nil(t, got.Descriptors[0].Segmentation)

	// Writing the parsed section back must reproduce the original bytes.
	roundTripped, err := WriteSCTE35SpliceInfoSection(got)
	require.NoError(t, err)
	assert.Equal(t, raw, roundTripped)
}

func TestParseSCTE35_PrivateCommand(t *testing.T) {
	d := &SCTE35SpliceInfoSection{
		SpliceCommand: &SCTE35SpliceCommand{
			Type: SCTE35SpliceCommandTypePrivateCommand,
			Private: &SCTE35PrivateCommand{
				Identifier: 0x41424344,
				Raw:        []byte{1, 2, 3, 4, 5},
			},
		},
	}
	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)
	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	require.NotNil(t, got.SpliceCommand.Private)
	assert.Equal(t, uint32(0x41424344), got.SpliceCommand.Private.Identifier)
	assert.Equal(t, []byte{1, 2, 3, 4, 5}, got.SpliceCommand.Private.Raw)
}

func TestParseSCTE35_SpliceNullAndBandwidthReservation(t *testing.T) {
	for _, typ := range []SCTE35SpliceCommandType{
		SCTE35SpliceCommandTypeSpliceNull,
		SCTE35SpliceCommandTypeBandwidthReservation,
	} {
		d := &SCTE35SpliceInfoSection{
			SpliceCommand: &SCTE35SpliceCommand{Type: typ},
		}
		raw, err := WriteSCTE35SpliceInfoSection(d)
		require.NoError(t, err)
		got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
		require.NoError(t, err)
		assert.Equal(t, typ, got.SpliceCommand.Type)
		assert.Zero(t, got.SpliceCommandLength)
	}
}

func TestSCTE35SpliceCommandType_String(t *testing.T) {
	assert.Equal(t, "splice_insert", SCTE35SpliceCommandTypeSpliceInsert.String())
	assert.Equal(t, "time_signal", SCTE35SpliceCommandTypeTimeSignal.String())
	assert.Equal(t, "splice_null", SCTE35SpliceCommandTypeSpliceNull.String())
	assert.Equal(t, "unknown(0x40)", SCTE35SpliceCommandType(0x40).String())
}

func TestSCTE35_PTSWrapping(t *testing.T) {
	assert.Equal(t, uint64(0), SCTE35NormalizePTS(scte35PTSModulo))
	assert.Equal(t, uint64(5), SCTE35NormalizePTS(scte35PTSModulo+5))

	// Adjustment that crosses the 2^33 boundary wraps per protocol.
	adj := uint64(scte35PTSModulo - 10)
	pts := uint64(15)
	assert.Equal(t, uint64(5), SCTE35AdjustedPTS(adj, pts))
}

func TestParseSCTE35_PointerBytePrefixedPayload(t *testing.T) {
	raw := refTimeSignalSection(t)
	prefixed := append([]byte{0x00}, raw...)

	pes := &PESData{Data: prefixed}
	got, err := parseSCTE35FromPESData(pes)
	require.NoError(t, err)
	require.NotNil(t, got.SpliceCommand.TimeSignal)
	assert.Equal(t, uint64(0x100000002), got.SpliceCommand.TimeSignal.PTS)
}

func TestParseSCTE35_Errors(t *testing.T) {
	good := refTimeSignalSection(t)

	t.Run("truncated header", func(t *testing.T) {
		_, err := ParseSCTE35SpliceInfoSectionBytes(good[:2])
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("truncated body", func(t *testing.T) {
		// Keep the header but cut the body before the CRC.
		_, err := ParseSCTE35SpliceInfoSectionBytes(good[:len(good)-5])
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("empty input", func(t *testing.T) {
		_, err := ParseSCTE35SpliceInfoSectionBytes(nil)
		require.Error(t, err)
	})

	t.Run("wrong table id", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] = 0xfb
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
		assert.True(t, strings.Contains(err.Error(), "table_id"))
	})

	t.Run("bad crc", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[len(bad)-1] ^= 0xff
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrCRC32Mismatch))
	})

	t.Run("section syntax indicator set", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[1] |= 0x80
		// section_length unchanged, CRC will also mismatch; the header check fires first.
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("private indicator set", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[1] |= 0x40
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("unsupported protocol version", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[3] = 1
		bad = recomputeSCTE35CRC(t, bad)
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("encrypted section rejected", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[3] |= 0x80 // encrypted_packet
		bad = recomputeSCTE35CRC(t, bad)
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("declared length exceeds input", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[2] += 10
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("section length below minimum", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[1] = (bad[1] & 0xf0)
		bad[2] = 4
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("splice time reserved bits", func(t *testing.T) {
		// Corrupt the reserved 6 bits inside splice_time, then fix the CRC.
		bad := append([]byte(nil), good...)
		// splice_time starts after table(3)+protocol/pts/cw(7)+tier/cmdlen(3)+cmdtype(1)=14.
		// byte 14 is time_specified + reserved6 + first PTS bit.
		bad[14] &^= 0x7e // clear the reserved bits
		bad = recomputeSCTE35CRC(t, bad)
		_, err := ParseSCTE35SpliceInfoSectionBytes(bad)
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("pes payload without table id", func(t *testing.T) {
		_, err := parseSCTE35FromPESData(&PESData{Data: []byte{1, 2, 3}})
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("empty pes payload", func(t *testing.T) {
		_, err := parseSCTE35FromPESData(&PESData{})
		require.Error(t, err)
	})
}

// recomputeSCTE35CRC rewrites the trailing CRC32 of a section whose body was
// deliberately mutated, so structural validation rather than CRC mismatch is
// exercised.
func recomputeSCTE35CRC(t *testing.T, raw []byte) []byte {
	t.Helper()
	require.GreaterOrEqual(t, len(raw), 7)
	crc := computeCRC32(raw[:len(raw)-4])
	raw[len(raw)-4] = byte(crc >> 24)
	raw[len(raw)-3] = byte(crc >> 16)
	raw[len(raw)-2] = byte(crc >> 8)
	raw[len(raw)-1] = byte(crc)
	return raw
}

func TestWriteSCTE35_Validation(t *testing.T) {
	t.Run("missing command", func(t *testing.T) {
		_, err := WriteSCTE35SpliceInfoSection(&SCTE35SpliceInfoSection{})
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("pts adjustment too wide", func(t *testing.T) {
		_, err := WriteSCTE35SpliceInfoSection(&SCTE35SpliceInfoSection{
			PTSAdjustment: scte35PTSModulo,
			SpliceCommand: &SCTE35SpliceCommand{Type: SCTE35SpliceCommandTypeSpliceNull},
		})
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("tier too wide", func(t *testing.T) {
		_, err := WriteSCTE35SpliceInfoSection(&SCTE35SpliceInfoSection{
			Tier:          0x1000,
			SpliceCommand: &SCTE35SpliceCommand{Type: SCTE35SpliceCommandTypeSpliceNull},
		})
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("bad table id", func(t *testing.T) {
		_, err := WriteSCTE35SpliceInfoSection(&SCTE35SpliceInfoSection{
			TableID:       0x01,
			SpliceCommand: &SCTE35SpliceCommand{Type: SCTE35SpliceCommandTypeSpliceNull},
		})
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})

	t.Run("upid length mismatch", func(t *testing.T) {
		_, err := WriteSCTE35SpliceInfoSection(&SCTE35SpliceInfoSection{
			SpliceCommand: &SCTE35SpliceCommand{
				Type:       SCTE35SpliceCommandTypeTimeSignal,
				TimeSignal: &SCTE35SpliceTime{},
			},
			Descriptors: []*SCTE35SpliceDescriptor{{
				Tag: SCTE35DescriptorTagSegmentation,
				Segmentation: &SCTE35SegmentationDescriptor{
					SegmentationUPIDLength: 2,
					SegmentationUPID:       []byte{1},
				},
			}},
		})
		require.True(t, errors.Is(err, ErrSCTE35InvalidSection))
	})
}

func TestWriteSCTE35_LengthFieldsAreRecomputed(t *testing.T) {
	d := buildSpliceInsertSection(t)
	// Stale wire values must not survive serialization.
	d.SectionLength = 0xffff
	d.SpliceCommandLength = 0xffff
	d.DescriptorLoopLength = 0xffff

	raw, err := WriteSCTE35SpliceInfoSection(d)
	require.NoError(t, err)
	got, err := ParseSCTE35SpliceInfoSectionBytes(raw)
	require.NoError(t, err)
	assert.NotEqual(t, uint16(0xffff), got.SectionLength)
	assert.NotEqual(t, uint16(0xffff), got.SpliceCommandLength)
	assert.NotEqual(t, uint16(0xffff), got.DescriptorLoopLength)
}

func FuzzParseSCTE35(f *testing.F) {
	f.Add(refTimeSignalSection(f))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := ParseSCTE35SpliceInfoSectionBytes(b)
		if err != nil {
			return
		}
		// Anything successfully parsed must round-trip and re-parse.
		raw, err := WriteSCTE35SpliceInfoSection(d)
		if err != nil {
			t.Fatalf("write failed after successful parse: %v", err)
		}
		if _, err := ParseSCTE35SpliceInfoSectionBytes(raw); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
	})
}
