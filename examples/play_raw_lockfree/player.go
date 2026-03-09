package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// RawAudioPlayer plays raw PCM audio using an AudioStreamer for output.
// A producer goroutine reads from the source and pushes data via
// streamer.Write(). The streamer owns the SPSC ring buffer, PortAudio
// stream, and consumer (callback or writer goroutine).
//
//	Producer goroutine              AudioStreamer
//	┌──────────────────┐     ┌────────────────────────────────────┐
//	│ source.Read       │────▶│ Write → SPSC → [callback/writer]  │
//	│ → streamer.Write  │     │                → audio hardware    │
//	└──────────────────┘     └────────────────────────────────────┘
//
// Format changes are handled transparently by the streamer: it drains
// buffered data in the old format, reconfigures PortAudio, and continues.
type RawAudioPlayer struct {
	streamer       AudioStreamer
	reader         io.ReadCloser
	format         FrameFormat
	mode           PlayerMode
	channels       int
	sampleRate     int
	bytesPerSample int
	framesPerBuf   int
	ringMs         int

	eof  atomic.Bool // file fully read by producer
	done atomic.Bool // eof + streamer buffer drained

	// Data integrity counters (producer side)
	bytesFromFile atomic.Uint64 // total bytes returned by source.Read
	bytesToRing   atomic.Uint64 // total bytes accepted by streamer.Write
}

// PlayerConfig holds the parameters for creating a RawAudioPlayer.
type PlayerConfig struct {
	Device       int
	Channels     int
	SampleFormat SampleFormat
	SampleRate   int
	FramesPerBuf int
	RingMs       int
	Mode         PlayerMode
}

// NewRawAudioPlayer creates a player that reads from the given source.
// Internally creates an AudioStreamer to handle buffering and playback.
func NewRawAudioPlayer(cfg PlayerConfig, source io.ReadCloser) (*RawAudioPlayer, error) {
	streamer, err := NewPortAudioStreamer(cfg.Device, cfg.Channels, cfg.SampleFormat, cfg.SampleRate, cfg.RingMs, cfg.Mode)
	if err != nil {
		return nil, err
	}

	bytesPerSample := cfg.SampleFormat.BytesPerSample()
	return &RawAudioPlayer{
		streamer: streamer,
		reader:   source,
		format: FrameFormat{
			SampleRate:   cfg.SampleRate,
			Channels:     cfg.Channels,
			SampleFormat: cfg.SampleFormat,
		},
		mode:           cfg.Mode,
		channels:       cfg.Channels,
		sampleRate:     cfg.SampleRate,
		bytesPerSample: bytesPerSample,
		framesPerBuf:   cfg.FramesPerBuf,
		ringMs:         cfg.RingMs,
	}, nil
}

// Start opens the audio stream and begins the producer goroutine.
func (p *RawAudioPlayer) Start(ctx context.Context) error {
	if err := p.streamer.Open(p.framesPerBuf); err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	if err := p.streamer.StartStream(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}
	go p.producer(ctx)
	return nil
}

// producer reads audio data from the source and pushes it to the streamer.
// Runs in its own goroutine — blocking I/O is safe here.
func (p *RawAudioPlayer) producer(ctx context.Context) {
	buf := make([]byte, 4096)
	frameSize := p.channels * p.bytesPerSample
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := p.reader.Read(buf)
		if n > 0 {
			p.bytesFromFile.Add(uint64(n))
			frames := n / frameSize
			if werr := p.streamer.Write(p.format, frames, buf[:n]); werr != nil {
				return
			}
			p.bytesToRing.Add(uint64(n))
		}
		if err != nil {
			if err == io.EOF {
				p.eof.Store(true)
			}
			return
		}
	}
}

// Stop stops the audio stream and releases PortAudio resources.
// Safe to call multiple times.
func (p *RawAudioPlayer) Stop() error {
	if p.streamer == nil {
		return nil
	}
	var errs []error
	if err := p.streamer.StopStream(); err != nil {
		errs = append(errs, fmt.Errorf("stop stream: %w", err))
	}
	if err := p.streamer.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close stream: %w", err))
	}
	p.streamer = nil
	return errors.Join(errs...)
}

