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

## Teletext

DVB teletext subtitles are carried in private_stream_1 PES packets. Feed the
PES data emitted by the demuxer to a `TeletextDecoder` to accumulate pages and
receive one update per changed line:

```go
// Optionally cap the number of cached pages (default: 64, FIFO eviction)
ttx := astits.NewTeletextDecoder(astits.TeletextDecoderOptMaxPages(32))

for {
    d, err := dmx.NextData()
    if err != nil {
        // End of stream: report any data unit still waiting for completion
        if err := ttx.Flush(); err != nil {
            fmt.Println(err)
        }
        break
    }
    if d.PES == nil {
        continue
    }

    // Updates contain the page id and the new/changed row. Data units may span
    // several PES packets; incomplete ones are buffered automatically.
    updates, err := ttx.FeedPES(d.PES)
    if err != nil {
        // Hamming, truncation, invalid page and cache-eviction errors are
        // non-fatal: decoding continues and the full history is in ttx.Errors()
        fmt.Println(err)
    }
    for _, u := range updates {
        fmt.Printf("page %s row %d: %q\n", u.PageID, u.Line.Row, u.Line.Text)
    }
}

// Export a whole page as text, rows in ascending order
if page, ok := ttx.Page(astits.TeletextPageID{Magazine: 1, Page: 0}); ok {
    fmt.Println(page.Text())
}
```

Each line keeps its original 40 bytes in `Line.Raw`, so characters outside the
default G0 set (rendered as the unicode replacement character) can be reviewed
against the raw stream.

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
