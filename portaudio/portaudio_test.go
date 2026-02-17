package portaudio

import (
	"testing"
)

// TestInitializeTerminate tests basic library initialization and termination
func TestInitializeTerminate(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	err = Terminate()
	if err != nil {
		t.Errorf("Terminate failed: %v", err)
	}
}

// TestMultipleInitialize tests reference counting behavior
func TestMultipleInitialize(t *testing.T) {
	// Initialize twice
	err := Initialize()
	if err != nil {
		t.Fatalf("First Initialize failed: %v", err)
	}

	err = Initialize()
	if err != nil {
		t.Fatalf("Second Initialize failed: %v", err)
	}

	// Should require two terminates
	err = Terminate()
	if err != nil {
		t.Errorf("First Terminate failed: %v", err)
	}

	err = Terminate()
	if err != nil {
		t.Errorf("Second Terminate failed: %v", err)
	}
}

// TestGetVersion tests version information retrieval
func TestGetVersion(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	version := GetVersion()
	if version == 0 {
		t.Error("GetVersion returned 0")
	}

	versionText := GetVersionText()
	if versionText == "" {
		t.Error("GetVersionText returned empty string")
	}

	t.Logf("PortAudio Version: %d (%s)", version, versionText)
}

// TestGetErrorText tests error message retrieval
func TestGetErrorText(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		wantText string
	}{
		{"NoError", 0, "Success"},
		{"InvalidDevice", -9996, "Invalid device"},
		{"InvalidSampleRate", -9997, "Invalid sample rate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := GetErrorText(tt.code)
			if text == "" {
				t.Error("GetErrorText returned empty string")
			}
			t.Logf("Error code %d: %s", tt.code, text)
		})
	}
}

// TestGetSampleSize tests sample format size calculations
func TestGetSampleSize(t *testing.T) {
	tests := []struct {
		name     string
		format   PaSampleFormat
		expected int
	}{
		{"Float32", SampleFmtFloat32, 4},
		{"Int32", SampleFmtInt32, 4},
		{"Int24", SampleFmtInt24, 3},
		{"Int16", SampleFmtInt16, 2},
		{"Int8", SampleFmtInt8, 1},
		{"UInt8", SampleFmtUInt8, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size := GetSampleSize(tt.format)
			if size != tt.expected {
				t.Errorf("GetSampleSize(%v) = %d, want %d", tt.format, size, tt.expected)
			}
		})
	}
}

// TestGetDeviceCount tests device enumeration
func TestGetDeviceCount(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	count, err := GetDeviceCount()
	if err != nil {
		t.Fatalf("GetDeviceCount failed: %v", err)
	}

	if count < 0 {
		t.Error("GetDeviceCount returned negative count")
	}

	t.Logf("Found %d audio devices", count)
}

// TestGetDeviceInfo tests device information retrieval
func TestGetDeviceInfo(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	count, err := GetDeviceCount()
	if err != nil {
		t.Fatalf("GetDeviceCount failed: %v", err)
	}

	if count == 0 {
		t.Skip("No audio devices available")
	}

	// Test first device
	info, err := GetDeviceInfo(0)
	if err != nil {
		t.Errorf("GetDeviceInfo(0) failed: %v", err)
	} else {
		if info.Name == "" {
			t.Error("Device name is empty")
		}
		if info.MaxInputChannels < 0 || info.MaxOutputChannels < 0 {
			t.Error("Invalid channel counts")
		}
		t.Logf("Device 0: %s (In: %d, Out: %d)", info.Name, info.MaxInputChannels, info.MaxOutputChannels)
	}

	// Test invalid device index
	_, err = GetDeviceInfo(-1)
	if err == nil {
		t.Error("GetDeviceInfo(-1) should fail")
	}
}

// TestDefaultOutputDevice tests default output device retrieval
func TestDefaultOutputDevice(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	device, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	if device.MaxOutputChannels <= 0 {
		t.Error("Default output device has no output channels")
	}

	t.Logf("Default output device: %s (%d channels)", device.Name, device.MaxOutputChannels)
}

// TestDefaultInputDevice tests default input device retrieval
func TestDefaultInputDevice(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	device, err := DefaultInputDevice()
	if err != nil {
		t.Skip("No default input device available")
	}

	if device.MaxInputChannels <= 0 {
		t.Error("Default input device has no input channels")
	}

	t.Logf("Default input device: %s (%d channels)", device.Name, device.MaxInputChannels)
}

// TestGetHostApiCount tests host API enumeration
func TestGetHostApiCount(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	count, err := GetHostApiCount()
	if err != nil {
		t.Fatalf("GetHostApiCount failed: %v", err)
	}

	if count <= 0 {
		t.Error("No host APIs available")
	}

	t.Logf("Found %d host APIs", count)
}

