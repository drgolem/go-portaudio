package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/signal"

	"github.com/DrGolem/go-portaudio/portaudio"
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

	for devIdx := 0; devIdx < devCnt; devIdx++ {
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
	for idx := 0; idx < hostApiCnt; idx++ {
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

	fmt.Println("Playing.  Press Ctrl-C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	err = st.StartStream()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}

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
		case <-sig:
			return
		default:
		}
	}

	st.StopStream()
}
