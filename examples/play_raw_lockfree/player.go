package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/drgolem/go-portaudio/portaudio"
)

// PlayerMode selects how audio data is delivered to PortAudio.
type PlayerMode string

const (
	// ModeCallback uses PortAudio's callback API (low latency).
	// The audio callback reads from the SPSC ring buffer on the RT thread.
	ModeCallback PlayerMode = "callback"
	// ModeStream uses PortAudio's blocking Write API (higher latency).
	// A writer goroutine reads from the SPSC ring buffer and calls stream.Write.
	ModeStream PlayerMode = "stream"
)

// SampleFormat describes the PCM sample encoding.
type SampleFormat int

const (
	SampleFmtInt8    SampleFormat = iota + 1
	SampleFmtInt16
	SampleFmtInt24
	SampleFmtInt32
	SampleFmtFloat32
)

// BytesPerSample returns the number of bytes occupied by one sample.
func (f SampleFormat) BytesPerSample() int {
	switch f {
	case SampleFmtInt8:
		return 1
	case SampleFmtInt16:
		return 2
	case SampleFmtInt24:
		return 3
	case SampleFmtInt32, SampleFmtFloat32:
		return 4
	default:
		return 0
	}
}

func (f SampleFormat) toPaSampleFormat() portaudio.PaSampleFormat {
	switch f {
	case SampleFmtInt8:
		return portaudio.SampleFmtInt8
	case SampleFmtInt16:
		return portaudio.SampleFmtInt16
	case SampleFmtInt24:
		return portaudio.SampleFmtInt24
	case SampleFmtInt32:
		return portaudio.SampleFmtInt32
	case SampleFmtFloat32:
		return portaudio.SampleFmtFloat32
	default:
		return portaudio.SampleFmtInt16
	}
}