// TestGetHostApiInfo tests host API information retrieval
func TestGetHostApiInfo(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	count, err := GetHostApiCount()
	if err != nil {
		t.Fatalf("GetHostApiCount failed: %v", err)
	}

	if count == 0 {
		t.Skip("No host APIs available")
	}

	// Test first host API
	info, err := GetHostApiInfo(0)
	if err != nil {
		t.Errorf("GetHostApiInfo(0) failed: %v", err)
	} else {
		if info.Name == "" {
			t.Error("Host API name is empty")
		}
		if info.DeviceCount < 0 {
			t.Error("Invalid device count")
		}
		t.Logf("Host API 0: %s (%d devices)", info.Name, info.DeviceCount)
	}
}

// TestDevices tests the Devices convenience function
func TestDevices(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	devices, err := Devices()
	if err != nil {
		t.Fatalf("Devices failed: %v", err)
	}

	if len(devices) == 0 {
		t.Skip("No audio devices available")
	}

	for i, device := range devices {
		if device.Name == "" {
			t.Errorf("Device %d has empty name", i)
		}
		t.Logf("Device %d: %s", i, device.Name)
	}
}

// TestNewOutputStream tests output stream creation helper
func TestNewOutputStream(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	// Create output stream
	stream, err := NewOutputStream(outputDevice.Index, 2, SampleFmtInt16, 44100)
	if err != nil {
		t.Fatalf("NewOutputStream failed: %v", err)
	}

	if stream == nil {
		t.Fatal("NewOutputStream returned nil stream")
	}

	if stream.OutputParameters == nil {
		t.Error("OutputParameters is nil")
	}

	if stream.OutputParameters.ChannelCount != 2 {
		t.Errorf("Expected 2 channels, got %d", stream.OutputParameters.ChannelCount)
	}

	if stream.SampleRate != 44100 {
		t.Errorf("Expected sample rate 44100, got %f", stream.SampleRate)
	}

	if !stream.UseHighLatency {
		t.Error("Expected UseHighLatency=true for output stream")
	}

	t.Logf("Created output stream: %d channels, %.0f Hz (device %s)", stream.OutputParameters.ChannelCount, stream.SampleRate, outputDevice.Name)
}

// TestNewCallbackStream tests callback stream creation helper
func TestNewCallbackStream(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	// Create callback stream
	stream, err := NewCallbackStream(outputDevice.Index, 2, SampleFmtFloat32, 44100)
	if err != nil {
		t.Fatalf("NewCallbackStream failed: %v", err)
	}

	if stream == nil {
		t.Fatal("NewCallbackStream returned nil stream")
	}

	if stream.OutputParameters == nil {
		t.Error("OutputParameters is nil")
	}

	if stream.OutputParameters.ChannelCount != 2 {
		t.Errorf("Expected 2 channels, got %d", stream.OutputParameters.ChannelCount)
	}

	if stream.UseHighLatency {
		t.Error("Expected UseHighLatency=false for callback stream")
	}

	if stream.StreamFlags != NoFlag {
		t.Errorf("Expected StreamFlags=NoFlag for callback stream, got %d", stream.StreamFlags)
	}

	t.Logf("Created callback stream: %d channels, %.0f Hz (device %s)", stream.OutputParameters.ChannelCount, stream.SampleRate, outputDevice.Name)
}

// TestNewInputStream tests input stream creation helper
func TestNewInputStream(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default input device
	inputDevice, err := DefaultInputDevice()
	if err != nil {
		t.Skip("No default input device available")
	}

	// Create input stream parameters
	params := PaStreamParameters{
		DeviceIndex:  inputDevice.Index,
		ChannelCount: 1,
		SampleFormat: SampleFmtInt16,
	}

	// Create input stream
	stream, err := NewInputStream(params, 44100)
	if err != nil {
		t.Fatalf("NewInputStream failed: %v", err)
	}

	if stream == nil {
		t.Fatal("NewInputStream returned nil stream")
	}

	if stream.InputParameters == nil {
		t.Error("InputParameters is nil")
	}

	if stream.InputParameters.ChannelCount != 1 {
		t.Errorf("Expected 1 channel, got %d", stream.InputParameters.ChannelCount)
	}

	if stream.SampleRate != 44100 {
		t.Errorf("Expected sample rate 44100, got %f", stream.SampleRate)
	}

	t.Logf("Created input stream: %d channels, %.0f Hz (device %s)", stream.InputParameters.ChannelCount, stream.SampleRate, inputDevice.Name)
}

