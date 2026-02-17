package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/drgolem/go-portaudio/portaudio"
)

// AudioRecorder records audio from an input device to a raw PCM file
type AudioRecorder struct {
	stream         *portaudio.PaStream
	file           *os.File
	channels       int
	sampleFormat   portaudio.PaSampleFormat
	sampleRate     int
	bytesPerSample int
	samplesWritten atomic.Uint64
	overflows      atomic.Uint64
	done           chan struct{}
}

func main() {
	// Command-line flags
	deviceIdx := flag.Int("device", 0, "Audio input device index (0 for default input)")
	channels := flag.Int("channels", 1, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("samplerate", 44100, "Sample rate in Hz")
	bitsPerSample := flag.Int("bitspersample", 16, "Bits per sample (8, 16, 24, 32)")
	outputFile := flag.String("out", "recording.raw", "Output raw audio file")
	duration := flag.Int("duration", 0, "Recording duration in seconds (0=until Ctrl-C)")
	listDevices := flag.Bool("list", false, "List available input devices and exit")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: record_audio [options]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Records audio from an input device and saves to a raw PCM file.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  # List available input devices")
		fmt.Fprintln(os.Stderr, "  record_audio -list")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  # Record from default device, mono, 44.1kHz")
		fmt.Fprintln(os.Stderr, "  record_audio -out recording.raw")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  # Record from device 2, stereo, 24-bit, 48kHz for 10 seconds")
		fmt.Fprintln(os.Stderr, "  record_audio -device 2 -channels 2 -bitspersample 24 -samplerate 48000 -duration 10 -out audio.raw")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "The output is raw PCM data (signed integer, little-endian).")
		fmt.Fprintln(os.Stderr, "Use play_raw to play the recorded audio.")
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
		listInputDevices()
		return
	}

	// Get input device
	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		log.Fatalf("Failed to get device %d: %v", *deviceIdx, err)
	}
	fmt.Printf("Using input device %d: %s\n", *deviceIdx, device.Name)

	// Validate parameters
	if *channels < 1 || *channels > device.MaxInputChannels {
		log.Fatalf("Invalid channel count %d (device supports 1-%d)", *channels, device.MaxInputChannels)
	}

	sampleFormat, err := sampleFormatFromBits(*bitsPerSample)
	if err != nil {
		log.Fatal(err)
	}

	// Create recorder
	recorder, err := NewAudioRecorder(*deviceIdx, *channels, sampleFormat, *sampleRate, *outputFile)
	if err != nil {
		log.Fatal("Failed to create recorder:", err)
	}
	defer recorder.Close()

	// Setup signal handler for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start recording
	fmt.Printf("Recording to %s...\n", *outputFile)
	fmt.Printf("Configuration: %d channel(s), %d Hz, %d-bit PCM\n", *channels, *sampleRate, *bitsPerSample)
	fmt.Println("Press Ctrl-C to stop recording")

	if err := recorder.Start(); err != nil {
		log.Fatal("Failed to start recording:", err)
	}

	// Wait for signal or duration
	if *duration > 0 {
		fmt.Printf("Recording for %d seconds...\n", *duration)
		select {
		case <-ctx.Done():
			// User interrupted
		case <-time.After(time.Duration(*duration) * time.Second):
			// Duration reached
		}
	} else {
		<-ctx.Done()
	}
	fmt.Println("\nStopping recording...")

	if err := recorder.Stop(); err != nil {
		log.Println("Warning: Error stopping recorder:", err)
	}

	samples := recorder.GetSamplesWritten()
	durationSec := float64(samples) / float64(*sampleRate)
	fmt.Printf("Recording complete: %d samples (%.2f seconds)\n", samples, durationSec)
	fmt.Printf("Output file: %s\n", *outputFile)
	if o := recorder.overflows.Load(); o > 0 {
		fmt.Printf("Warning: %d input overflow(s) detected (audio data lost)\n", o)
	}
}

func listInputDevices() {
	devices, err := portaudio.Devices()
	if err != nil {
		log.Fatal("Failed to get devices:", err)
	}

	fmt.Println("\nAvailable Input Devices:")
	fmt.Println("========================")

	for i, device := range devices {
		if device.MaxInputChannels > 0 {
			fmt.Printf("Device %d: %s\n", i, device.Name)
			fmt.Printf("  Channels: %d input\n", device.MaxInputChannels)
			fmt.Printf("  Sample Rate: %.0f Hz\n", device.DefaultSampleRate)
			fmt.Printf("  Low Latency: %.1f ms\n", float64(device.DefaultLowInputLatency)*1000)
			fmt.Printf("  High Latency: %.1f ms\n", float64(device.DefaultHighInputLatency)*1000)
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

func NewAudioRecorder(device int, channels int, sampleFormat portaudio.PaSampleFormat, sampleRate int, outputFile string) (*AudioRecorder, error) {
	file, err := os.Create(outputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	params := portaudio.PaStreamParameters{
		DeviceIndex:  device,
		ChannelCount: channels,
		SampleFormat: sampleFormat,
	}

	stream, err := portaudio.NewInputStream(params, float64(sampleRate))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create input stream: %w", err)
	}

	stream.UseHighLatency = false

	recorder := &AudioRecorder{
		stream:         stream,
		file:           file,
		channels:       channels,
		sampleFormat:   sampleFormat,
		sampleRate:     sampleRate,
		bytesPerSample: portaudio.GetSampleSize(sampleFormat),
		done:           make(chan struct{}),
	}

	return recorder, nil
}

func (r *AudioRecorder) Start() error {
	// Open stream with callback
	err := r.stream.OpenCallback(512, r.audioCallback)
	if err != nil {
		return fmt.Errorf("failed to open callback: %w", err)
	}

	// Start the stream
	if err := r.stream.StartStream(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}

	return nil
}

func (r *AudioRecorder) Stop() error {
	if r.stream == nil {
		return nil
	}
	var errs []error
	if err := r.stream.StopStream(); err != nil {
		errs = append(errs, fmt.Errorf("stop stream: %w", err))
	}
	if err := r.stream.CloseCallback(); err != nil {
		errs = append(errs, fmt.Errorf("close callback: %w", err))
	}
	r.stream = nil
	return errors.Join(errs...)
}

func (r *AudioRecorder) Close() error {
	r.Stop()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

func (r *AudioRecorder) GetSamplesWritten() uint64 {
	return r.samplesWritten.Load()
}

// audioCallback is called by PortAudio to receive audio data
func (r *AudioRecorder) audioCallback(
	input, output []byte,
	frameCount uint,
	timeInfo *portaudio.StreamCallbackTimeInfo,
	statusFlags portaudio.StreamCallbackFlags,
) portaudio.StreamCallbackResult {

	// Track input overflow (check from main goroutine via r.overflows)
	if statusFlags&portaudio.InputOverflow != 0 {
		r.overflows.Add(1)
	}

	// Write input data to file
	if len(input) > 0 {
		n, err := r.file.Write(input)

		if err != nil {
			return portaudio.Abort
		}

		// Track samples written
		samplesWritten := n / (r.channels * r.bytesPerSample)
		r.samplesWritten.Add(uint64(samplesWritten))
	}

	return portaudio.Continue
}
