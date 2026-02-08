package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/drgolem/go-portaudio/portaudio"
)

// RawAudioPlayer plays raw PCM audio files
type RawAudioPlayer struct {
	stream         *portaudio.PaStream
	file           *os.File
	channels       int
	sampleRate     int
	bytesPerSample int
	buffer         []byte
	mu             sync.Mutex
	samplesPlayed  atomic.Uint64
	eof            atomic.Bool
}

func main() {
	// Command-line flags
	deviceIdx := flag.Int("device", 1, "Audio output device index (1 for default output)")
	channels := flag.Int("channels", 1, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("rate", 44100, "Sample rate in Hz")
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
		fmt.Fprintln(os.Stderr, "  # Play a mono recording at 44.1kHz")
		fmt.Fprintln(os.Stderr, "  play_raw -in recording.raw -channels 1 -rate 44100")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  # Play a stereo recording at 48kHz on device 1")
		fmt.Fprintln(os.Stderr, "  play_raw -in audio.raw -channels 2 -rate 48000 -device 1")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Note: The input file must be raw PCM data (16-bit signed integer, little-endian).")
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

	// Validate input file
	if *inputFile == "" {
		fmt.Fprintln(os.Stderr, "Error: Input file is required")
		flag.Usage()
		os.Exit(1)
	}

	// Get output device
	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		log.Fatalf("Failed to get device %d: %v", *deviceIdx, err)
	}
	fmt.Printf("Using output device %d: %s\n", *deviceIdx, device.Name)

	// Create player
	player, err := NewRawAudioPlayer(*deviceIdx, *channels, *sampleRate, *inputFile)
	if err != nil {
		log.Fatal("Failed to create player:", err)
	}
	defer player.Close()

	// Get file size for duration estimate
	fileInfo, err := os.Stat(*inputFile)
	if err == nil {
		fileSize := fileInfo.Size()
		samples := fileSize / int64(*channels*2) // 2 bytes per sample
		durationSec := float64(samples) / float64(*sampleRate)
		fmt.Printf("File size: %d bytes (%.2f seconds)\n", fileSize, durationSec)
	}

	// Setup signal handler for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start playback
	fmt.Printf("Playing %s...\n", *inputFile)
	fmt.Printf("Configuration: %d channel(s), %d Hz, 16-bit PCM\n", *channels, *sampleRate)
	fmt.Println("Press Ctrl-C to stop playback")

	if err := player.Start(); err != nil {
		log.Fatal("Failed to start playback:", err)
	}

	// Wait for completion or interrupt
	done := make(chan struct{})
	go func() {
		for !player.IsEOF() {
			// Poll every 100ms
			select {
			case <-ctx.Done():
				return
			default:
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

func NewRawAudioPlayer(device int, channels int, sampleRate int, inputFile string) (*RawAudioPlayer, error) {
	// Open input file
	file, err := os.Open(inputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}

	// Create output stream
	stream, err := portaudio.NewCallbackStream(device, channels, portaudio.SampleFmtInt16, float64(sampleRate))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create callback stream: %w", err)
	}

	player := &RawAudioPlayer{
		stream:         stream,
		file:           file,
		channels:       channels,
		sampleRate:     sampleRate,
		bytesPerSample: 2, // 16-bit = 2 bytes
		buffer:         make([]byte, 4096*channels*2), // Buffer for reading
	}

	return player, nil
}

func (p *RawAudioPlayer) Start() error {
	// Open stream with callback
	err := p.stream.OpenCallback(512, p.audioCallback)
	if err != nil {
		return fmt.Errorf("failed to open callback: %w", err)
	}

	// Start the stream
	if err := p.stream.StartStream(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}

	return nil
}

func (p *RawAudioPlayer) Stop() error {
	if p.stream != nil {
		if err := p.stream.StopStream(); err != nil {
			return fmt.Errorf("failed to stop stream: %w", err)
		}
		if err := p.stream.CloseCallback(); err != nil {
			return fmt.Errorf("failed to close callback: %w", err)
		}
	}
	return nil
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

// audioCallback is called by PortAudio to send audio data
func (p *RawAudioPlayer) audioCallback(
	input, output []byte,
	frameCount uint,
	timeInfo *portaudio.StreamCallbackTimeInfo,
	statusFlags portaudio.StreamCallbackFlags,
) portaudio.StreamCallbackResult {

	// Check for output underflow
	if statusFlags&portaudio.OutputUnderflow != 0 {
		fmt.Fprintf(os.Stderr, "\nWarning: Output underflow detected (audio glitch)\n")
	}

	bytesNeeded := int(frameCount) * p.channels * p.bytesPerSample

	// Read from file
	p.mu.Lock()
	n, err := io.ReadFull(p.file, output[:bytesNeeded])
	p.mu.Unlock()

	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// End of file reached
		p.eof.Store(true)

		// Fill remainder with silence
		if n < bytesNeeded {
			clear(output[n:bytesNeeded])
		}

		return portaudio.Complete
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nError reading from file: %v\n", err)
		return portaudio.Abort
	}

	// Track samples played
	samplesPlayed := n / (p.channels * p.bytesPerSample)
	p.samplesPlayed.Add(uint64(samplesPlayed))

	return portaudio.Continue
}