// RawAudioPlayer plays raw PCM audio files using a lock-free SPSC
// ring buffer to decouple file I/O from audio output.
//
// Supports two modes controlled by -mode flag:
//
// Callback mode (default, low latency):
//
//	Producer goroutine     SPSC Ring Buffer     Audio Callback (RT thread)
//	┌──────────────┐     ┌──────────────┐     ┌──────────────┐
//	│ file.Read     │─W─▶│ atomic w/r   │──R─▶│ copy to output│
//	└──────────────┘     └──────────────┘     └──────────────┘
//
// Stream mode (blocking I/O, higher latency):
//
//	Producer goroutine     SPSC Ring Buffer     Writer goroutine
//	┌──────────────┐     ┌──────────────┐     ┌──────────────────┐
//	│ file.Read     │─W─▶│ atomic w/r   │──R─▶│ stream.Write (blocks)│
//	└──────────────┘     └──────────────┘     └──────────────────┘
//
// Both modes use the same lock-free SPSC ring buffer and producer.
// Only the consumer side differs: callback vs blocking write loop.
type RawAudioPlayer struct {
	stream         *portaudio.PaStream
	reader         io.ReadCloser
	ring           *SPSCRingBuffer
	mode           PlayerMode
	channels       int
	sampleRate     int
	bytesPerSample int
	framesPerBuf   int
	ringSize       int
	samplesPlayed  atomic.Uint64
	underflows     atomic.Uint64
	silentBytes    atomic.Uint64 // bytes of silence inserted due to partial reads
	eof            atomic.Bool   // file fully read by producer
	done           atomic.Bool   // ring buffer done after EOF

	// Data integrity counters — track bytes at each pipeline stage:
	//   File → [file.Read] → buf → [ring.Write] → Ring → [ring.Read] → output
	bytesFromFile atomic.Uint64 // total bytes returned by file.Read (producer)
	bytesToRing   atomic.Uint64 // total bytes accepted by ring.Write (producer)
	bytesFromRing atomic.Uint64 // total bytes returned by ring.Read (consumer)
	bytesOutput   atomic.Uint64 // total bytes delivered to PortAudio

	// Callback mode diagnostics (written only from the audio callback thread)
	callbackCount    atomic.Uint64
	lastCallbackNano atomic.Int64
	maxIntervalNs    atomic.Int64
	totalIntervalNs  atomic.Int64
	maxDurationNs    atomic.Int64
	totalDurationNs  atomic.Int64
	minBufferFill    atomic.Int64

	// Stream mode diagnostics (written only from the writer goroutine)
	writeCount   atomic.Uint64
	maxWriteNs   atomic.Int64
	totalWriteNs atomic.Int64
	writeErrors  atomic.Uint64
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
func NewRawAudioPlayer(cfg PlayerConfig, source io.ReadCloser) (*RawAudioPlayer, error) {
	paFmt := cfg.SampleFormat.toPaSampleFormat()
	var stream *portaudio.PaStream
	var err error
	switch cfg.Mode {
	case ModeStream:
		stream, err = portaudio.NewOutputStream(cfg.Device, cfg.Channels, paFmt, float64(cfg.SampleRate))
	default:
		stream, err = portaudio.NewCallbackStream(cfg.Device, cfg.Channels, paFmt, float64(cfg.SampleRate))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	bytesPerSample := cfg.SampleFormat.BytesPerSample()
	ringSize := cfg.SampleRate * cfg.Channels * bytesPerSample * cfg.RingMs / 1000

	player := &RawAudioPlayer{
		stream:         stream,
		reader:         source,
		ring:           NewSPSCRingBuffer(ringSize),
		mode:           cfg.Mode,
		channels:       cfg.Channels,
		sampleRate:     cfg.SampleRate,
		bytesPerSample: bytesPerSample,
		framesPerBuf:   cfg.FramesPerBuf,
		ringSize:       ringSize,
	}
	player.minBufferFill.Store(int64(ringSize))

	return player, nil
}

// Start begins the producer goroutine and opens the audio stream.
// In callback mode, PortAudio pulls data via the audio callback.
// In stream mode, a writer goroutine pushes data via stream.Write.
func (p *RawAudioPlayer) Start(ctx context.Context) error {
	// Producer is the same in both modes: source → ring buffer
	go p.producer(ctx)

	switch p.mode {
	case ModeStream:
		if err := p.stream.Open(p.framesPerBuf); err != nil {
			return fmt.Errorf("failed to open stream: %w", err)
		}
		if err := p.stream.StartStream(); err != nil {
			return fmt.Errorf("failed to start stream: %w", err)
		}
		go p.writer(ctx)
	default:
		if err := p.stream.OpenCallback(p.framesPerBuf, p.audioCallback); err != nil {
			return fmt.Errorf("failed to open callback: %w", err)
		}
		if err := p.stream.StartStream(); err != nil {
			return fmt.Errorf("failed to start stream: %w", err)
		}
	}
	return nil
}

// producer reads audio data from the source into the ring buffer.
// Runs in its own goroutine — blocking I/O is safe here.
func (p *RawAudioPlayer) producer(ctx context.Context) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Wait for space in the ring buffer
		if p.ring.Free() < len(buf) {
			time.Sleep(1 * time.Millisecond)
			continue
		}

		n, err := p.reader.Read(buf)
		if n > 0 {
			p.bytesFromFile.Add(uint64(n))
			written := p.ring.Write(buf[:n])
			for written < n {
				time.Sleep(500 * time.Microsecond)
				written += p.ring.Write(buf[written:n])
			}
			p.bytesToRing.Add(uint64(written))
		}
		if err != nil {
			if err == io.EOF {
				p.eof.Store(true)
			}
			return
		}
	}
}

