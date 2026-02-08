# PortAudio Go Examples

This directory contains examples demonstrating various features of the go-portaudio library.

## Examples Overview

### Audio Playback

#### [play_sine](play_sine/) - Sine Wave Generator (Blocking I/O)
Generates a sine wave using blocking I/O mode. Good introduction to basic audio output.

```bash
cd play_sine
go run main.go
```

#### [play_sine_async](play_sine_async/) - Sine Wave Generator (Async)
Asynchronous sine wave generation.

```bash
cd play_sine_async
go run main.go
```

#### [play_sine_callback](play_sine_callback/) - Sine Wave Generator (Callback Mode) ⭐
Generates a sine wave using callback mode. **Recommended** - demonstrates best practices for real-time audio.

```bash
cd play_sine_callback
go run main.go
```

**Features**:
- Low-latency callback-based audio
- Zero allocations in audio callback
- Stereo output (left: 256Hz, right: 320Hz)

#### [play_raw](play_raw/) - Raw PCM Audio Player ⭐ **NEW**
Plays raw PCM audio files recorded with `record_audio`.

```bash
cd play_raw

# List available output devices
go run main.go -list

# Play a mono recording
go run main.go -in recording.raw -channels 1 -rate 44100

# Play a stereo recording on device 1
go run main.go -in audio.raw -channels 2 -rate 48000 -device 1
```

**Features**:
- Plays raw 16-bit PCM files
- Configurable sample rate and channels
- Device selection
- Progress tracking

### Audio Recording

#### [record_audio](record_audio/) - Audio Recorder ⭐ **NEW**
Records audio from an input device (microphone) to a raw PCM file.

```bash
cd record_audio

# List available input devices
go run main.go -list

# Record from default device, mono, 44.1kHz
go run main.go -out recording.raw

# Record from device 2, stereo, 48kHz
go run main.go -device 2 -channels 2 -rate 48000 -out audio.raw
```

**Features**:
- Records from microphone or line-in
- Low-latency callback-based recording
- Configurable sample rate and channels
- Device selection
- Graceful shutdown on Ctrl-C

**Complete Recording Workflow**:
```bash
# 1. List input devices
go run examples/record_audio/main.go -list

# 2. Record 10 seconds of audio
go run examples/record_audio/main.go -device 0 -channels 1 -rate 44100 -out my_recording.raw

# 3. Play it back
go run examples/play_raw/main.go -in my_recording.raw -channels 1 -rate 44100
```

### Device Management

#### [audio_enum](audio_enum/) - Device Enumeration
Lists all available audio devices with their capabilities.

```bash
cd audio_enum
go run main.go
```

Shows:
- Device name and index
- Input/output channel counts
- Supported sample rates
- Latency information
- Host API details

## Building Examples

### Build All Examples
```bash
go build ./examples/...
```

### Build Specific Example
```bash
cd examples/play_sine_callback
go build
./play_sine_callback
```

### Install to $GOPATH/bin
```bash
go install ./examples/record_audio
go install ./examples/play_raw
record_audio -out test.raw
play_raw -in test.raw -channels 1 -rate 44100
```

## Example Categories

### For Beginners
1. **[audio_enum](audio_enum/)** - Start here to see available devices
2. **[play_sine](play_sine/)** - Simple blocking I/O playback
3. **[play_sine_callback](play_sine_callback/)** - Recommended callback mode

### For Real-Time Audio
1. **[play_sine_callback](play_sine_callback/)** - Low-latency playback
2. **[record_audio](record_audio/)** - Low-latency recording
3. **[play_raw](play_raw/)** - Raw PCM playback

### For Production Use
- **[play_sine_callback](play_sine_callback/)** - Production-ready callback pattern
- **[record_audio](record_audio/)** - Production recording with proper error handling
- **[play_raw](play_raw/)** - Efficient file playback

## Common Patterns

### Callback Mode (Recommended for Real-Time)
```go
// 1. Create stream
stream, _ := portaudio.NewCallbackStream(device, 2, portaudio.SampleFmtFloat32, 44100)

// 2. Open with callback
stream.OpenCallback(512, func(input, output []byte, frameCount uint, ...) portaudio.StreamCallbackResult {
    // Generate or process audio
    return portaudio.Continue
})

// 3. Start playing
stream.StartStream()

// 4. Cleanup
stream.StopStream()
stream.CloseCallback()
```

### Blocking I/O Mode
```go
// 1. Create stream
stream, _ := portaudio.NewOutputStream(device, 2, portaudio.SampleFmtInt16, 44100)

// 2. Open
stream.Open(512)
stream.StartStream()

// 3. Write audio data
stream.Write(frames, buffer)

// 4. Cleanup
stream.StopStream()
stream.Close()
```

### Recording Audio
```go
// 1. Create input stream
params := portaudio.PaStreamParameters{
    DeviceIndex:  device,
    ChannelCount: 1,
    SampleFormat: portaudio.SampleFmtInt16,
}
stream, _ := portaudio.NewInputStream(params, 44100)

// 2. Open with callback
stream.OpenCallback(512, func(input, output []byte, ...) portaudio.StreamCallbackResult {
    // Save input data
    file.Write(input)
    return portaudio.Continue
})

// 3. Start recording
stream.StartStream()
```

## Audio Callback Best Practices

✅ **DO**:
- Process audio quickly (< 1ms)
- Use pre-allocated buffers
- Return Continue/Complete/Abort appropriately
- Use atomic operations for shared state

❌ **DON'T**:
- Allocate memory (`make()`, `new()`, `append()`)
- Block (mutex, I/O, `time.Sleep()`)
- Perform unbounded operations
- Call Go runtime functions

See [play_sine_callback](play_sine_callback/) for a complete example following best practices.

## Troubleshooting

### "No default device" error
- Run `audio_enum` to see available devices
- Specify device with `-device` flag

### Audio glitches or dropouts
- Increase buffer size (e.g., `-frames 1024`)
- Use callback mode instead of blocking I/O
- Ensure audio callback is fast (no allocations)

### "Device busy" error
- Close other audio applications
- Only one stream per device at a time

### Compilation errors
```bash
# macOS: Install PortAudio
brew install portaudio

# Linux: Install PortAudio development headers
sudo apt-get install portaudio19-dev

# Rebuild
go clean -cache
go build
```

## Platform Notes

### macOS
- Uses CoreAudio backend
- Works well with Apple Silicon (M1/M2/M3)
- Default devices usually work out of box

### Linux
- Uses ALSA backend
- May need to configure ALSA permissions
- Check `/proc/asound/cards` for available devices

### Windows
- Uses WASAPI or DirectSound backend
- May need to run as administrator for some devices

## Contributing

To add a new example:

1. Create a new directory under `examples/`
2. Add a descriptive `main.go`
3. Update this README
4. Follow existing patterns (error handling, flags, help text)
5. Include usage examples in comments

## See Also

- [PortAudio Documentation](http://www.portaudio.com/docs.html)
- [Main README](../README.md)
- [API Reference](https://pkg.go.dev/github.com/drgolem/go-portaudio/portaudio)
