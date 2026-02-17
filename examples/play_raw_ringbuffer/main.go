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
	"github.com/smallnest/ringbuffer"
)

// RawAudioPlayer plays raw PCM audio files using a ring buffer
// to decouple file I/O from the real-time audio callback.
//
// Architecture:
//
//	Producer goroutine          Ring Buffer           Audio Callback
//	┌──────────────┐          ┌──────────┐          ┌──────────────┐
//	│ io.ReadFull   │──write─▶│ ●●●●●○○○ │──read──▶ │ copy to output│
//	│ (blocking OK) │          └──────────┘          │ (non-blocking)│
//	└──────────────┘                                 └──────────────┘
type RawAudioPlayer struct {
	stream         *portaudio.PaStream
	file           *os.File
	ring           *ringbuffer.RingBuffer
	channels       int
	sampleRate     int
	bytesPerSample int
	framesPerBuf   int
	ringSize       int
	samplesPlayed  atomic.Uint64
	underflows     atomic.Uint64
	silentBytes    atomic.Uint64 // bytes of silence inserted due to partial reads
	eof            atomic.Bool   // file fully read by producer
	drained        atomic.Bool   // ring buffer drained after EOF

	// Data integrity counters — track bytes at each pipeline stage
	bytesFromFile atomic.Uint64 // total bytes returned by file.Read (producer)
	bytesToRing   atomic.Uint64 // total bytes accepted by ring.Write (producer)
	bytesFromRing atomic.Uint64 // total bytes returned by ring.TryRead (callback)
	bytesOutput   atomic.Uint64 // total bytes delivered to PortAudio (ring data + silence)

	// Diagnostics (written only from the audio callback thread)
	callbackCount    atomic.Uint64
	lastCallbackNano atomic.Int64
	maxIntervalNs    atomic.Int64
	totalIntervalNs  atomic.Int64
	maxDurationNs    atomic.Int64
	totalDurationNs  atomic.Int64
	minBufferFill    atomic.Int64
}

