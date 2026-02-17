# go-portaudio

Go bindings for [PortAudio](http://www.portaudio.com/) - cross-platform audio I/O library.

## Features

- ✅ **Callback-based audio** - Low-latency real-time audio processing
- ✅ **Blocking I/O** - Simple write-based audio output
- ✅ **Device management** - Enumerate and select audio devices
- ✅ **Multiple formats** - Int16, Int24, Int32, Float32 sample formats
- ✅ **Latency control** - High and low latency modes
- ✅ **Error handling** - Comprehensive error types including host-specific errors
- ✅ **Thread-safe init** - Reference-counted Initialize/Terminate

## Quick Start

### Installation

```bash
# macOS
brew install portaudio
go get github.com/drgolem/go-portaudio

# Linux
sudo apt-get install portaudio19-dev
go get github.com/drgolem/go-portaudio

# Windows
# Download PortAudio from http://www.portaudio.com/download.html
go get github.com/drgolem/go-portaudio
```

### Simple Callback Example

```go
package main

import (
    "github.com/drgolem/go-portaudio/portaudio"
)

func main() {
    // Initialize PortAudio
    portaudio.Initialize()
    defer portaudio.Terminate()

    // Create stream
    stream := &portaudio.PaStream{
        OutputParameters: portaudio.PaStreamParameters{
            DeviceIndex:  1,                          // Device ID
            ChannelCount: 2,                          // Stereo
            SampleFormat: portaudio.SampleFmtFloat32, // 32-bit float
        },
        SampleRate: 44100.0,
    }

    // Open with callback
    stream.OpenCallback(512, func(input, output []byte, frameCount uint,
        timeInfo *portaudio.StreamCallbackTimeInfo,
        flags portaudio.StreamCallbackFlags) portaudio.StreamCallbackResult {

        // Generate or process audio here
        // Fill 'output' buffer with audio samples

        return portaudio.Continue
    })
    defer stream.CloseCallback()

    // Start playing
    stream.StartStream()
    defer stream.StopStream()

    // ... your code here ...
}
```

## Examples

Run the examples to see the library in action:

```bash
# Enumerate audio devices
cd examples/audio_enum
go run main.go

# Play a sine wave (blocking I/O)
cd examples/play_sine
go run main.go

# Play a sine wave (callback mode) - Recommended!
cd examples/play_sine_callback
go run main.go

# NEW: Record audio from microphone
cd examples/record_audio
go run main.go -list                    # List input devices
go run main.go -out recording.raw       # Record audio

# NEW: Play back raw PCM files
cd examples/play_raw
go run main.go -in recording.raw -channels 1 -rate 44100
```

See [examples/README.md](examples/README.md) for comprehensive documentation of all examples.

## API Overview

### Stream Types

#### Callback-Based (Recommended for Real-Time Audio)
```go
stream.OpenCallback(framesPerBuffer, callbackFunc)
stream.StartStream()
// Audio callback runs in separate thread
stream.StopStream()
stream.CloseCallback()
```

**Use when**:
- Real-time audio processing
- Low latency required
- Maximum performance needed

#### Blocking I/O
```go
stream.Open(framesPerBuffer)
stream.StartStream()
stream.Write(frames, buffer)  // Write audio data
stream.StopStream()
stream.Close()
```

**Use when**:
- Simple playback scenarios
- Higher latency acceptable
- Easier programming model preferred

### Configuration

#### Latency Modes
```go
// Low latency (default for callbacks)
stream.UseHighLatency = false  // ~10-20ms latency

// High latency (recommended for blocking I/O)
stream.UseHighLatency = true   // ~50-100ms latency, fewer underruns
```

#### Stream Flags
```go
// ClipOff: only for float output guaranteed within [-1.0, 1.0].
// Avoid for integer formats — some backends convert internally to float
// and skipping clipping can cause distortion.
stream.StreamFlags = portaudio.ClipOff

stream.StreamFlags = portaudio.DitherOff  // Disable dithering
```

### Device Management

```go
// List all devices
devices, err := portaudio.Devices()

// Get default output device
device, err := portaudio.DefaultOutputDevice()

// Get device info
info, err := portaudio.GetDeviceInfo(deviceIndex)
```

### Sample Formats

```go
portaudio.SampleFmtFloat32  // 32-bit float (range: -1.0 to 1.0)
portaudio.SampleFmtInt32    // 32-bit integer
portaudio.SampleFmtInt24    // 24-bit integer (3 bytes)
portaudio.SampleFmtInt16    // 16-bit integer (most common)
portaudio.SampleFmtInt8     // 8-bit integer
portaudio.SampleFmtUInt8    // 8-bit unsigned integer
```

## Thread Safety

⚠️ **Important**: This library is NOT thread-safe. You must ensure that:

- `Initialize()` and `Terminate()` are called from a single goroutine
- Each `PaStream` instance is accessed by only one goroutine at a time
- No concurrent calls to the same stream's methods

It is safe to use multiple `PaStream` instances from different goroutines as long as each stream is accessed by only one goroutine.

### Audio Callback Constraints

Audio callbacks run in a **separate real-time thread** managed by PortAudio (not a Go goroutine). In the callback, you must:

✅ **DO**:
- Process audio quickly (< 1ms typically)
- Use pre-allocated buffers
- Return Continue/Complete/Abort appropriately

❌ **DON'T**:
- Allocate memory (`make()`, `new()`)
- Block (mutex, I/O, `time.Sleep()`)
- Perform unbounded operations
- Call Go runtime functions

**Example** (from production use):
```go
func audioCallback(input, output []byte, frameCount uint, ...) portaudio.StreamCallbackResult {
    // ✅ Copy from pre-allocated ringbuffer (fast, no allocation)
    n, _ := ringbuf.Read(output)

    // ✅ Fill silence if underrun
    if n < len(output) {
        clear(output[n:])
    }

    return portaudio.Continue
}
```

## Performance

**Benchmarks** (from production usage on Apple M2 Pro):
- Callback throughput: >200k frames/sec
- Latency: ~10-20ms (512 frame buffer @ 44.1kHz)
- CPU usage: ~2-3% for stereo 16-bit @ 44.1kHz
- Zero allocations in callback path ✅

## Documentation

- [API Reference](https://pkg.go.dev/github.com/drgolem/go-portaudio/portaudio) (godoc)
- [PortAudio Documentation](http://www.portaudio.com/docs.html) (official PortAudio docs)

## Contributing

This is a learning project exploring CGO and audio libraries. Contributions welcome!

**Current status**:
- ✅ Callback mode - production ready
- ✅ Blocking I/O - working
- ✅ Input streams - full support (recording works!)
- ✅ Duplex streams - supported
- ✅ Recording/playback examples - complete
- ✅ Unit tests - 52% coverage with 20 comprehensive tests

See [REVIEW_AND_RECOMMENDATIONS.md](REVIEW_AND_RECOMMENDATIONS.md) for detailed roadmap.

## Platform Support

- ✅ **macOS** - Tested on Apple Silicon (M2 Pro)
- ✅ **Linux** - Tested on Raspberry Pi 5

## Credits & Inspiration

This project was inspired by and learned from [gordonklaus/portaudio](https://github.com/gordonklaus/portaudio), an excellent Go binding for PortAudio. 

## Related Projects

- [PortAudio](http://www.portaudio.com/) - The underlying C library