// writer reads from the ring buffer and writes to the stream using
// blocking I/O. Used in "stream" mode as an alternative to the callback.
// Pa_WriteStream blocks until PortAudio has consumed the buffer, so
// this naturally paces itself.
func (p *RawAudioPlayer) writer(ctx context.Context) {
	frameSize := p.channels * p.bytesPerSample
	bufSize := p.framesPerBuf * frameSize
	buf := make([]byte, bufSize)
	pending := 0 // leftover bytes from previous read (partial frame)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Track ring buffer fill before reading
		fill := int64(p.ring.Available())
		if fill < p.minBufferFill.Load() {
			p.minBufferFill.Store(fill)
		}

		// Read from ring buffer, appending after any pending partial-frame bytes
		n := p.ring.Read(buf[pending : pending+bufSize-pending])
		total := pending + n
		p.bytesFromRing.Add(uint64(n))

		if total < frameSize {
			if p.eof.Load() && p.ring.Available() == 0 {
				p.done.Store(true)
				return
			}
			// Not enough for a full frame yet — wait for producer
			pending = total
			time.Sleep(1 * time.Millisecond)
			continue
		}

		// Write frame-aligned portion
		frames := total / frameSize
		alignedBytes := frames * frameSize

		startNano := time.Now().UnixNano()
		err := p.stream.Write(frames, buf[:alignedBytes])
		duration := time.Now().UnixNano() - startNano

		p.totalWriteNs.Add(duration)
		if duration > p.maxWriteNs.Load() {
			p.maxWriteNs.Store(duration)
		}
		p.writeCount.Add(1)

		if err != nil {
			p.writeErrors.Add(1)
		} else {
			p.bytesOutput.Add(uint64(alignedBytes))
			p.samplesPlayed.Add(uint64(frames))
		}

		// Carry over partial frame bytes for next iteration
		pending = total - alignedBytes
		if pending > 0 {
			copy(buf[:pending], buf[alignedBytes:total])
		}
	}
}

// audioCallback is called by PortAudio on the real-time audio thread.
// It only calls SPSCRingBuffer.Read which is lock-free: two atomic
// loads and one atomic store — no mutexes, no TryLock, no CAS loops.
func (p *RawAudioPlayer) audioCallback(
	input, output []byte,
	frameCount uint,
	timeInfo *portaudio.StreamCallbackTimeInfo,
	statusFlags portaudio.StreamCallbackFlags,
) portaudio.StreamCallbackResult {
	startNano := time.Now().UnixNano()

	// Track interval between callbacks
	if last := p.lastCallbackNano.Swap(startNano); last != 0 {
		interval := startNano - last
		p.totalIntervalNs.Add(interval)
		if interval > p.maxIntervalNs.Load() {
			p.maxIntervalNs.Store(interval)
		}
	}

	if statusFlags&portaudio.OutputUnderflow != 0 {
		p.underflows.Add(1)
	}

	// Track buffer fill before reading
	fill := int64(p.ring.Available())
	if fill < p.minBufferFill.Load() {
		p.minBufferFill.Store(fill)
	}

	bytesNeeded := int(frameCount) * p.channels * p.bytesPerSample
	n := p.ring.Read(output[:bytesNeeded])
	p.bytesFromRing.Add(uint64(n))
	p.bytesOutput.Add(uint64(bytesNeeded))

	if n < bytesNeeded {
		// Fill remainder with silence
		clear(output[n:bytesNeeded])

		// If the producer has finished reading the file and the ring
		// buffer is now empty, playback is done.
		if p.eof.Load() && p.ring.Available() == 0 {
			p.done.Store(true)
			p.samplesPlayed.Add(uint64(n / (p.channels * p.bytesPerSample)))
			p.trackCallbackDuration(startNano)
			return portaudio.Complete
		}

		// Mid-playback silence — producer couldn't keep up
		p.silentBytes.Add(uint64(bytesNeeded - n))
		if n == 0 {
			p.underflows.Add(1)
		}
	}

	p.samplesPlayed.Add(uint64(n / (p.channels * p.bytesPerSample)))
	p.trackCallbackDuration(startNano)
	return portaudio.Continue
}

