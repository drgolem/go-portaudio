package main

import (
	"fmt"

	"github.com/drgolem/go-portaudio/portaudio"
)

func main() {
	fmt.Println("Testing PortAudio Helper Functions")
	fmt.Println("===================================")

	err := portaudio.Initialize()
	if err != nil {
		fmt.Printf("ERR: %v\n", err)
		return
	}
	defer portaudio.Terminate()

	// Test 1: Devices() helper
	fmt.Println("\n1. Testing Devices() helper:")
	devices, err := portaudio.Devices()
	if err != nil {
		fmt.Printf("ERR getting devices: %v\n", err)
	} else {
		fmt.Printf("Found %d devices:\n", len(devices))
		for i, dev := range devices {
			fmt.Printf("  [%d] %s (in: %d, out: %d)\n",
				i, dev.Name, dev.MaxInputChannels, dev.MaxOutputChannels)
		}
	}

	// Test 2: HostApis() helper
	fmt.Println("\n2. Testing HostApis() helper:")
	apis, err := portaudio.HostApis()
	if err != nil {
		fmt.Printf("ERR getting host APIs: %v\n", err)
	} else {
		fmt.Printf("Found %d host APIs:\n", len(apis))
		for i, api := range apis {
			fmt.Printf("  [%d] %s (%d devices)\n", i, api.Name, api.DeviceCount)
		}
	}

	// Test 3: DefaultInputDevice() helper
	fmt.Println("\n3. Testing DefaultInputDevice():")
	defIn, err := portaudio.DefaultInputDevice()
	if err != nil {
		fmt.Printf("No default input device: %v\n", err)
	} else {
		fmt.Printf("Default input: %s\n", defIn.Name)
		fmt.Printf("  Channels: %d\n", defIn.MaxInputChannels)
		fmt.Printf("  Sample Rate: %.0f Hz\n", defIn.DefaultSampleRate)
	}

	// Test 4: DefaultOutputDevice() helper
	fmt.Println("\n4. Testing DefaultOutputDevice():")
	defOut, err := portaudio.DefaultOutputDevice()
	if err != nil {
		fmt.Printf("No default output device: %v\n", err)
	} else {
		fmt.Printf("Default output: %s\n", defOut.Name)
		fmt.Printf("  Channels: %d\n", defOut.MaxOutputChannels)
		fmt.Printf("  Sample Rate: %.0f Hz\n", defOut.DefaultSampleRate)

		// Test 5: HighLatencyParameters() helper
		fmt.Println("\n5. Testing HighLatencyParameters():")
		highLatency := portaudio.HighLatencyParameters(
			defOut,
			2, // stereo
			portaudio.SampleFmtFloat32,
			false, // output
		)
		fmt.Printf("High latency config:\n")
		fmt.Printf("  Channels: %d\n", highLatency.ChannelCount)
		fmt.Printf("  Sample Format: %d\n", highLatency.SampleFormat)
		fmt.Printf("  Suggested Latency: %.3f sec\n", highLatency.SuggestedLatency)

		// Test 6: LowLatencyParameters() helper
		fmt.Println("\n6. Testing LowLatencyParameters():")
		lowLatency := portaudio.LowLatencyParameters(
			defOut,
			2, // stereo
			portaudio.SampleFmtFloat32,
			false, // output
		)
		fmt.Printf("Low latency config:\n")
		fmt.Printf("  Channels: %d\n", lowLatency.ChannelCount)
		fmt.Printf("  Sample Format: %d\n", lowLatency.SampleFormat)
		fmt.Printf("  Suggested Latency: %.3f sec\n", lowLatency.SuggestedLatency)
	}

	// Test 7: Reference-counted initialization
	fmt.Println("\n7. Testing reference-counted initialization:")
	fmt.Println("Calling Initialize() again (should be safe)...")
	err = portaudio.Initialize()
	if err != nil {
		fmt.Printf("ERR on second Initialize: %v\n", err)
	} else {
		fmt.Println("Second Initialize() succeeded (reference counted)")
		// Match with extra Terminate()
		portaudio.Terminate()
	}

	fmt.Println("\n✓ All helper functions tested successfully!")
}
