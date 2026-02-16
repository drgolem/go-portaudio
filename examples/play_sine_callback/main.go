package main

import (
	"context"
	"fmt"
	"math"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/drgolem/go-portaudio/portaudio"
)

// Sine wave generator state
type sineGenerator struct {
	phaseL   float64
	phaseR   float64
	stepL    float64
	stepR    float64
	channels int
	// samples is pre-allocated to avoid allocation in the real-time callback
	samples []float32
}

func newSineGenerator(freqL, freqR float64, sampleRate float64, channels int, framesPerBuffer int) *sineGenerator {
	return &sineGenerator{
		stepL:    freqL / sampleRate,
		stepR:    freqR / sampleRate,
		channels: channels,
		samples:  make([]float32, framesPerBuffer*channels),
	}
}

// Callback function that generates sine wave audio
func (sg *sineGenerator) audioCallback(input, output []byte, frameCount uint, timeInfo *portaudio.StreamCallbackTimeInfo, statusFlags portaudio.StreamCallbackFlags) portaudio.StreamCallbackResult {
	sampleCount := int(frameCount) * sg.channels
	samples := sg.samples[:sampleCount]

	// Generate stereo sine wave samples
	for i := 0; i < sampleCount; {
		// Left channel
		samples[i] = float32(math.Sin(2 * math.Pi * sg.phaseL))
		i++
		_, sg.phaseL = math.Modf(sg.phaseL + sg.stepL)

		// Right channel (if stereo)
		if sg.channels > 1 {
			samples[i] = float32(math.Sin(2 * math.Pi * sg.phaseR))
			i++
			_, sg.phaseR = math.Modf(sg.phaseR + sg.stepR)
		}
	}

	// Convert samples to bytes using unsafe (zero-copy)
	samplesBytes := unsafe.Slice((*byte)(unsafe.Pointer(&samples[0])), len(samples)*4)
	copy(output, samplesBytes)

	return portaudio.Continue
}

func main() {
	fmt.Println("PortAudio Callback Example - Sine Wave Generator")

	fmt.Printf("version: %s\n", portaudio.GetVersionText())

	err := portaudio.Initialize()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}
	defer portaudio.Terminate()

	// Get device count
	devCnt, err := portaudio.GetDeviceCount()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
	} else {
		fmt.Printf("device count: %d\n", devCnt)
	}

	// Configure output stream parameters
	outStreamParams := portaudio.PaStreamParameters{
		DeviceIndex:  1,
		ChannelCount: 2,
		SampleFormat: portaudio.SampleFmtFloat32,
	}
	sampleRate := float64(44100)

	// Verify format is supported
	err = portaudio.IsFormatSupported(nil, &outStreamParams, sampleRate)
	if err != nil {
		fmt.Printf("ERR: format not supported: %v\n", err)
		return
	}
	fmt.Printf("Format supported: %v, sampleRate: %f\n", outStreamParams, sampleRate)

	framesPerBuffer := 512 // Smaller buffer for lower latency in callback mode

	// Create stream
	st, err := portaudio.NewStream(outStreamParams, sampleRate)
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}

	// Configure for callback mode: use low latency (default)
	// ClipOff is safe since sine wave output is guaranteed to be within [-1.0, 1.0]
	st.StreamFlags = portaudio.ClipOff
	st.UseHighLatency = false // Low latency for callback mode

	// Create sine wave generator
	// Left channel: 256 Hz, Right channel: 320 Hz
	generator := newSineGenerator(256.0, 320.0, sampleRate, outStreamParams.ChannelCount, framesPerBuffer)

	// Open stream with callback
	err = st.OpenCallback(framesPerBuffer, generator.audioCallback)
	if err != nil {
		fmt.Printf("ERR: failed to open stream: %v\n", err)
		return
	}
	defer st.CloseCallback()

	fmt.Println("Starting audio stream...")

	// Start the stream (callback will begin being called)
	err = st.StartStream()
	if err != nil {
		fmt.Printf("ERR: failed to start stream: %v\n", err)
		return
	}
	defer st.StopStream()

	fmt.Printf("Playing sine waves (Left: 256Hz, Right: 320Hz)\n")
	fmt.Printf("Press Ctrl-C to stop.\n")

	// Wait for interrupt signal
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	fmt.Println("\nStopping...")
}
