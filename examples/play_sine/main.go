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

func main() {
	fmt.Println("test portaudio bindings")

	fmt.Printf("version text: %s\n", portaudio.GetVersionText())

	err := portaudio.Initialize()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}
	defer portaudio.Terminate()

	devCnt, err := portaudio.GetDeviceCount()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
	} else {
		fmt.Printf("device count: %d\n", devCnt)
	}

	for devIdx := range devCnt {
		di, err := portaudio.GetDeviceInfo(devIdx)
		if err != nil {
			fmt.Printf("ERR: %v\n", err)
		} else {
			fmt.Printf("[%d] device: %#v\n", devIdx, di)
		}
	}

	hostApiCnt, err := portaudio.GetHostApiCount()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}

	fmt.Printf("Host API Info (count: %d)\n", hostApiCnt)
	for idx := range hostApiCnt {
		hi, err := portaudio.GetHostApiInfo(idx)
		if err != nil {
			fmt.Printf("ERR: %v\n", err)
		} else {
			fmt.Printf("[%d] api info: %#v\n", idx, hi)
		}
	}

	outStreamParams := portaudio.PaStreamParameters{
		DeviceIndex:  1,
		ChannelCount: 2,
		SampleFormat: portaudio.SampleFmtFloat32,
	}
	sampleRate := float64(44100)
	err = portaudio.IsFormatSupported(nil, &outStreamParams, sampleRate)
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	} else {
		fmt.Printf("Format supported: %v, sampleRate: %f\n", outStreamParams, sampleRate)
	}

	framesPerBuffer := 4096

	st, err := portaudio.NewStream(outStreamParams, sampleRate)
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}

	// Configure for blocking I/O: use ClipOff flag and high latency
	// ClipOff is safe since sine wave output is guaranteed to be within [-1.0, 1.0]
	// High latency helps avoid underruns in blocking I/O mode
	st.StreamFlags = portaudio.ClipOff
	st.UseHighLatency = true

	err = st.Open(framesPerBuffer)
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}
	defer st.Close()

	ctx, ctxCancelFn := context.WithCancel(context.Background())
	defer ctxCancelFn()

	fmt.Printf("Press Ctrl-C to stop.\n")
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = st.StartStream()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}
	defer st.StopStream()

	dataBuffer := make([]float32, 2*framesPerBuffer)

	var phaseL float64
	var phaseR float64

	stepL := float64(256.0 / sampleRate)
	stepR := float64(320.0 / sampleRate)

	for {
		for i := 0; i < 2*framesPerBuffer; {

			dataBuffer[i] = float32(math.Sin(2 * math.Pi * phaseL))
			i++
			_, phaseL = math.Modf(phaseL + stepL)

			dataBuffer[i] = float32(math.Sin(2 * math.Pi * phaseR))
			i++
			_, phaseR = math.Modf(phaseR + stepR)
		}

		// Convert []float32 to []byte using unsafe (zero-copy, much faster)
		buf := unsafe.Slice((*byte)(unsafe.Pointer(&dataBuffer[0])), len(dataBuffer)*4)

		err = st.Write(framesPerBuffer, buf)
		if err != nil {
			panic(err)
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}

}
