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

// RawAudioPlayer plays raw PCM audio files
type RawAudioPlayer struct {
	stream         *portaudio.PaStream
	file           *os.File
	channels       int
	sampleRate     int
	bytesPerSample int
	framesPerBuf   int
	buffer         []byte
	samplesPlayed  atomic.Uint64
	underflows     atomic.Uint64
	eof            atomic.Bool

	// Data integrity counters
	bytesFromFile atomic.Uint64 // total bytes returned by io.ReadFull (callback)
	bytesOutput   atomic.Uint64 // total bytes delivered to PortAudio

	// Diagnostics (written only from the audio callback thread)
	callbackCount    atomic.Uint64
	lastCallbackNano atomic.Int64
	maxIntervalNs    atomic.Int64
	totalIntervalNs  atomic.Int64
	maxDurationNs    atomic.Int64
	totalDurationNs  atomic.Int64
}

func main() {
	deviceIdx := flag.Int("device", 1, "Audio output device index (1 for default output)")
	channels := flag.Int("channels", 2, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("samplerate", 44100, "Sample rate in Hz")
	bitsPerSample := flag.Int("bitspersample", 16, "Bits per sample (8, 16, 24, 32)")
	bufferFrames := flag.Int("buffer", 512, "Frames per PortAudio callback buffer")
	inputFile := flag.String("in", "", "Input raw audio file (required)")
	listDevices := flag.Bool("list", false, "List available output devices and exit")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: play_raw [options]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Plays a raw PCM audio file.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  # List available output devices")
		fmt.Fprintln(os.Stderr, "  play_raw -list")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  # Play a mono 16-bit recording at 44.1kHz")
		fmt.Fprintln(os.Stderr, "  play_raw -in recording.raw -channels 1 -samplerate 44100")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  # Play a stereo 24-bit recording at 48kHz on device 1")
		fmt.Fprintln(os.Stderr, "  play_raw -in audio.raw -channels 2 -bitspersample 24 -samplerate 48000 -device 1")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Note: The input file must be raw PCM data (signed integer, little-endian).")
		fmt.Fprintln(os.Stderr, "      Use record_audio to create compatible recordings.")
	}
	flag.Parse()

	// Initialize PortAudio
	fmt.Println("Initializing PortAudio...")
	if err := portaudio.Initialize(); err != nil {
		log.Fatal("Failed to initialize PortAudio:", err)
	}
	defer portaudio.Terminate()

	fmt.Printf("PortAudio version: %s\n", portaudio.GetVersionText())

	// List devices if requested
	if *listDevices {
		listOutputDevices()
		return
	}

	// Validate input
	if *inputFile == "" {
		fmt.Fprintln(os.Stderr, "Error: Input file is required")
		flag.Usage()
		os.Exit(1)
	}

	sampleFormat, err := sampleFormatFromBits(*bitsPerSample)
	if err != nil {
		log.Fatal(err)
	}

	// Get output device
	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		log.Fatalf("Failed to get device %d: %v", *deviceIdx, err)
	}
	fmt.Printf("Using output device %d: %s\n", *deviceIdx, device.Name)

	bytesPerSample := *bitsPerSample / 8
	player, err := NewRawAudioPlayer(*deviceIdx, *channels, sampleFormat, *sampleRate, *bufferFrames, *inputFile)
	if err != nil {
		log.Fatal("Failed to create player:", err)
	}
	defer player.Close()

	// Get file size for duration estimate
	fileInfo, err := os.Stat(*inputFile)
	if err == nil {
		fileSize := fileInfo.Size()
		samples := fileSize / int64(*channels*bytesPerSample)
		durationSec := float64(samples) / float64(*sampleRate)
		fmt.Printf("File size: %d bytes (%.2f seconds)\n", fileSize, durationSec)
	}

	// Setup signal handler for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Playing %s...\n", *inputFile)
	fmt.Printf("Configuration: %d channel(s), %d Hz, %d-bit PCM, buffer %d frames\n",
		*channels, *sampleRate, *bitsPerSample, *bufferFrames)
	fmt.Println("Press Ctrl-C to stop playback")

	if err := player.Start(); err != nil {
		log.Fatal("Failed to start playback:", err)
	}

	// Wait for completion or interrupt
	done := make(chan struct{})
	go func() {
		for !player.IsEOF() {
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(50 * time.Millisecond)
			}
		}
		close(done)
	}()

	select {
	case <-done:
		fmt.Println("\nPlayback complete")
	case <-ctx.Done():
		fmt.Println("\nPlayback interrupted")
	}

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