// trackCallbackDuration records how long the callback took.
// Only called from the audio thread — single writer, so no CAS needed.
func (p *RawAudioPlayer) trackCallbackDuration(startNano int64) {
	duration := time.Now().UnixNano() - startNano
	p.totalDurationNs.Add(duration)
	if duration > p.maxDurationNs.Load() {
		p.maxDurationNs.Store(duration)
	}
	p.callbackCount.Add(1)
}

// Stop stops the audio stream and releases PortAudio resources.
// Safe to call multiple times.
func (p *RawAudioPlayer) Stop() error {
	if p.stream == nil {
		return nil
	}
	var errs []error
	if err := p.stream.StopStream(); err != nil {
		errs = append(errs, fmt.Errorf("stop stream: %w", err))
	}
	switch p.mode {
	case ModeStream:
		if err := p.stream.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close stream: %w", err))
		}
	default:
		if err := p.stream.CloseCallback(); err != nil {
			errs = append(errs, fmt.Errorf("close callback: %w", err))
		}
	}
	p.stream = nil
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
	return p.samplesPlayed.Load()
}

// Done returns true when all audio data has been played.
func (p *RawAudioPlayer) Done() bool {
	return p.done.Load()
}

// Diagnostics returns a snapshot of the player's diagnostic counters.
type Diagnostics struct {
	Mode           PlayerMode
	FramesPerBuf   int
	RingSize       int
	RingMs         float64
	SampleRate     int
	Channels       int
	BytesPerSample int

	// Data integrity
	BytesFromFile uint64
	BytesToRing   uint64
	BytesFromRing uint64
	BytesOutput   uint64
	RingResidual  int

	// Callback mode
	CallbackCount uint64
	AvgIntervalNs float64
	MaxIntervalNs int64
	AvgDurationNs float64
	MaxDurationNs int64
	MinBufferFill int64
	Underflows    uint64
	SilentBytes   uint64

	// Stream mode
	WriteCount  uint64
	AvgWriteNs  float64
	MaxWriteNs  int64
	WriteErrors uint64
}

// GetDiagnostics returns a snapshot of all diagnostic counters.
func (p *RawAudioPlayer) GetDiagnostics() Diagnostics {
	d := Diagnostics{
		Mode:           p.mode,
		FramesPerBuf:   p.framesPerBuf,
		RingSize:       p.ringSize,
		RingMs:         float64(p.ringSize) / float64(p.sampleRate*p.channels*p.bytesPerSample) * 1000,
		SampleRate:     p.sampleRate,
		Channels:       p.channels,
		BytesPerSample: p.bytesPerSample,
		BytesFromFile:  p.bytesFromFile.Load(),
		BytesToRing:    p.bytesToRing.Load(),
		BytesFromRing:  p.bytesFromRing.Load(),
		BytesOutput:    p.bytesOutput.Load(),
		RingResidual:   p.ring.Available(),
		MinBufferFill:  p.minBufferFill.Load(),
		Underflows:     p.underflows.Load(),
		SilentBytes:    p.silentBytes.Load(),
	}

	switch p.mode {
	case ModeStream:
		d.WriteCount = p.writeCount.Load()
		d.MaxWriteNs = p.maxWriteNs.Load()
		d.WriteErrors = p.writeErrors.Load()
		if d.WriteCount > 0 {
			d.AvgWriteNs = float64(p.totalWriteNs.Load()) / float64(d.WriteCount)
		}
	default:
		d.CallbackCount = p.callbackCount.Load()
		d.MaxIntervalNs = p.maxIntervalNs.Load()
		d.MaxDurationNs = p.maxDurationNs.Load()
		if d.CallbackCount > 1 {
			d.AvgIntervalNs = float64(p.totalIntervalNs.Load()) / float64(d.CallbackCount-1)
		}
		if d.CallbackCount > 0 {
			d.AvgDurationNs = float64(p.totalDurationNs.Load()) / float64(d.CallbackCount)
		}
	}

	return d
}

