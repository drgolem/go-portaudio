package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/drgolem/go-portaudio/portaudio"
)

func main() {
	deviceIdx := flag.Int("device", 1, "Audio output device index")
	channels := flag.Int("channels", 2, "Number of channels (1=mono, 2=stereo)")
	sampleRate := flag.Int("samplerate", 44100, "Sample rate in Hz")
	bitsPerSample := flag.Int("bitspersample", 16, "Bits per sample (8, 16, 24, 32)")
	bufferFrames := flag.Int("buffer", 512, "Frames per PortAudio buffer (can be 0 in callback mode, see paFramesPerBufferUnspecified)")
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
		slog.Error("failed to initialize PortAudio", "error", err)
		os.Exit(1)
	}
	defer portaudio.Terminate()

	slog.Info("PortAudio initialized", "version", portaudio.GetVersionText())

	if *listDevices {
		listOutputDevices()
		return
	}

	if *inputFile == "" {
		slog.Error("input file is required")
		flag.Usage()
		os.Exit(1)
	}

	playerMode := PlayerMode(*mode)
	if playerMode != ModeCallback && playerMode != ModeStream {
		slog.Error("invalid mode", "mode", *mode)
		os.Exit(1)
	}

	sampleFormat, err := sampleFormatFromBits(*bitsPerSample)
	if err != nil {
		slog.Error("invalid sample format", "error", err)
		os.Exit(1)
	}

	device, err := portaudio.GetDeviceInfo(*deviceIdx)
	if err != nil {
		slog.Error("failed to get device", "device", *deviceIdx, "error", err)
		os.Exit(1)
	}
	slog.Info("using output device", "index", *deviceIdx, "name", device.Name)

	file, err := os.Open(*inputFile)
	if err != nil {
		slog.Error("failed to open input file", "error", err)
		os.Exit(1)
	}

	player, err := NewRawAudioPlayer(PlayerConfig{
		Device:       *deviceIdx,
		Channels:     *channels,
		SampleFormat: sampleFormat,
		SampleRate:   *sampleRate,
		FramesPerBuf: *bufferFrames,
		RingMs:       *ringMs,
		Mode:         playerMode,
	}, file)
	if err != nil {
		file.Close()
		slog.Error("failed to create player", "error", err)
		os.Exit(1)
	}
	defer player.Close()

	if fileInfo, err := os.Stat(*inputFile); err == nil {
		fileSize := fileInfo.Size()
		bytesPerSample := *bitsPerSample / 8
		samples := fileSize / int64(*channels*bytesPerSample)
		durationSec := float64(samples) / float64(*sampleRate)
		slog.Info("file info", "bytes", fileSize, "duration_sec", durationSec)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("starting playback",
		"file", *inputFile,
		"input", fmt.Sprintf("%d:%d:%d", *sampleRate, *channels, *bitsPerSample),
		"output", fmt.Sprintf("%.0f:%d:%d", device.DefaultSampleRate, *channels, *bitsPerSample),
		"device", device.Name,
		"buffer_frames", *bufferFrames,
		"ring_ms", *ringMs,
		"mode", *mode,
	)
	slog.Info("press Ctrl-C to stop playback")

	if err := player.Start(ctx); err != nil {
		slog.Error("failed to start playback", "error", err)
		os.Exit(1)
	}

	// Monitor goroutine: print status every 2 seconds
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if player.Done() {
					return
				}
				d := player.GetDiagnostics()
				decoded := d.BytesFromFile / uint64(d.Channels*d.BytesPerSample)
				played := player.GetSamplesPlayed()
				fmt.Printf("\r  decoded: %d  played: %d  %s  [%d:%d:%d]",
					decoded, played,
					formatTime(played, uint64(d.SampleRate)),
					d.SampleRate, d.Channels, d.BytesPerSample*8)
			}
		}
	}()

	// Wait for playback to finish or interrupt
	for !player.Done() {
		select {
		case <-ctx.Done():
			slog.Info("playback interrupted")
			goto done
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	slog.Info("playback complete")

done:
	<-monitorDone
	if err := player.Stop(); err != nil {
		slog.Warn("error stopping player", "error", err)
	}

	samples := player.GetSamplesPlayed()
	durationSec := float64(samples) / float64(*sampleRate)
	slog.Info("playback finished", "samples", samples, "duration_sec", durationSec)

	player.PrintDiagnostics(os.Stdout)
}

func listOutputDevices() {
	devices, err := portaudio.Devices()
	if err != nil {
		slog.Error("failed to get devices", "error", err)
		os.Exit(1)
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

func formatTime(samples, sampleRate uint64) string {
	if sampleRate == 0 {
		return "00:00:00.000"
	}
	totalMs := samples * 1000 / sampleRate
	h := totalMs / 3600000
	m := (totalMs % 3600000) / 60000
	s := (totalMs % 60000) / 1000
	ms := totalMs % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

func sampleFormatFromBits(bits int) (SampleFormat, error) {
	switch bits {
	case 8:
		return SampleFmtInt8, nil
	case 16:
		return SampleFmtInt16, nil
	case 24:
		return SampleFmtInt24, nil
	case 32:
		return SampleFmtInt32, nil
	default:
		return 0, fmt.Errorf("unsupported bits per sample: %d (use 8, 16, 24, or 32)", bits)
	}
}