func main() {
	deviceIdx := flag.Int("device", 1, "Audio output device index")
	channels := flag.Int("channels", 2, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("samplerate", 44100, "Sample rate in Hz")
	bitsPerSample := flag.Int("bitspersample", 16, "Bits per sample (8, 16, 24, 32)")
	bufferFrames := flag.Int("buffer", 512, "Frames per PortAudio callback buffer")
	ringMs := flag.Int("ringms", 250, "Ring buffer size in milliseconds of audio")
	inputFile := flag.String("in", "", "Input raw audio file (required)")
	listDevices := flag.Bool("list", false, "List available output devices and exit")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: play_raw_ringbuffer [options]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Plays a raw PCM audio file using a ring buffer for real-time safe callback.")
		fmt.Fprintln(os.Stderr, "Unlike play_raw, the audio callback never does file I/O or blocking operations.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  play_raw_ringbuffer -list")
		fmt.Fprintln(os.Stderr, "  play_raw_ringbuffer -in recording.raw -channels 1 -samplerate 44100")
		fmt.Fprintln(os.Stderr, "  play_raw_ringbuffer -in audio.raw -channels 2 -bitspersample 24 -samplerate 48000 -device 1")
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

	sampleFormat, err := sampleFormatFromBits(*bitsPerSample)
	if err != nil {
		log.Fatal(err)
	}

	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		log.Fatalf("Failed to get device %d: %v", *deviceIdx, err)
	}
	fmt.Printf("Using output device %d: %s\n", *deviceIdx, device.Name)

	player, err := NewRawAudioPlayer(*deviceIdx, *channels, sampleFormat, *sampleRate, *bufferFrames, *ringMs, *inputFile)
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
	fmt.Printf("Configuration: %d channel(s), %d Hz, %d-bit PCM, buffer %d frames, ring %d ms\n",
		*channels, *sampleRate, *bitsPerSample, *bufferFrames, *ringMs)
	fmt.Println("Press Ctrl-C to stop playback")

	if err := player.Start(ctx); err != nil {
		log.Fatal("Failed to start playback:", err)
	}

	// Wait for playback to finish or interrupt
	for !player.IsDrained() {
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

func NewRawAudioPlayer(device int, channels int, sampleFormat portaudio.PaSampleFormat, sampleRate int, framesPerBuffer int, ringMs int, inputFile string) (*RawAudioPlayer, error) {
	file, err := os.Open(inputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}

	stream, err := portaudio.NewCallbackStream(device, channels, sampleFormat, float64(sampleRate))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create callback stream: %w", err)
	}

	bytesPerSample := portaudio.GetSampleSize(sampleFormat)
	ringSize := sampleRate * channels * bytesPerSample * ringMs / 1000

	player := &RawAudioPlayer{
		stream:         stream,
		file:           file,
		ring:           ringbuffer.New(ringSize),
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
func (p *RawAudioPlayer) Start(ctx context.Context) error {
	// Start producer goroutine: reads file → ring buffer
	go p.producer(ctx)

	// Open and start audio stream
	if err := p.stream.OpenCallback(p.framesPerBuf, p.audioCallback); err != nil {
		return fmt.Errorf("failed to open callback: %w", err)
	}
	if err := p.stream.StartStream(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
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
			written, _ := p.ring.Write(buf[:n])
			for written < n {
				time.Sleep(500 * time.Microsecond)
				w, _ := p.ring.Write(buf[written:n])
				written += w
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

// audioCallback is called by PortAudio on the real-time audio thread.
// It only reads from the ring buffer — no file I/O, no locks, no allocations.
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
	fill := int64(p.ring.Length())
	if fill < p.minBufferFill.Load() {
		p.minBufferFill.Store(fill)
	}

	bytesNeeded := int(frameCount) * p.channels * p.bytesPerSample
	n, _ := p.ring.TryRead(output[:bytesNeeded])
	p.bytesFromRing.Add(uint64(n))
	p.bytesOutput.Add(uint64(bytesNeeded))

	if n < bytesNeeded {
		// Fill remainder with silence
		clear(output[n:bytesNeeded])

		// If the producer has finished reading the file and the ring
		// buffer is now empty, playback is done.
		if p.eof.Load() && p.ring.Length() == 0 {
			p.drained.Store(true)
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
	if err := p.stream.CloseCallback(); err != nil {
		errs = append(errs, fmt.Errorf("close callback: %w", err))
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

func (p *RawAudioPlayer) IsDrained() bool {
	return p.drained.Load()
}

func (p *RawAudioPlayer) PrintDiagnostics() {
	count := p.callbackCount.Load()
	if count < 2 {
		return
	}

	expectedMs := float64(p.framesPerBuf) / float64(p.sampleRate) * 1000
	budgetUs := expectedMs * 1000

	avgIntervalMs := float64(p.totalIntervalNs.Load()) / float64(count-1) / 1e6
	maxIntervalMs := float64(p.maxIntervalNs.Load()) / 1e6
	avgDurationUs := float64(p.totalDurationNs.Load()) / float64(count) / 1e3
	maxDurationUs := float64(p.maxDurationNs.Load()) / 1e3
	minFill := p.minBufferFill.Load()
	ringMs := float64(p.ringSize) / float64(p.sampleRate*p.channels*p.bytesPerSample) * 1000

	fmt.Printf("\nDiagnostics (%d callbacks, %d frames/buffer, ring %d bytes / %.0f ms):\n",
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

	// Data integrity report
	fromFile := p.bytesFromFile.Load()
	toRing := p.bytesToRing.Load()
	fromRing := p.bytesFromRing.Load()
	output := p.bytesOutput.Load()
	silence := p.silentBytes.Load()
	residual := p.ring.Length()

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
	fmt.Printf("    Output total:   %d bytes (%d audio + %d silence)\n", output, fromRing, silence)
}