// Close stops playback and closes the audio source.
func (p *RawAudioPlayer) Close() error {
	p.Stop()
	if p.reader != nil {
		return p.reader.Close()
	}
	return nil
}

// GetSamplesPlayed returns the total number of audio frames delivered to output.
func (p *RawAudioPlayer) GetSamplesPlayed() uint64 {
	if s, ok := p.streamer.(*paStreamer); ok {
		return s.SamplesPlayed()
	}
	return 0
}

// Done returns true when all audio data has been played.
func (p *RawAudioPlayer) Done() bool {
	if p.done.Load() {
		return true
	}
	if p.eof.Load() && p.streamer != nil && p.streamer.Buffered() == 0 {
		p.done.Store(true)
		return true
	}
	return false
}

// Diagnostics holds a snapshot of the player's diagnostic counters.
type Diagnostics struct {
	Mode           PlayerMode
	FramesPerBuf   int
	RingSize       int
	RingMs         float64
	SampleRate     int
	Channels       int
	BytesPerSample int

	// Data integrity (producer side)
	BytesFromFile uint64
	BytesToRing   uint64
	RingResidual  int

	// Streamer diagnostics
	SamplesPlayed uint64
	Underflows    uint64
	SilentBytes   uint64
}

// GetDiagnostics returns a snapshot of all diagnostic counters.
func (p *RawAudioPlayer) GetDiagnostics() Diagnostics {
	d := Diagnostics{
		Mode:           p.mode,
		FramesPerBuf:   p.framesPerBuf,
		SampleRate:     p.sampleRate,
		Channels:       p.channels,
		BytesPerSample: p.bytesPerSample,
		BytesFromFile:  p.bytesFromFile.Load(),
		BytesToRing:    p.bytesToRing.Load(),
	}
	if p.streamer != nil {
		d.RingResidual = p.streamer.Buffered()
	}
	if s, ok := p.streamer.(*paStreamer); ok {
		ringSize := s.sampleRate * s.channels * s.bytesPerSample * s.ringMs / 1000
		d.RingSize = ringSize
		d.RingMs = float64(ringSize) / float64(s.sampleRate*s.channels*s.bytesPerSample) * 1000
		d.SamplesPlayed = s.SamplesPlayed()
		d.Underflows = s.Underflows()
		d.SilentBytes = s.silentBytes.Load()
	}
	return d
}

// PrintDiagnostics writes a human-readable diagnostics report to w.
func (p *RawAudioPlayer) PrintDiagnostics(w io.Writer) {
	d := p.GetDiagnostics()

	fmt.Fprintf(w, "\nDiagnostics (%d frames/buffer, ring %d bytes / %.0f ms, %s mode):\n",
		d.FramesPerBuf, d.RingSize, d.RingMs, d.Mode)

	if d.Underflows > 0 {
		fmt.Fprintf(w, "  Underflows:         %d\n", d.Underflows)
	}
	if d.SilentBytes > 0 {
		silentMs := float64(d.SilentBytes) / float64(d.SampleRate*d.Channels*d.BytesPerSample) * 1000
		fmt.Fprintf(w, "  Silence inserted:   %d bytes (%.1f ms)\n", d.SilentBytes, silentMs)
	}

	fmt.Fprintf(w, "\n  Data integrity:\n")
	fmt.Fprintf(w, "    File → Streamer:  %d read, %d written", d.BytesFromFile, d.BytesToRing)
	if d.BytesFromFile == d.BytesToRing {
		fmt.Fprintf(w, "  ✓\n")
	} else {
		fmt.Fprintf(w, "  MISMATCH (lost %d bytes)\n", d.BytesFromFile-d.BytesToRing)
	}
	fmt.Fprintf(w, "    Ring residual:    %d bytes\n", d.RingResidual)
	fmt.Fprintf(w, "    Samples played:   %d\n", d.SamplesPlayed)

	duration := time.Duration(0)
	if d.SampleRate > 0 {
		duration = time.Duration(float64(d.SamplesPlayed) / float64(d.SampleRate) * float64(time.Second))
	}
	fmt.Fprintf(w, "    Duration played:  %s\n", duration.Truncate(time.Millisecond))
}
