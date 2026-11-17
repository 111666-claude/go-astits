// Command astits-subtitle-dump extracts raw DVB bitmap subtitle segments
// (EN 300 743) from an MPEG transport stream. Subtitling streams are selected
// from the PMT subtitling descriptors by language and composition page ID.
// For every segment it prints the PTS, PID, language, page, segment type and
// the raw segment payload as hexadecimal. Bitmaps are neither decoded nor
// rendered.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/asticode/go-astits"
)

func main() {
	inputPath := flag.String("i", "", "the input TS file path (required)")
	language := flag.String("lang", "", "ISO 639-2/B language code to select (e.g. eng); empty means any language")
	page := flag.Int("page", -1, "composition page ID to select (decimal); -1 means any page")
	flag.Parse()

	if *inputPath == "" {
		log.Fatal("use -i to indicate an input path")
	}

	f, err := os.Open(*inputPath)
	if err != nil {
		log.Fatalf("astits: opening %s failed: %v", *inputPath, err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handleSignals(cancel)

	var opts []astits.DVBSubtitleExtractorOpt
	if *language != "" {
		opts = append(opts, astits.DVBSubtitleOptLanguage([]byte(*language)))
	}
	if *page >= 0 {
		opts = append(opts, astits.DVBSubtitleOptCompositionPage(uint16(*page)))
	}

	dmx := astits.NewDemuxer(ctx, f, astits.DemuxerOptPacketSize(astits.MpegTsPacketSize),
		astits.DemuxerOptDiscontinuityHandler(func(pid uint16) {
			fmt.Fprintf(os.Stderr, "continuity break on PID %#x\n", pid)
		}))
	extractor := astits.NewDVBSubtitleExtractor(dmx, opts...)

	for {
		ev, err := extractor.NextEvent()
		if err != nil {
			if errors.Is(err, astits.ErrNoMorePackets) {
				return
			}
			log.Fatalf("astits: extracting subtitle segment failed: %v", err)
		}

		switch ev.Type {
		case astits.DVBSubtitleEventTypeSegment:
			printSegment(ev.Segment)
		case astits.DVBSubtitleEventTypePIDChange:
			c := ev.PIDChange
			fmt.Fprintf(os.Stderr, "subtitle PID changed: %#x -> %#x (lang=%s page=%#x program=%d)\n",
				c.OldPID, c.NewPID, string(c.Language), c.CompositionPageID, c.ProgramNumber)
		case astits.DVBSubtitleEventTypeDiscontinuity:
			fmt.Fprintf(os.Stderr, "continuity break on subtitle PID %#x\n", ev.Discontinuity.PID)
		}
	}
}

func printSegment(s *astits.DVBSubtitleSegment) {
	var sb strings.Builder
	enc := hex.NewEncoder(&sb)
	enc.Write(s.Data)
	fmt.Printf("pts=%10d pid=%#06x lang=%s page=%#06x type=%-24s n=%4d bytes=%s\n",
		s.PTS.Base, s.PID, string(s.Language), s.CompositionPageID,
		s.Type.String(), len(s.Data), sb.String())
}

func handleSignals(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch)
	go func() {
		for sig := range ch {
			if sig != syscall.SIGURG {
				log.Printf("received signal %v", sig)
			}
			switch sig {
			case syscall.SIGABRT, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM:
				cancel()
				return
			}
		}
	}()
}
