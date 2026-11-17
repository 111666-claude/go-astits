[![GoReportCard](http://goreportcard.com/badge/github.com/asticode/go-astits)](http://goreportcard.com/report/github.com/asticode/go-astits)
[![GoDoc](https://godoc.org/github.com/asticode/go-astits?status.svg)](https://godoc.org/github.com/asticode/go-astits)
[![Test](https://github.com/asticode/go-astits/actions/workflows/test.yml/badge.svg)](https://github.com/asticode/go-astits/actions/workflows/test.yml)
[![Coveralls](https://coveralls.io/repos/github/asticode/go-astits/badge.svg?branch=master)](https://coveralls.io/github/asticode/go-astits)

This is a Golang library to natively demux and mux MPEG Transport Streams (ts) in GO.

WARNING: this library is not yet production ready. Use at your own risks!

# Installation

To install the library use the following:

    go get -u github.com/asticode/go-astits/...

To install the executables use the following:

    go install github.com/asticode/go-astits/cmd
    
# Before looking at the code...

The transport stream is made of packets.<br>
Each packet has a header, an optional adaptation field and a payload.<br>
Several payloads can be appended and parsed as a data.

```
                                           TRANSPORT STREAM
 +--------------------------------------------------------------------------------------------------+
 |                                                                                                  |
 
                       PACKET                                         PACKET
 +----------------------------------------------+----------------------------------------------+----
 |                                              |                                              |
 
 +--------+---------------------------+---------+--------+---------------------------+---------+
 | HEADER | OPTIONAL ADAPTATION FIELD | PAYLOAD | HEADER | OPTIONAL ADAPTATION FIELD | PAYLOAD | ...
 +--------+---------------------------+---------+--------+---------------------------+---------+
 
                                      |         |                                    |         |
                                      +---------+                                    +---------+
                                           |                                              |
                                           +----------------------------------------------+
                                                                DATA
```
    
# Using the library in your code

WARNING: the code below doesn't handle errors for readability purposes. However you SHOULD!

## Demux

```go
// Create a cancellable context in case you want to stop reading packets/data any time you want
ctx, cancel := context.WithCancel(context.Background())

// Handle SIGTERM signal
ch := make(chan os.Signal, 1)
signal.Notify(ch, syscall.SIGTERM)
go func() {
    <-ch
    cancel()
}()

// Open your file or initialize any kind of io.Reader
// Buffering using bufio.Reader is recommended for performance
f, _ := os.Open("/path/to/file.ts")
defer f.Close()

// Create the demuxer
dmx := astits.NewDemuxer(ctx, f)
for {
    // Get the next data
    d, _ := dmx.NextData()
    
    // Data is a PMT data
    if d.PMT != nil {
        // Loop through elementary streams
        for _, es := range d.PMT.ElementaryStreams {
                fmt.Printf("Stream detected: %d\n", es.ElementaryPID)
        }
        return
    }
}
```

## Mux

```go
// Create a cancellable context in case you want to stop writing packets/data any time you want
ctx, cancel := context.WithCancel(context.Background())

// Handle SIGTERM signal
ch := make(chan os.Signal, 1)
signal.Notify(ch, syscall.SIGTERM)
go func() {
    <-ch
    cancel()
}()

// Create your file or initialize any kind of io.Writer
// Buffering using bufio.Writer is recommended for performance
f, _ := os.Create("/path/to/file.ts")
defer f.Close()

// Create the muxer
mx := astits.NewMuxer(ctx, f)

// Add an elementary stream
mx.AddElementaryStream(astits.PMTElementaryStream{
    ElementaryPID: 1,
    StreamType:    astits.StreamTypeMetadata,
})

// Write tables
// Using that function is not mandatory, WriteData will retransmit tables from time to time 
mx.WriteTables()

// Write data
mx.WriteData(&astits.MuxerData{
    PES: &astits.PESData{
        Data: []byte("test"),
    },
    PID: 1,
})
```

## Options

In order to pass options to the demuxer or the muxer, look for the methods prefixed with `DemuxerOpt` or `MuxerOpt` and add them upon calling `NewDemuxer` or `NewMuxer` :

```go
// This is your custom packets parser
p := func(ps []*astits.Packet) (ds []*astits.Data, skip bool, err error) {
        // This is your logic
        skip = true
        return
}

// Now you can create a demuxer with the proper options
dmx := NewDemuxer(ctx, f, DemuxerOptPacketSize(192), DemuxerOptPacketsParser(p))
```

# CLI

This library provides 2 CLIs that will automatically get installed in `GOPATH/bin` on `go get` execution.

## astits-probe

### List streams

    $ astits-probe -i <path to your file> -f <format: text|json (default: text)>

### List packets

    $ astits-probe packets -i <path to your file>

### List data

    $ astits-probe data -i <path to your file> -d <data type: eit|nit|... (repeatable argument | if empty, all data types are shown)>

### Extract DVB teletext data units

The `teletext` command picks a teletext track from the PMT teletext descriptor
(tag 0x56) and emits the raw EBU teletext data units of the matching
language/page as JSON lines, each tagged with the PTS of its PES packet.
Missing descriptors, bad data unit lengths, missing PTS, continuity breaks and
program changes are reported as `{"kind":"error",...}` lines instead of being
silently dropped.

    $ astits-probe teletext -i <path to your file> \
        [-teletext-language <ISO 639-2/B code, e.g. eng>] \
        [-teletext-magazine <1-8>] [-teletext-page <0-99>] \
        [-teletext-program <program number>]

The same extraction is available in code through `NewTeletextExtractor`,
`FindTeletextTracks` and the `TeletextSelector`/`TeletextEvent` types.

## astits-es-split

### Split streams into separate .ts files

    $ astits-es-split <path to your file> -o <path to output dir>

# Features and roadmap

- [x] Add demuxer
- [x] Add muxer
- [x] Demux PES packets
- [x] Mux PES packets
- [x] Demux PAT packets
- [x] Mux PAT packets
- [x] Demux PMT packets
- [x] Mux PMT packets
- [x] Demux EIT packets
- [ ] Mux EIT packets
- [x] Demux NIT packets
- [ ] Mux NIT packets
- [x] Demux SDT packets
- [ ] Mux SDT packets
- [x] Demux TOT packets
- [ ] Mux TOT packets
- [x] Demux DVB teletext data units (EN 300 472 / EN 300 706)
- [ ] Demux BAT packets
- [ ] Mux BAT packets
- [ ] Demux DIT packets
- [ ] Mux DIT packets
- [ ] Demux RST packets
- [ ] Mux RST packets
- [ ] Demux SIT packets
- [ ] Mux SIT packets
- [ ] Mux ST packets
- [ ] Demux TDT packets
- [ ] Mux TDT packets
- [ ] Demux TSDT packets
- [ ] Mux TSDT packets