// TestIsFormatSupported tests format validation
func TestIsFormatSupported(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	tests := []struct {
		name       string
		channels   int
		format     PaSampleFormat
		sampleRate float64
		shouldPass bool
	}{
		{"Stereo 16-bit 44.1kHz", 2, SampleFmtInt16, 44100, true},
		{"Stereo 16-bit 48kHz", 2, SampleFmtInt16, 48000, true},
		{"Mono 16-bit 44.1kHz", 1, SampleFmtInt16, 44100, true},
		{"Invalid sample rate", 2, SampleFmtInt16, 1, false},
		{"Too many channels", 999, SampleFmtInt16, 44100, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := &PaStreamParameters{
				DeviceIndex:  outputDevice.Index,
				ChannelCount: tt.channels,
				SampleFormat: tt.format,
			}

			err := IsFormatSupported(nil, params, tt.sampleRate)
			if tt.shouldPass && err != nil {
				t.Errorf("Expected format to be supported, got error: %v", err)
			}
			if !tt.shouldPass && err == nil {
				t.Error("Expected format to be unsupported, but got no error")
			}
		})
	}

	t.Logf("Format validation tests completed for device %s", outputDevice.Name)
}

// TestStreamLifecycle tests basic stream open/close (without starting)
func TestStreamLifecycle(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	// Create stream
	stream, err := NewOutputStream(outputDevice.Index, 2, SampleFmtInt16, 44100)
	if err != nil {
		t.Fatalf("NewOutputStream failed: %v", err)
	}

	// Open stream
	err = stream.Open(512)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if !stream.isOpen {
		t.Error("Stream should be open")
	}

	// Close stream
	err = stream.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	if stream.isOpen {
		t.Error("Stream should be closed")
	}

	t.Logf("Stream lifecycle test passed for device %s", outputDevice.Name)
}

// TestCallbackStreamLifecycle tests callback stream open/close
func TestCallbackStreamLifecycle(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	// Create callback stream
	stream, err := NewCallbackStream(outputDevice.Index, 2, SampleFmtFloat32, 44100)
	if err != nil {
		t.Fatalf("NewCallbackStream failed: %v", err)
	}

	// Simple callback that fills with silence
	callback := func(input, output []byte, frameCount uint,
		timeInfo *StreamCallbackTimeInfo,
		flags StreamCallbackFlags) StreamCallbackResult {
		// Fill with silence
		for i := range output {
			output[i] = 0
		}
		return Continue
	}

	// Open with callback
	err = stream.OpenCallback(512, callback)
	if err != nil {
		t.Fatalf("OpenCallback failed: %v", err)
	}

	if !stream.isOpen {
		t.Error("Stream should be open")
	}

	// Close callback stream
	err = stream.CloseCallback()
	if err != nil {
		t.Errorf("CloseCallback failed: %v", err)
	}

	if stream.isOpen {
		t.Error("Stream should be closed")
	}

	t.Logf("Callback stream lifecycle test passed for device %s", outputDevice.Name)
}

// TestGetWriteAvailable tests write availability check
func TestGetWriteAvailable(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	// Create and open stream
	stream, err := NewOutputStream(outputDevice.Index, 2, SampleFmtInt16, 44100)
	if err != nil {
		t.Fatalf("NewOutputStream failed: %v", err)
	}

	err = stream.Open(512)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer stream.Close()

	// Start stream to enable writing
	err = stream.StartStream()
	if err != nil {
		t.Fatalf("StartStream failed: %v", err)
	}
	defer func() { _ = stream.StopStream() }()

	// Check write availability
	available, err := stream.GetWriteAvailable()
	if err != nil {
		t.Errorf("GetWriteAvailable failed: %v", err)
	}

	// Should have some space available in a freshly started stream
	t.Logf("Write available: %d frames (device %s)", available, outputDevice.Name)
}

// TestInvalidOperations tests error handling for invalid operations
func TestInvalidOperations(t *testing.T) {
	err := Initialize()
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = Terminate() }()

	// Get default output device
	outputDevice, err := DefaultOutputDevice()
	if err != nil {
		t.Skip("No default output device available")
	}

	stream, err := NewOutputStream(outputDevice.Index, 2, SampleFmtInt16, 44100)
	if err != nil {
		t.Fatalf("NewOutputStream failed: %v", err)
	}

	// Try to start without opening
	err = stream.StartStream()
	if err == nil {
		t.Error("StartStream should fail on unopened stream")
	}

	// Note: Close() on unopened stream is allowed (idempotent)

	// Open stream
	err = stream.Open(512)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Try to open again
	err = stream.Open(512)
	if err == nil {
		t.Error("Open should fail on already open stream")
	}

	// Clean up
	stream.Close()

	t.Logf("Invalid operations test passed for device %s", outputDevice.Name)
}
