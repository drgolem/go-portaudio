package main

import (
	"context"
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
	samplesPlayed  atomic.Uint64
	underflows     atomic.Uint64
	eof            atomic.Bool // file fully read by producer
	drained        atomic.Bool // ring buffer drained after EOF
}

const (
	framesPerBuffer = 512
	// Ring buffer holds ~500ms of audio at 44.1kHz stereo 16-bit.
	// This gives the producer goroutine ample headroom.
	ringBufferSize = 64 * 1024
)

func main() {
	deviceIdx := flag.Int("device", 1, "Audio output device index")
	channels := flag.Int("channels", 1, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("rate", 44100, "Sample rate in Hz")
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
		fmt.Fprintln(os.Stderr, "  play_raw_ringbuffer -in recording.raw -channels 1 -rate 44100")
		fmt.Fprintln(os.Stderr, "  play_raw_ringbuffer -in audio.raw -channels 2 -rate 48000 -device 1")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Note: Input must be raw PCM (16-bit signed integer, little-endian).")
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

	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		log.Fatalf("Failed to get device %d: %v", *deviceIdx, err)
	}
	fmt.Printf("Using output device %d: %s\n", *deviceIdx, device.Name)

	player, err := NewRawAudioPlayer(*deviceIdx, *channels, *sampleRate, *inputFile)
	if err != nil {
		log.Fatal("Failed to create player:", err)
	}
	defer player.Close()

	if fileInfo, err := os.Stat(*inputFile); err == nil {
		fileSize := fileInfo.Size()
		samples := fileSize / int64(*channels*2)
		durationSec := float64(samples) / float64(*sampleRate)
		fmt.Printf("File size: %d bytes (%.2f seconds)\n", fileSize, durationSec)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Playing %s...\n", *inputFile)
	fmt.Printf("Configuration: %d channel(s), %d Hz, 16-bit PCM\n", *channels, *sampleRate)
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
	if u := player.underflows.Load(); u > 0 {
		fmt.Printf("Warning: %d output underflow(s) detected (audio glitches)\n", u)
	}
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
	file, err := os.Open(inputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}

	stream, err := portaudio.NewCallbackStream(device, channels, portaudio.SampleFmtInt16, float64(sampleRate))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to create callback stream: %w", err)
	}

	player := &RawAudioPlayer{
		stream:         stream,
		file:           file,
		ring:           ringbuffer.New(ringBufferSize),
		channels:       channels,
		sampleRate:     sampleRate,
		bytesPerSample: 2,
	}

	return player, nil
}

// Start begins the producer goroutine and opens the audio stream.
func (p *RawAudioPlayer) Start(ctx context.Context) error {
	// Start producer goroutine: reads file → ring buffer
	go p.producer(ctx)

	// Open and start audio stream
	if err := p.stream.OpenCallback(framesPerBuffer, p.audioCallback); err != nil {
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
			p.ring.Write(buf[:n])
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

	if statusFlags&portaudio.OutputUnderflow != 0 {
		p.underflows.Add(1)
	}

	bytesNeeded := int(frameCount) * p.channels * p.bytesPerSample

	// TryRead is non-blocking: returns immediately even if the internal
	// lock is held by the producer goroutine.
	n, _ := p.ring.TryRead(output[:bytesNeeded])

	if n < bytesNeeded {
		// Fill remainder with silence
		clear(output[n:bytesNeeded])

		// If the producer has finished reading the file and the ring
		// buffer is now empty, playback is done.
		if p.eof.Load() && p.ring.Length() == 0 {
			p.drained.Store(true)
			p.samplesPlayed.Add(uint64(n / (p.channels * p.bytesPerSample)))
			return portaudio.Complete
		}

		// Otherwise it's just a temporary underflow — the producer
		// hasn't filled the buffer fast enough.
		if n == 0 {
			p.underflows.Add(1)
		}
	}

	p.samplesPlayed.Add(uint64(n / (p.channels * p.bytesPerSample)))
	return portaudio.Continue
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

func (p *RawAudioPlayer) IsDrained() bool {
	return p.drained.Load()
}
