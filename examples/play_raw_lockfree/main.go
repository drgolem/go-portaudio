package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/drgolem/go-portaudio/portaudio"
)

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
	file           *os.File
	ring           *SPSCRingBuffer
	mode           string // "callback" or "stream"
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

func main() {
	deviceIdx := flag.Int("device", 1, "Audio output device index")
	channels := flag.Int("channels", 2, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("samplerate", 44100, "Sample rate in Hz")
	bitsPerSample := flag.Int("bitspersample", 16, "Bits per sample (8, 16, 24, 32)")
	bufferFrames := flag.Int("buffer", 512, "Frames per PortAudio buffer")
	ringMs := flag.Int("ringms", 250, "Ring buffer size in milliseconds of audio")
	mode := flag.String("mode", "callback", "Audio output mode: callback (low latency) or stream (blocking I/O)")
	inputFile := flag.String("in", "", "Input raw audio file (required)")
	listDevices := flag.Bool("list", false, "List available output devices and exit")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: play_raw_lockfree [options]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Plays a raw PCM audio file using a lock-free SPSC ring buffer.")
		fmt.Fprintln(os.Stderr, "Supports two modes: callback (low latency) and stream (blocking I/O).")
		fmt.Fprintln(os.Stderr, "Both modes use the same lock-free ring buffer for file I/O decoupling.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  play_raw_lockfree -list")
		fmt.Fprintln(os.Stderr, "  play_raw_lockfree -in recording.raw -channels 1 -samplerate 44100")
		fmt.Fprintln(os.Stderr, "  play_raw_lockfree -in audio.raw -mode stream -buffer 1024")
		fmt.Fprintln(os.Stderr, "  play_raw_lockfree -in audio.raw -channels 2 -bitspersample 24 -samplerate 48000 -device 1")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Note: Input must be raw PCM (signed integer, little-endian).")
	}
	flag.Parse()

	if err := portaudio.Initialize(); err != nil {
		log.Fatal("Failed to initialize PortAudio:", err)
	}
	defer portaudio.Terminate()

	fmt.Printf("PortAudio version: %s\n", portaudio.GetVersionText())

	if *listDevices {
		listOutputDevices()
		return
	}

	if *inputFile == "" {
		fmt.Fprintln(os.Stderr, "Error: Input file is required")
		flag.Usage()
		os.Exit(1)
	}

	if *mode != "callback" && *mode != "stream" {
		log.Fatalf("Invalid mode %q: must be 'callback' or 'stream'", *mode)
	}

	sampleFormat, err := sampleFormatFromBits(*bitsPerSample)
	if err != nil {
		log.Fatal(err)
	}

	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		log.Fatalf("Failed to get device %d: %v", *deviceIdx, err)
	}
	fmt.Printf("Using output device %d: %s\n", *deviceIdx, device.Name)

	player, err := NewRawAudioPlayer(*deviceIdx, *channels, sampleFormat, *sampleRate, *bufferFrames, *ringMs, *mode, *inputFile)
	if err != nil {
		log.Fatal("Failed to create player:", err)
	}
	defer player.Close()

	if fileInfo, err := os.Stat(*inputFile); err == nil {
		fileSize := fileInfo.Size()
		bytesPerSample := *bitsPerSample / 8
		samples := fileSize / int64(*channels*bytesPerSample)
		durationSec := float64(samples) / float64(*sampleRate)
		fmt.Printf("File size: %d bytes (%.2f seconds)\n", fileSize, durationSec)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Playing %s...\n", *inputFile)
	fmt.Printf("Configuration: %d channel(s), %d Hz, %d-bit PCM, buffer %d frames, ring %d ms, mode %s\n",
		*channels, *sampleRate, *bitsPerSample, *bufferFrames, *ringMs, *mode)
	fmt.Println("Press Ctrl-C to stop playback")

	if err := player.Start(ctx); err != nil {
		log.Fatal("Failed to start playback:", err)
	}

	// Wait for playback to finish or interrupt
	for !player.Done() {
		select {
		case <-ctx.Done():
			fmt.Println("\nPlayback interrupted")
			goto done
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	fmt.Println("\nPlayback complete")

done:
	if err := player.Stop(); err != nil {
		log.Println("Warning: Error stopping player:", err)
	}

	samples := player.GetSamplesPlayed()
	durationSec := float64(samples) / float64(*sampleRate)
	fmt.Printf("Played: %d samples (%.2f seconds)\n", samples, durationSec)

	player.PrintDiagnostics()
}

func listOutputDevices() {
	devices, err := portaudio.Devices()
	if err != nil {
		log.Fatal("Failed to get devices:", err)
	}

	fmt.Println("\nAvailable Output Devices:")
	fmt.Println("=========================")

	for i, device := range devices {
		if device.MaxOutputChannels > 0 {
			fmt.Printf("Device %d: %s\n", i, device.Name)
			fmt.Printf("  Channels: %d output\n", device.MaxOutputChannels)
			fmt.Printf("  Sample Rate: %.0f Hz\n", device.DefaultSampleRate)
			fmt.Printf("  Low Latency: %.1f ms\n", float64(device.DefaultLowOutputLatency)*1000)
			fmt.Printf("  High Latency: %.1f ms\n", float64(device.DefaultHighOutputLatency)*1000)
			fmt.Println()
		}
	}
}

func sampleFormatFromBits(bits int) (portaudio.PaSampleFormat, error) {
	switch bits {
	case 8:
		return portaudio.SampleFmtInt8, nil
	case 16:
		return portaudio.SampleFmtInt16, nil
	case 24:
		return portaudio.SampleFmtInt24, nil
	case 32:
		return portaudio.SampleFmtInt32, nil
	default:
		return 0, fmt.Errorf("unsupported bits per sample: %d (use 8, 16, 24, or 32)", bits)
	}
}

func NewRawAudioPlayer(device int, channels int, sampleFormat portaudio.PaSampleFormat, sampleRate int, framesPerBuffer int, ringMs int, mode string, inputFile string) (*RawAudioPlayer, error) {
	file, err := os.Open(inputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}

	var stream *portaudio.PaStream
	switch mode {
	case "stream":
		stream, err = portaudio.NewOutputStream(device, channels, sampleFormat, float64(sampleRate))
	default:
		stream, err = portaudio.NewCallbackStream(device, channels, sampleFormat, float64(sampleRate))
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	bytesPerSample := portaudio.GetSampleSize(sampleFormat)
	ringSize := sampleRate * channels * bytesPerSample * ringMs / 1000

	player := &RawAudioPlayer{
		stream:         stream,
		file:           file,
		ring:           NewSPSCRingBuffer(ringSize),
		mode:           mode,
		channels:       channels,
		sampleRate:     sampleRate,
		bytesPerSample: bytesPerSample,
		framesPerBuf:   framesPerBuffer,
		ringSize:       ringSize,
	}
	player.minBufferFill.Store(int64(ringSize))

	return player, nil
}

// Start begins the producer goroutine and opens the audio stream.
// In callback mode, PortAudio pulls data via the audio callback.
// In stream mode, a writer goroutine pushes data via stream.Write.
func (p *RawAudioPlayer) Start(ctx context.Context) error {
	// Producer is the same in both modes: file → ring buffer
	go p.producer(ctx)

	switch p.mode {
	case "stream":
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

// producer reads audio data from the file into the ring buffer.
// Runs in its own goroutine — blocking file I/O is safe here.
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

		n, err := p.file.Read(buf)
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

func (p *RawAudioPlayer) Stop() error {
	if p.stream == nil {
		return nil
	}
	var errs []error
	if err := p.stream.StopStream(); err != nil {
		errs = append(errs, fmt.Errorf("stop stream: %w", err))
	}
	switch p.mode {
	case "stream":
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

func (p *RawAudioPlayer) Close() error {
	p.Stop()
	if p.file != nil {
		return p.file.Close()
	}
	return nil
}

func (p *RawAudioPlayer) GetSamplesPlayed() uint64 {
	return p.samplesPlayed.Load()
}

func (p *RawAudioPlayer) Done() bool {
	return p.done.Load()
}

func (p *RawAudioPlayer) PrintDiagnostics() {
	ringMs := float64(p.ringSize) / float64(p.sampleRate*p.channels*p.bytesPerSample) * 1000
	expectedMs := float64(p.framesPerBuf) / float64(p.sampleRate) * 1000
	minFill := p.minBufferFill.Load()

	switch p.mode {
	case "stream":
		count := p.writeCount.Load()
		if count == 0 {
			return
		}
		avgWriteMs := float64(p.totalWriteNs.Load()) / float64(count) / 1e6
		maxWriteMs := float64(p.maxWriteNs.Load()) / 1e6

		fmt.Printf("\nDiagnostics (%d writes, %d frames/buffer, ring %d bytes / %.0f ms, stream mode):\n",
			count, p.framesPerBuf, p.ringSize, ringMs)
		fmt.Printf("  Write duration:     avg %.2f ms,  max %.2f ms  (expected ~%.2f ms)\n",
			avgWriteMs, maxWriteMs, expectedMs)
		fmt.Printf("  Min buffer fill:    %d bytes (%.1f%%)\n",
			minFill, float64(minFill)/float64(p.ringSize)*100)
		if e := p.writeErrors.Load(); e > 0 {
			fmt.Printf("  Write errors:       %d\n", e)
		}

	default: // callback
		count := p.callbackCount.Load()
		if count < 2 {
			return
		}
		budgetUs := expectedMs * 1000
		avgIntervalMs := float64(p.totalIntervalNs.Load()) / float64(count-1) / 1e6
		maxIntervalMs := float64(p.maxIntervalNs.Load()) / 1e6
		avgDurationUs := float64(p.totalDurationNs.Load()) / float64(count) / 1e3
		maxDurationUs := float64(p.maxDurationNs.Load()) / 1e3

		fmt.Printf("\nDiagnostics (%d callbacks, %d frames/buffer, ring %d bytes / %.0f ms, callback mode):\n",
			count, p.framesPerBuf, p.ringSize, ringMs)
		fmt.Printf("  Callback interval:  avg %.2f ms,  max %.2f ms  (expected %.2f ms)\n",
			avgIntervalMs, maxIntervalMs, expectedMs)
		fmt.Printf("  Callback duration:  avg %.0f μs,  max %.0f μs  (budget %.0f μs)\n",
			avgDurationUs, maxDurationUs, budgetUs)
		fmt.Printf("  Min buffer fill:    %d bytes (%.1f%%)\n",
			minFill, float64(minFill)/float64(p.ringSize)*100)
		if u := p.underflows.Load(); u > 0 {
			fmt.Printf("  Underflows:         %d\n", u)
		}
		if s := p.silentBytes.Load(); s > 0 {
			silentMs := float64(s) / float64(p.sampleRate*p.channels*p.bytesPerSample) * 1000
			fmt.Printf("  Silence inserted:   %d bytes (%.1f ms)\n", s, silentMs)
		}
	}

	// Data integrity report (common to both modes)
	fromFile := p.bytesFromFile.Load()
	toRing := p.bytesToRing.Load()
	fromRing := p.bytesFromRing.Load()
	output := p.bytesOutput.Load()
	residual := p.ring.Available()

	fmt.Printf("\n  Data integrity:\n")
	fmt.Printf("    File → Ring:    %d read, %d written", fromFile, toRing)
	if fromFile == toRing {
		fmt.Printf("  ✓\n")
	} else {
		fmt.Printf("  MISMATCH (lost %d bytes)\n", fromFile-toRing)
	}
	fmt.Printf("    Ring → Output:  %d read, %d residual", fromRing, residual)
	if fromRing+uint64(residual) == toRing {
		fmt.Printf("  ✓\n")
	} else {
		fmt.Printf("  MISMATCH (expected %d, got %d)\n", toRing, fromRing+uint64(residual))
	}
	fmt.Printf("    Output total:   %d bytes (%d audio + %d silence)\n", output, fromRing, p.silentBytes.Load())
}
