package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os/signal"
	"syscall"

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
		//SampleFormat: portaudio.SampleFmtInt24,
	}
	sampleRate := float32(44100)
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

		buf := new(bytes.Buffer)
		for _, d := range dataBuffer {
			err := binary.Write(buf, binary.LittleEndian, d)
			if err != nil {
				fmt.Println("binary.Write failed:", err)
				panic(err)
			}
		}

		err = st.Write(framesPerBuffer, buf.Bytes())
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
