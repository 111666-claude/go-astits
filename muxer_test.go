package astits

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func patExpectedBytes(versionNumber uint8, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(PIDPAT, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint16(0))       // Table ID
	w.Write("1011")          // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(13), 12) // Section length

	w.Write(uint16(PSITableIDPAT))
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write(programNumberStart)
	w.Write("111") // reserved
	w.WriteN(pmtStartPID, 13)

	// CRC32
	if versionNumber == 0 {
		w.Write([]byte{0x71, 0x10, 0xd8, 0x78})
	} else {
		w.Write([]byte{0xef, 0xbe, 0x08, 0x5a})
	}

	w.Write(bytes.Repeat([]byte{0xff}, 167))

	return buf.Bytes()
}

func TestMuxer_generatePAT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)

	err := muxer.generatePAT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.patBytes.Len())
	assert.Equal(t, patExpectedBytes(0, 0), muxer.patBytes.Bytes())

	// Version number shouldn't change
	err = muxer.generatePAT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.patBytes.Len())
	assert.Equal(t, patExpectedBytes(0, 1), muxer.patBytes.Bytes())

	// Version number should change
	muxer.pmUpdated = true
	err = muxer.generatePAT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.patBytes.Len())
	assert.Equal(t, patExpectedBytes(1, 2), muxer.patBytes.Bytes())
}

func pmtExpectedBytesVideoOnly(versionNumber, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(pmtStartPID, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint16(PSITableIDPMT)) // Table ID
	w.Write("1011")                // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(18), 12)       // Section length

	w.Write(programNumberStart)
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write("111")               // reserved
	w.WriteN(uint16(0x1234), 13) // PCR PID

	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // program info length

	w.Write(uint8(StreamTypeH264Video))
	w.Write("111") // reserved
	w.WriteN(uint16(0x1234), 13)

	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // es info length

	w.Write([]byte{0x31, 0x48, 0x5b, 0xa2}) // CRC32

	w.Write(bytes.Repeat([]byte{0xff}, 162))

	return buf.Bytes()
}

func pmtExpectedBytesVideoAndAudio(versionNumber uint8, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(pmtStartPID, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint16(PSITableIDPMT)) // Table ID
	w.Write("1011")                // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(23), 12)       // Section length

	w.Write(programNumberStart)
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write("111")               // reserved
	w.WriteN(uint16(0x1234), 13) // PCR PID

	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // program info length

	w.Write(uint8(StreamTypeH264Video))
	w.Write("111") // reserved
	w.WriteN(uint16(0x1234), 13)
	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // es info length

	w.Write(uint8(StreamTypeADTS))
	w.Write("111") // reserved
	w.WriteN(uint16(0x0234), 13)
	w.Write("1111")         // reserved
	w.WriteN(uint16(0), 12) // es info length

	// CRC32
	if versionNumber == 0 {
		w.Write([]byte{0x29, 0x52, 0xc4, 0x50})
	} else {
		w.Write([]byte{0x06, 0xf4, 0xa6, 0xea})
	}

	w.Write(bytes.Repeat([]byte{0xff}, 157))

	return buf.Bytes()
}

func TestMuxer_generatePMT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	muxer.SetPCRPID(0x1234)
	assert.NoError(t, err)

	err = muxer.generatePMT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.pmtBytes.Len())
	assert.Equal(t, pmtExpectedBytesVideoOnly(0, 0), muxer.pmtBytes.Bytes())

	// Version number shouldn't change
	err = muxer.generatePMT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.pmtBytes.Len())
	assert.Equal(t, pmtExpectedBytesVideoOnly(0, 1), muxer.pmtBytes.Bytes())

	err = muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x0234,
		StreamType:    StreamTypeAACAudio,
	})
	assert.NoError(t, err)

	// Version number should change
	err = muxer.generatePMT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.pmtBytes.Len())
	assert.Equal(t, pmtExpectedBytesVideoAndAudio(1, 2), muxer.pmtBytes.Bytes())
}

func TestMuxer_WriteTables(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	muxer.SetPCRPID(0x1234)
	assert.NoError(t, err)

	n, err := muxer.WriteTables()
	assert.NoError(t, err)
	assert.Equal(t, 2*MpegTsPacketSize, n)
	assert.Equal(t, n, buf.Len())

	expectedBytes := append(patExpectedBytes(0, 0), pmtExpectedBytesVideoOnly(0, 0)...)
	assert.Equal(t, expectedBytes, buf.Bytes())
}

func TestMuxer_WriteTables_Error(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)

	_, err = muxer.WriteTables()
	assert.Equal(t, ErrPCRPIDInvalid, err)
}