// PrintDiagnostics writes a human-readable diagnostics report to w.
func (p *RawAudioPlayer) PrintDiagnostics(w io.Writer) {
	d := p.GetDiagnostics()
	expectedMs := float64(d.FramesPerBuf) / float64(d.SampleRate) * 1000

	switch d.Mode {
	case ModeStream:
		if d.WriteCount == 0 {
			return
		}
		fmt.Fprintf(w, "\nDiagnostics (%d writes, %d frames/buffer, ring %d bytes / %.0f ms, stream mode):\n",
			d.WriteCount, d.FramesPerBuf, d.RingSize, d.RingMs)
		fmt.Fprintf(w, "  Write duration:     avg %.2f ms,  max %.2f ms  (expected ~%.2f ms)\n",
			d.AvgWriteNs/1e6, float64(d.MaxWriteNs)/1e6, expectedMs)
		fmt.Fprintf(w, "  Min buffer fill:    %d bytes (%.1f%%)\n",
			d.MinBufferFill, float64(d.MinBufferFill)/float64(d.RingSize)*100)
		if d.WriteErrors > 0 {
			fmt.Fprintf(w, "  Write errors:       %d\n", d.WriteErrors)
		}

	default:
		if d.CallbackCount < 2 {
			return
		}
		budgetUs := expectedMs * 1000
		fmt.Fprintf(w, "\nDiagnostics (%d callbacks, %d frames/buffer, ring %d bytes / %.0f ms, callback mode):\n",
			d.CallbackCount, d.FramesPerBuf, d.RingSize, d.RingMs)
		fmt.Fprintf(w, "  Callback interval:  avg %.2f ms,  max %.2f ms  (expected %.2f ms)\n",
			d.AvgIntervalNs/1e6, float64(d.MaxIntervalNs)/1e6, expectedMs)
		fmt.Fprintf(w, "  Callback duration:  avg %.0f μs,  max %.0f μs  (budget %.0f μs)\n",
			d.AvgDurationNs/1e3, float64(d.MaxDurationNs)/1e3, budgetUs)
		fmt.Fprintf(w, "  Min buffer fill:    %d bytes (%.1f%%)\n",
			d.MinBufferFill, float64(d.MinBufferFill)/float64(d.RingSize)*100)
		if d.Underflows > 0 {
			fmt.Fprintf(w, "  Underflows:         %d\n", d.Underflows)
		}
		if d.SilentBytes > 0 {
			silentMs := float64(d.SilentBytes) / float64(d.SampleRate*d.Channels*d.BytesPerSample) * 1000
			fmt.Fprintf(w, "  Silence inserted:   %d bytes (%.1f ms)\n", d.SilentBytes, silentMs)
		}
	}

	// Data integrity report (common to both modes)
	fmt.Fprintf(w, "\n  Data integrity:\n")
	fmt.Fprintf(w, "    File → Ring:    %d read, %d written", d.BytesFromFile, d.BytesToRing)
	if d.BytesFromFile == d.BytesToRing {
		fmt.Fprintf(w, "  ✓\n")
	} else {
		fmt.Fprintf(w, "  MISMATCH (lost %d bytes)\n", d.BytesFromFile-d.BytesToRing)
	}
	fmt.Fprintf(w, "    Ring → Output:  %d read, %d residual", d.BytesFromRing, d.RingResidual)
	if d.BytesFromRing+uint64(d.RingResidual) == d.BytesToRing {
		fmt.Fprintf(w, "  ✓\n")
	} else {
		fmt.Fprintf(w, "  MISMATCH (expected %d, got %d)\n", d.BytesToRing, d.BytesFromRing+uint64(d.RingResidual))
	}
	fmt.Fprintf(w, "    Output total:   %d bytes (%d audio + %d silence)\n", d.BytesOutput, d.BytesFromRing, d.SilentBytes)
}