func NewRawAudioPlayer(device int, channels int, sampleFormat portaudio.PaSampleFormat, sampleRate int, framesPerBuffer int, inputFile string) (*RawAudioPlayer, error) {
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
	player := &RawAudioPlayer{
		stream:         stream,
		file:           file,
		channels:       channels,
		sampleRate:     sampleRate,
		bytesPerSample: bytesPerSample,
		framesPerBuf:   framesPerBuffer,
		buffer:         make([]byte, 4096*channels*bytesPerSample),
	}

	return player, nil
}

func (p *RawAudioPlayer) Start() error {
	if err := p.stream.OpenCallback(p.framesPerBuf, p.audioCallback); err != nil {
		return fmt.Errorf("failed to open callback: %w", err)
	}
	if err := p.stream.StartStream(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}
	return nil
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

func (p *RawAudioPlayer) IsEOF() bool {
	return p.eof.Load()
}

// audioCallback is called by PortAudio to send audio data.
// Note: this example reads directly from the file in the callback,
// which is not real-time safe. See play_raw_lockfree for a proper approach.
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

	bytesNeeded := int(frameCount) * p.channels * p.bytesPerSample
	n, err := io.ReadFull(p.file, output[:bytesNeeded])
	p.bytesFromFile.Add(uint64(n))
	p.bytesOutput.Add(uint64(bytesNeeded))

	if err == io.EOF || err == io.ErrUnexpectedEOF {
		p.eof.Store(true)
		if n < bytesNeeded {
			clear(output[n:bytesNeeded])
		}
		p.samplesPlayed.Add(uint64(n / (p.channels * p.bytesPerSample)))
		p.trackCallbackDuration(startNano)
		return portaudio.Complete
	}

	if err != nil {
		p.trackCallbackDuration(startNano)
		return portaudio.Abort
	}

	p.samplesPlayed.Add(uint64(n / (p.channels * p.bytesPerSample)))
	p.trackCallbackDuration(startNano)
	return portaudio.Continue
}

func (p *RawAudioPlayer) trackCallbackDuration(startNano int64) {
	duration := time.Now().UnixNano() - startNano
	p.totalDurationNs.Add(duration)
	if duration > p.maxDurationNs.Load() {
		p.maxDurationNs.Store(duration)
	}
	p.callbackCount.Add(1)
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

	fmt.Printf("\nDiagnostics (%d callbacks, %d frames/buffer):\n", count, p.framesPerBuf)
	fmt.Printf("  Callback interval:  avg %.2f ms,  max %.2f ms  (expected %.2f ms)\n",
		avgIntervalMs, maxIntervalMs, expectedMs)
	fmt.Printf("  Callback duration:  avg %.0f μs,  max %.0f μs  (budget %.0f μs)\n",
		avgDurationUs, maxDurationUs, budgetUs)
	if u := p.underflows.Load(); u > 0 {
		fmt.Printf("  Underflows:         %d\n", u)
	}

	// Data integrity report
	fromFile := p.bytesFromFile.Load()
	output := p.bytesOutput.Load()
	silence := output - fromFile

	fmt.Printf("\n  Data integrity:\n")
	fmt.Printf("    File → Output:  %d bytes read\n", fromFile)
	fmt.Printf("    Output total:   %d bytes (%d audio + %d silence)\n", output, fromFile, silence)
}