func sdtExpectedBytes(versionNumber uint8, cc uint8) []byte {
	buf := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &buf})
	w.Write(uint8(syncByte))
	w.Write("010") // no transport error, payload start, no priority
	w.WriteN(PIDSDT, 13)
	w.Write("0001") // no scrambling, no AF, payload present
	w.WriteN(cc, 4)

	w.Write(uint8(0))                     // pointer field
	w.Write(uint8(PSITableIDSDTVariant1)) // Table ID
	w.Write("1011")                       // Syntax section indicator, private bit, reserved
	w.WriteN(uint16(47), 12)              // Section length

	w.Write(uint16(0))         // Transport stream ID
	w.Write("11")              // Reserved bits
	w.WriteN(versionNumber, 5) // Version number
	w.Write("1")               // Current/next indicator
	w.Write(uint8(0))          // Section number
	w.Write(uint8(0))          // Last section number

	w.Write(uint16(0))   // Original network ID
	w.Write(uint8(0xff)) // Reserved

	w.Write(uint16(programNumberStart)) // Service ID
	w.Write(uint8(0xfc))                // Reserved + EIT schedule/present-following flags
	w.Write("100")                      // Running status: running
	w.Write("0")                        // Free CA mode
	w.WriteN(uint16(30), 12)            // Descriptors loop length

	w.Write(uint8(DescriptorTagService))
	w.Write(uint8(28)) // Descriptor length
	w.Write(uint8(ServiceTypeDigitalTelevision))
	w.Write(uint8(13))
	w.Write([]byte("Test Provider"))
	w.Write(uint8(12))
	w.Write([]byte("Test Service"))

	// CRC32 (computed independently with CRC-32/MPEG-2)
	if versionNumber == 0 {
		w.Write([]byte{0x8a, 0x79, 0xe9, 0x19})
	} else {
		w.Write([]byte{0x0e, 0x1e, 0x44, 0x14})
	}

	w.Write(bytes.Repeat([]byte{0xff}, 133))

	return buf.Bytes()
}

func TestMuxer_generateSDT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.SetSDT(programNumberStart, "Test Service", "Test Provider")
	assert.NoError(t, err)

	err = muxer.generateSDT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.sdtBytes.Len())
	assert.Equal(t, sdtExpectedBytes(0, 0), muxer.sdtBytes.Bytes())

	// Version number shouldn't change
	err = muxer.generateSDT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.sdtBytes.Len())
	assert.Equal(t, sdtExpectedBytes(0, 1), muxer.sdtBytes.Bytes())

	// Version number should change
	err = muxer.SetSDT(programNumberStart, "Test Service", "Test Provider")
	assert.NoError(t, err)
	err = muxer.generateSDT()
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, muxer.sdtBytes.Len())
	assert.Equal(t, sdtExpectedBytes(1, 2), muxer.sdtBytes.Bytes())
}

func TestMuxer_SetSDT(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)

	// Service ID doesn't match any program
	err := muxer.SetSDT(programNumberStart+1, "name", "provider")
	assert.True(t, errors.Is(err, ErrSDTServiceNotFound), "expected %v, got %v", ErrSDTServiceNotFound, err)

	// Name too long
	err = muxer.SetSDT(programNumberStart, strings.Repeat("a", maxSDTStringLength+1), "provider")
	assert.True(t, errors.Is(err, ErrSDTNameTooLong), "expected %v, got %v", ErrSDTNameTooLong, err)

	// Provider too long
	err = muxer.SetSDT(programNumberStart, "name", strings.Repeat("a", maxSDTStringLength+1))
	assert.True(t, errors.Is(err, ErrSDTProviderTooLong), "expected %v, got %v", ErrSDTProviderTooLong, err)

	// Name and provider are individually valid but the section doesn't fit into a single TS packet
	err = muxer.SetSDT(programNumberStart, strings.Repeat("a", 100), strings.Repeat("a", 100))
	assert.True(t, errors.Is(err, ErrSDTSectionDoesNotFit), "expected %v, got %v", ErrSDTSectionDoesNotFit, err)

	// Failed attempts must not enable SDT
	assert.False(t, muxer.sdtEnabled)

	// Longest names that still fit
	err = muxer.SetSDT(programNumberStart, strings.Repeat("a", 100), strings.Repeat("a", 58))
	assert.NoError(t, err)
	assert.True(t, muxer.sdtEnabled)
}

func TestMuxer_WriteTables_SDT(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)
	muxer.SetPCRPID(0x1234)
	err = muxer.SetSDT(programNumberStart, "Test Service", "Test Provider")
	assert.NoError(t, err)

	n, err := muxer.WriteTables()
	assert.NoError(t, err)
	assert.Equal(t, 3*MpegTsPacketSize, n)
	assert.Equal(t, n, buf.Len())

	expectedBytes := append(patExpectedBytes(0, 0), pmtExpectedBytesVideoOnly(0, 0)...)
	expectedBytes = append(expectedBytes, sdtExpectedBytes(0, 0)...)
	assert.Equal(t, expectedBytes, buf.Bytes())
}

