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

// RawAudioPlayer plays raw PCM audio files using PortAudio's blocking
// Write API — no callbacks, no ring buffers.
//
// Architecture:
//
//	Writer goroutine
//	┌──────────────────────────────────┐
//	│ file.Read → stream.Write (blocks) │
//	└──────────────────────────────────┘
//
// Pa_WriteStream blocks until PortAudio has consumed the buffer,
// so no intermediate ring buffer is needed. This is the simplest
// approach but has higher latency than callback-based examples.
type RawAudioPlayer struct {
	stream         *portaudio.PaStream
	file           *os.File
	channels       int
	sampleRate     int
	bytesPerSample int
	framesPerBuf   int
	samplesPlayed  atomic.Uint64
	eof            atomic.Bool // file fully read and written to stream

	// Data integrity counters
	bytesFromFile atomic.Uint64 // total bytes returned by io.ReadFull
	bytesToStream atomic.Uint64 // total bytes written via stream.Write

	// Diagnostics
	writeCount       atomic.Uint64
	maxWriteNs       atomic.Int64
	totalWriteNs     atomic.Int64
	writeErrors      atomic.Uint64
}

func main() {
	deviceIdx := flag.Int("device", 1, "Audio output device index")
	channels := flag.Int("channels", 2, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("samplerate", 44100, "Sample rate in Hz")
	bitsPerSample := flag.Int("bitspersample", 16, "Bits per sample (8, 16, 24, 32)")
	bufferFrames := flag.Int("buffer", 512, "Frames per PortAudio buffer")
	inputFile := flag.String("in", "", "Input raw audio file (required)")
	listDevices := flag.Bool("list", false, "List available output devices and exit")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: play_raw_stream [options]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Plays a raw PCM audio file using PortAudio's blocking Write API.")
		fmt.Fprintln(os.Stderr, "No callbacks or ring buffers — just file.Read → stream.Write in a loop.")
		fmt.Fprintln(os.Stderr, "Simpler but higher latency than callback-based examples.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  play_raw_stream -list")
		fmt.Fprintln(os.Stderr, "  play_raw_stream -in recording.raw -channels 1 -samplerate 44100")
		fmt.Fprintln(os.Stderr, "  play_raw_stream -in audio.raw -channels 2 -bitspersample 24 -samplerate 48000 -device 1")
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

	player, err := NewRawAudioPlayer(*deviceIdx, *channels, sampleFormat, *sampleRate, *bufferFrames, *inputFile)
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
	fmt.Printf("Configuration: %d channel(s), %d Hz, %d-bit PCM, buffer %d frames (blocking I/O)\n",
		*channels, *sampleRate, *bitsPerSample, *bufferFrames)
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

func NewRawAudioPlayer(device int, channels int, sampleFormat portaudio.PaSampleFormat, sampleRate int, framesPerBuffer int, inputFile string) (*RawAudioPlayer, error) {
	file, err := os.Open(inputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}

	stream, err := portaudio.NewOutputStream(device, channels, sampleFormat, float64(sampleRate))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create output stream: %w", err)
	}

	bytesPerSample := portaudio.GetSampleSize(sampleFormat)

	player := &RawAudioPlayer{
		stream:         stream,
		file:           file,
		channels:       channels,
		sampleRate:     sampleRate,
		bytesPerSample: bytesPerSample,
		framesPerBuf:   framesPerBuffer,
	}

	return player, nil
}

// Start opens the stream and begins the writer goroutine.
func (p *RawAudioPlayer) Start(ctx context.Context) error {
	if err := p.stream.Open(p.framesPerBuf); err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	if err := p.stream.StartStream(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}

	go p.writer(ctx)
	return nil
}

// writer reads audio data from the file and writes it to the stream.
// Pa_WriteStream blocks until the data has been consumed, so this
// naturally paces itself without needing a ring buffer or sleep.
func (p *RawAudioPlayer) writer(ctx context.Context) {
	frameBytes := p.framesPerBuf * p.channels * p.bytesPerSample
	buf := make([]byte, frameBytes)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := io.ReadFull(p.file, buf)
		if n > 0 {
			p.bytesFromFile.Add(uint64(n))

			frames := n / (p.channels * p.bytesPerSample)
			if frames > 0 {
				startNano := time.Now().UnixNano()
				werr := p.stream.Write(frames, buf[:frames*p.channels*p.bytesPerSample])
				duration := time.Now().UnixNano() - startNano

				p.totalWriteNs.Add(duration)
				if duration > p.maxWriteNs.Load() {
					p.maxWriteNs.Store(duration)
				}
				p.writeCount.Add(1)

				if werr != nil {
					p.writeErrors.Add(1)
				} else {
					written := frames * p.channels * p.bytesPerSample
					p.bytesToStream.Add(uint64(written))
					p.samplesPlayed.Add(uint64(frames))
				}
			}
		}

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			p.eof.Store(true)
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *RawAudioPlayer) Stop() error {
	if p.stream == nil {
		return nil
	}
	var errs []error
	if err := p.stream.StopStream(); err != nil {
		errs = append(errs, fmt.Errorf("stop stream: %w", err))
	}
	if err := p.stream.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close stream: %w", err))
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
	return p.eof.Load()
}

func (p *RawAudioPlayer) PrintDiagnostics() {
	count := p.writeCount.Load()
	if count == 0 {
		return
	}

	expectedMs := float64(p.framesPerBuf) / float64(p.sampleRate) * 1000
	avgWriteMs := float64(p.totalWriteNs.Load()) / float64(count) / 1e6
	maxWriteMs := float64(p.maxWriteNs.Load()) / 1e6

	fmt.Printf("\nDiagnostics (%d writes, %d frames/buffer, blocking I/O):\n", count, p.framesPerBuf)
	fmt.Printf("  Write duration:     avg %.2f ms,  max %.2f ms  (expected ~%.2f ms)\n",
		avgWriteMs, maxWriteMs, expectedMs)
	if e := p.writeErrors.Load(); e > 0 {
		fmt.Printf("  Write errors:       %d\n", e)
	}

	// Data integrity report
	fromFile := p.bytesFromFile.Load()
	toStream := p.bytesToStream.Load()

	fmt.Printf("\n  Data integrity:\n")
	fmt.Printf("    File → Stream:  %d read, %d written", fromFile, toStream)
	if fromFile == toStream {
		fmt.Printf("  ✓\n")
	} else {
		tailBytes := fromFile - toStream
		fmt.Printf("  (tail %d bytes — partial frame at EOF)\n", tailBytes)
	}
}