func TestMuxer_SDTRoundTrip(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf,
		MuxerOptTablesRetransmitPeriod(2),
		WithTransportStreamID(7),
		WithOriginalNetworkID(9),
	)
	require.NoError(t, muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	}))
	muxer.SetPCRPID(0x1234)
	require.NoError(t, muxer.SetSDT(programNumberStart, "Test Service", "Test Provider"))

	// Write enough PES packets to trigger table retransmissions
	payload := testPayload()
	for i := 0; i < 5; i++ {
		_, err := muxer.WriteData(&MuxerData{
			PID: 0x1234,
			PES: &PESData{
				Data:   payload,
				Header: &PESHeader{},
			},
		})
		require.NoError(t, err)
	}

	// Demux the output and collect SDTs (first transmission and retransmissions)
	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))
	var pat *PATData
	var pmt *PMTData
	var sdts []*SDTData
	pesCount := 0
	for {
		d, err := dmx.NextData()
		if errors.Is(err, ErrNoMorePackets) {
			break
		}
		require.NoError(t, err)

		if d.PAT != nil {
			pat = d.PAT
		}
		if d.PMT != nil {
			pmt = d.PMT
		}
		if d.SDT != nil {
			sdts = append(sdts, d.SDT)
		}
		if d.PES != nil {
			pesCount++
		}
	}

	// PAT and PMT are still there
	require.NotNil(t, pat)
	require.NotNil(t, pmt)
	assert.Equal(t, uint16(7), pat.TransportStreamID)
	assert.Equal(t, uint16(programNumberStart), pmt.ProgramNumber)

	// SDT was received at least twice (first transmission + retransmission)
	require.GreaterOrEqual(t, len(sdts), 2)
	for _, sdt := range sdts {
		assert.Equal(t, uint16(7), sdt.TransportStreamID)
		assert.Equal(t, uint16(9), sdt.OriginalNetworkID)
		require.Len(t, sdt.Services, 1)

		svc := sdt.Services[0]
		assert.Equal(t, uint16(programNumberStart), svc.ServiceID)
		assert.Equal(t, uint8(RunningStatusRunning), svc.RunningStatus)
		require.Len(t, svc.Descriptors, 1)

		d := svc.Descriptors[0]
		assert.Equal(t, uint8(DescriptorTagService), d.Tag)
		require.NotNil(t, d.Service)
		assert.Equal(t, ServiceTypeDigitalTelevision, d.Service.Type)
		assert.Equal(t, "Test Service", string(d.Service.Name))
		assert.Equal(t, "Test Provider", string(d.Service.Provider))
	}

	// PES packets still come through
	assert.Equal(t, 5, pesCount)
}

func TestMuxer_AddElementaryStream(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)

	err = muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.Equal(t, ErrPIDAlreadyExists, err)
}

func TestMuxer_RemoveElementaryStream(t *testing.T) {
	muxer := NewMuxer(context.Background(), nil)
	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	assert.NoError(t, err)

	err = muxer.RemoveElementaryStream(0x1234)
	assert.NoError(t, err)

	err = muxer.RemoveElementaryStream(0x1234)
	assert.Equal(t, ErrPIDNotFound, err)
}

func testPayload() []byte {
	ret := make([]byte, 0xff+1)
	for i := 0; i <= 0xff; i++ {
		ret[i] = byte(i)
	}
	return ret
}

func TestMuxer_WritePayload(t *testing.T) {
	buf := bytes.Buffer{}
	muxer := NewMuxer(context.Background(), &buf)

	err := muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x1234,
		StreamType:    StreamTypeH264Video,
	})
	muxer.SetPCRPID(0x1234)
	assert.NoError(t, err)

	err = muxer.AddElementaryStream(PMTElementaryStream{
		ElementaryPID: 0x0234,
		StreamType:    StreamTypeAACAudio,
	})
	assert.NoError(t, err)

	payload := testPayload()
	pcr := ClockReference{
		Base:      5726623061,
		Extension: 341,
	}
	pts := ClockReference{Base: 5726623060}

	n, err := muxer.WriteData(&MuxerData{
		PID: 0x1234,
		AdaptationField: &PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &pcr,
			RandomAccessIndicator: true,
		},
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					DTS:             &pts,
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorBothPresent,
				},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, buf.Len(), n)

	bytesTotal := n

	n, err = muxer.WriteData(&MuxerData{
		PID: 0x0234,
		AdaptationField: &PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &pcr,
			RandomAccessIndicator: true,
		},
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{
					DTS:             &pts,
					PTS:             &pts,
					PTSDTSIndicator: PTSDTSIndicatorBothPresent,
				},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, buf.Len(), bytesTotal+n)
	assert.Equal(t, 0, buf.Len()%MpegTsPacketSize)

	bs := buf.Bytes()
	assert.Equal(t, patExpectedBytes(0, 0), bs[:MpegTsPacketSize])
	assert.Equal(t, pmtExpectedBytesVideoAndAudio(0, 0), bs[MpegTsPacketSize:MpegTsPacketSize*2])
}
