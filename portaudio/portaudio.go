// Package portaudio provides Go bindings for PortAudio - a cross-platform audio I/O library.
//
// PortAudio provides a simple, portable API for audio input and output on Windows, macOS,
// and Linux. This package wraps the C PortAudio library with idiomatic Go interfaces.
//
// # Quick Start
//
// For the simplest way to play audio, use OpenDefaultStream:
//
//	portaudio.Initialize()
//	defer portaudio.Terminate()
//
//	stream, _ := portaudio.OpenDefaultStream(2, portaudio.SampleFmtFloat32, 44100, 512,
//	    func(input, output []byte, frameCount uint, ...) portaudio.StreamCallbackResult {
//	        // Generate or process audio here
//	        return portaudio.Continue
//	    })
//	defer stream.CloseCallback()
//	stream.StartStream()
//
// For more control, use NewCallbackStream or NewOutputStream:
//
//	stream, _ := portaudio.NewCallbackStream(1, 2,
//	    portaudio.SampleFmtInt16, 44100)
//	stream.OpenCallback(512, yourCallbackFunc)
//	stream.StartStream()
//
// # Stream Types
//
// PortAudio supports two modes of operation:
//
// Callback Mode (recommended for real-time audio):
//   - Low latency (typically 10-20ms)
//   - Automatic buffer management
//   - Best performance
//   - Use NewCallbackStream() or OpenDefaultStream()
//
// Blocking I/O Mode (simpler but higher latency):
//   - Higher latency (typically 50-100ms)
//   - Manual buffer management with Write()
//   - Easier to understand
//   - Use NewOutputStream()
//
// # Thread Safety
//
// This library is NOT thread-safe. Callers must ensure that:
//   - Initialize() and Terminate() are called from a single goroutine
//   - Each PaStream instance is accessed by only one goroutine at a time
//   - No concurrent calls to the same stream's methods (Open, Close, Write, etc.)
//
// It is safe to use multiple PaStream instances from different goroutines
// as long as each stream is accessed by only one goroutine.
//
// # Audio Callback Constraints
//
// Audio callbacks run in a real-time context managed by PortAudio (not a Go goroutine).
// In callbacks, you MUST:
//   - Process audio quickly (typically < 1ms)
//   - Use pre-allocated buffers only
//   - Avoid memory allocation (make, new, append)
//   - Avoid blocking operations (mutex, I/O, time.Sleep)
//   - Avoid calling Go runtime functions
//
// Violating these constraints may cause audio glitches, stuttering, or dropouts.
//
// # Performance
//
// Typical performance on modern hardware (Apple M2 Pro):
//   - Throughput: >200k frames/sec
//   - Latency: 10-20ms (512 frame buffer @ 44.1kHz)
//   - CPU usage: 2-3% for stereo 16-bit @ 44.1kHz
//   - Zero allocations in callback path
//
// # See Also
//
//   - PortAudio documentation: http://www.portaudio.com/docs.html
//   - Production example: github.com/drgolem/learnRingbuffer
package portaudio

/*
#cgo pkg-config: portaudio-2.0
#include <portaudio.h>

// Ensure these PortAudio functions are available
PaDeviceIndex Pa_GetDefaultInputDevice(void);
PaDeviceIndex Pa_GetDefaultOutputDevice(void);
PaHostApiIndex Pa_GetDefaultHostApi(void);
const PaHostErrorInfo* Pa_GetLastHostErrorInfo(void);
*/
import "C"
import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

var (
	// initialized tracks the initialization reference count
	initialized int
	// initMu protects the initialized counter
	initMu sync.Mutex
)

type PaSampleFormat int

const (
	SampleFmtFloat32 PaSampleFormat = C.paFloat32
	SampleFmtInt32   PaSampleFormat = C.paInt32
	SampleFmtInt24   PaSampleFormat = C.paInt24
	SampleFmtInt16   PaSampleFormat = C.paInt16
	SampleFmtInt8    PaSampleFormat = C.paInt8
	SampleFmtUInt8   PaSampleFormat = C.paUInt8
)

// PortAudio error codes (commonly used)
const (
	ErrNoError      = C.paNoError
	ErrBadStreamPtr = C.paBadStreamPtr
)

// PaStreamFlags specify special options when opening a stream
type PaStreamFlags int

const (
	// NoFlag is the default, no special flags set
	NoFlag PaStreamFlags = 0x00000000
	// ClipOff disables automatic output clipping. Recommended for blocking I/O
	// when you guarantee the output samples are within range [-1.0, 1.0].
	ClipOff PaStreamFlags = 0x00000001
	// DitherOff disables dithering when converting from float to integer samples
	DitherOff PaStreamFlags = 0x00000002
	// NeverDropInput prevents PortAudio from dropping input data when the callback is slow
	NeverDropInput PaStreamFlags = 0x00000004
	// PrimeOutputBuffersUsingStreamCallback pre-fills output buffers before starting
	PrimeOutputBuffersUsingStreamCallback PaStreamFlags = 0x00000008
	// PlatformSpecificFlags allows platform-specific flag usage
	PlatformSpecificFlags PaStreamFlags = 0x00010000
)

// PaTime represents time in seconds as used by PortAudio (maps to C double).
type PaTime float64

type PaStreamParameters struct {
	DeviceIndex      int
	ChannelCount     int
	SampleFormat     PaSampleFormat
	SuggestedLatency PaTime
}

// HighLatencyParameters creates stream parameters configured for high latency.
// This is recommended for blocking I/O to avoid underruns.
// Uses the device's DefaultHighInputLatency or DefaultHighOutputLatency.
func HighLatencyParameters(device *DeviceInfo, channels int, format PaSampleFormat, isInput bool) PaStreamParameters {
	latency := device.DefaultHighOutputLatency
	if isInput {
		latency = device.DefaultHighInputLatency
	}

	return PaStreamParameters{
		DeviceIndex:      0, // Caller should set this to the device index
		ChannelCount:     channels,
		SampleFormat:     format,
		SuggestedLatency: latency,
	}
}

// LowLatencyParameters creates stream parameters configured for low latency.
// This is recommended for callback-based I/O for real-time audio processing.
// Uses the device's DefaultLowInputLatency or DefaultLowOutputLatency.
func LowLatencyParameters(device *DeviceInfo, channels int, format PaSampleFormat, isInput bool) PaStreamParameters {
	latency := device.DefaultLowOutputLatency
	if isInput {
		latency = device.DefaultLowInputLatency
	}

	return PaStreamParameters{
		DeviceIndex:      0, // Caller should set this to the device index
		ChannelCount:     channels,
		SampleFormat:     format,
		SuggestedLatency: latency,
	}
}

type PaError struct {
	ErrorCode int
}

// UnanticipatedHostError represents a host-specific error that occurred
// within the underlying audio API (ALSA, CoreAudio, WASAPI, etc.).
type UnanticipatedHostError struct {
	Code             int
	Text             string
	HostApiType      int
	HostErrorCode    int
	HostErrorText    string
}

func (e *UnanticipatedHostError) Error() string {
	if e.HostErrorText != "" {
		return fmt.Sprintf("%s [Host API error %d: %s]", e.Text, e.HostErrorCode, e.HostErrorText)
	}
	return fmt.Sprintf("%s [Host API error %d]", e.Text, e.HostErrorCode)
}

type PaStream struct {
	stream           unsafe.Pointer
	isOpen           bool
	InputParameters  *PaStreamParameters // nil for output-only streams
	OutputParameters *PaStreamParameters // nil for input-only streams
	SampleRate       float64
	// StreamFlags specifies special options for the stream.
	// For blocking I/O, ClipOff is recommended when output samples are guaranteed
	// to be within [-1.0, 1.0] range. Default is NoFlag.
	StreamFlags PaStreamFlags
	// UseHighLatency when true uses DefaultHighOutputLatency instead of
	// DefaultLowOutputLatency. Recommended for blocking I/O to avoid underruns.
	UseHighLatency bool
	// callbackID stores the stream ID for callback-based streams (internal use)
	callbackID int
	// callbackIDPtr stores the C-allocated pointer to the stream ID (for cleanup)
	callbackIDPtr unsafe.Pointer
}

func (e *PaError) Error() string {
	return GetErrorText(e.ErrorCode)
}

func GetVersion() int {
	return int(C.Pa_GetVersion())
}

func GetVersionText() string {
	vi := C.Pa_GetVersionInfo()
	vt := C.GoString(vi.versionText)
	return vt
}

func GetErrorText(errorCode int) string {
	return C.GoString(C.Pa_GetErrorText(C.int(errorCode)))
}

// newError creates an appropriate error from a PortAudio error code.
// For unanticipated host errors, it extracts detailed host-specific information.
func newError(code C.PaError) error {
	if code == C.paNoError {
		return nil
	}

	// Check for unanticipated host error
	if code == C.paUnanticipatedHostError {
		hostErr := C.Pa_GetLastHostErrorInfo()
		if hostErr != nil {
			return &UnanticipatedHostError{
				Code:          int(code),
				Text:          C.GoString(C.Pa_GetErrorText(code)),
				HostApiType:   int(hostErr.hostApiType),
				HostErrorCode: int(hostErr.errorCode),
				HostErrorText: C.GoString(hostErr.errorText),
			}
		}
	}

	return &PaError{int(code)}
}

// Initialize initializes the PortAudio library.
//
// This function MUST be called before using any other PortAudio API functions.
// It initializes internal PortAudio data structures and scans for available devices.
//
// This function uses reference counting, so multiple calls are safe. Each call to
// Initialize must be matched with a corresponding call to Terminate. The library is
// only actually terminated when the last Terminate call is made.
//
// It is safe to call Initialize multiple times from different parts of your program,
// as long as each Initialize is matched with a Terminate.
//
// Example:
//
//	if err := portaudio.Initialize(); err != nil {
//	    log.Fatal("Failed to initialize PortAudio:", err)
//	}
//	defer portaudio.Terminate()
//
// Thread Safety: This function is thread-safe due to internal mutex protection.
func Initialize() error {
	initMu.Lock()
	defer initMu.Unlock()

	if initialized == 0 {
		errCode := C.Pa_Initialize()
		if errCode != C.paNoError {
			return newError(errCode)
		}
	}
	initialized++
	return nil
}

// Terminate terminates the PortAudio library and releases resources.
//
// This function uses reference counting to match Initialize calls. The library is
// only actually terminated when the last Terminate call is made (when the reference
// count reaches zero).
//
// Best practice is to defer Terminate immediately after a successful Initialize:
//
//	portaudio.Initialize()
//	defer portaudio.Terminate()
//
// Thread Safety: This function is thread-safe due to internal mutex protection.
func Terminate() error {
	initMu.Lock()
	defer initMu.Unlock()

	if initialized == 0 {
		return nil
	}

	initialized--
	if initialized == 0 {
		errCode := C.Pa_Terminate()
		if errCode != C.paNoError {
			initialized++ // restore count on error
			return newError(errCode)
		}
	}
	return nil
}

func GetDeviceCount() (int, error) {
	dc := int(C.Pa_GetDeviceCount())
	if dc < 0 {
		return 0, &PaError{dc}
	}
	return dc, nil
}

// Devices returns a slice of all available audio devices.
// This is a convenience function that wraps GetDeviceCount and GetDeviceInfo.
func Devices() ([]*DeviceInfo, error) {
	count, err := GetDeviceCount()
	if err != nil {
		return nil, err
	}

	devices := make([]*DeviceInfo, count)
	for i := 0; i < count; i++ {
		devices[i], err = GetDeviceInfo(i)
		if err != nil {
			return nil, err
		}
	}
	return devices, nil
}

// DefaultInputDevice returns the default input device.
// Returns an error if no default input device is available.
func DefaultInputDevice() (*DeviceInfo, error) {
	index := int(C.Pa_GetDefaultInputDevice())
	if index < 0 {
		return nil, errors.New("no default input device available")
	}
	return GetDeviceInfo(index)
}

// DefaultOutputDevice returns the default output device.
// Returns an error if no default output device is available.
func DefaultOutputDevice() (*DeviceInfo, error) {
	index := int(C.Pa_GetDefaultOutputDevice())
	if index < 0 {
		return nil, errors.New("no default output device available")
	}
	return GetDeviceInfo(index)
}

// const PaDeviceInfo* Pa_GetDeviceInfo	(	PaDeviceIndex 	device	)

type DeviceInfo struct {
	// Index is the PortAudio device index used when opening streams
	Index int
	// const char * 	name
	Name string
	// PaHostApiIndex 	hostApi
	HostApiIndex int
	// int 	maxInputChannels
	MaxInputChannels int
	// int 	maxOutputChannels
	MaxOutputChannels int
	// PaTime 	defaultLowInputLatency
	DefaultLowInputLatency PaTime
	// PaTime 	defaultLowOutputLatency
	DefaultLowOutputLatency PaTime
	// PaTime 	defaultHighInputLatency
	DefaultHighInputLatency PaTime
	// PaTime 	defaultHighOutputLatency
	DefaultHighOutputLatency PaTime
	// double 	defaultSampleRate
	DefaultSampleRate float64
}

func GetDeviceInfo(deviceIdx int) (*DeviceInfo, error) {
	di := C.Pa_GetDeviceInfo(C.int(deviceIdx))
	if di == nil {
		return nil, errors.New("invalid device index")
	}

	devInfo := DeviceInfo{
		Index:                    deviceIdx,
		Name:                     C.GoString(di.name),
		HostApiIndex:             int(di.hostApi),
		MaxInputChannels:         int(di.maxInputChannels),
		MaxOutputChannels:        int(di.maxOutputChannels),
		DefaultLowInputLatency:   PaTime(di.defaultLowInputLatency),
		DefaultLowOutputLatency:  PaTime(di.defaultLowOutputLatency),
		DefaultHighInputLatency:  PaTime(di.defaultHighInputLatency),
		DefaultHighOutputLatency: PaTime(di.defaultHighOutputLatency),
		DefaultSampleRate:        float64(di.defaultSampleRate),
	}

	return &devInfo, nil
}

func GetHostApiCount() (int, error) {
	hc := int(C.Pa_GetHostApiCount())
	if hc < 0 {
		return 0, &PaError{hc}
	}
	return hc, nil
}

// HostApis returns a slice of all available host APIs.
// This is a convenience function that wraps GetHostApiCount and GetHostApiInfo.
func HostApis() ([]*HostApiInfo, error) {
	count, err := GetHostApiCount()
	if err != nil {
		return nil, err
	}

	apis := make([]*HostApiInfo, count)
	for i := 0; i < count; i++ {
		apis[i], err = GetHostApiInfo(i)
		if err != nil {
			return nil, err
		}
	}
	return apis, nil
}

type HostApiInfo struct {
	Type                int
	Name                string
	DeviceCount         int
	DefaultInputDevice  int
	DefaultOutputDevice int
}

func GetHostApiInfo(hostApiIdx int) (*HostApiInfo, error) {
	hi := C.Pa_GetHostApiInfo(C.int(hostApiIdx))
	if hi == nil {
		return nil, errors.New("invalid host API index")
	}

	hostInfo := HostApiInfo{
		Type:                int(hi._type),
		Name:                C.GoString(hi.name),
		DeviceCount:         int(hi.deviceCount),
		DefaultInputDevice:  int(hi.defaultInputDevice),
		DefaultOutputDevice: int(hi.defaultOutputDevice),
	}

	return &hostInfo, nil
}

func IsFormatSupported(inputParameters *PaStreamParameters, outputParameters *PaStreamParameters, sampleRate float64) error {
	var inParams, outParams *C.PaStreamParameters

	if inputParameters != nil {
		inParams = &C.PaStreamParameters{
			device:           C.int(inputParameters.DeviceIndex),
			channelCount:     C.int(inputParameters.ChannelCount),
			sampleFormat:     C.PaSampleFormat(inputParameters.SampleFormat),
			suggestedLatency: C.double(inputParameters.SuggestedLatency),
		}
	}

	if outputParameters != nil {
		outParams = &C.PaStreamParameters{
			device:           C.int(outputParameters.DeviceIndex),
			channelCount:     C.int(outputParameters.ChannelCount),
			sampleFormat:     C.PaSampleFormat(outputParameters.SampleFormat),
			suggestedLatency: C.double(outputParameters.SuggestedLatency),
		}
	}

	errCode := C.Pa_IsFormatSupported(inParams, outParams, C.double(sampleRate))
	if errCode != C.paFormatIsSupported {
		return newError(errCode)
	}
	return nil
}

// NewStream creates a new output stream with the specified parameters.
// The stream must be opened with Open() or OpenCallback() before use.
//
// This function validates that the format is supported before creating the stream.
// For convenience functions that set reasonable defaults, see NewOutputStream,
// NewCallbackStream, or OpenDefaultStream.
//
// Note: Currently only output streams are supported. Input and duplex streams
// will be added in a future version.
func NewStream(outParams PaStreamParameters, sampleRate float64) (*PaStream, error) {
	err := IsFormatSupported(nil, &outParams, sampleRate)
	if err != nil {
		return nil, err
	}

	st := PaStream{
		OutputParameters: &outParams,
		SampleRate:       sampleRate,
	}

	return &st, nil
}

// NewInputStream creates a new input stream for recording audio.
// The stream must be opened with Open() or OpenCallback() before use.
//
// Parameters:
//   - inParams: Input stream parameters (device, channels, format)
//   - sampleRate: Sample rate in Hz (e.g., 44100, 48000)
//
// Example:
//
//	device, _ := portaudio.DefaultInputDevice()
//	params := portaudio.PaStreamParameters{
//	    DeviceIndex:  device.HostApiIndex,
//	    ChannelCount: 1, // mono
//	    SampleFormat: portaudio.SampleFmtInt16,
//	}
//	stream, err := portaudio.NewInputStream(params, 44100)
func NewInputStream(inParams PaStreamParameters, sampleRate float64) (*PaStream, error) {
	err := IsFormatSupported(&inParams, nil, sampleRate)
	if err != nil {
		return nil, err
	}

	st := PaStream{
		InputParameters: &inParams,
		SampleRate:      sampleRate,
	}

	return &st, nil
}

// NewOutputStream creates a new output stream with reasonable defaults for blocking I/O.
// This is a convenience function that simplifies common use cases.
//
// Parameters:
//   - device: Device index for audio output (use DefaultOutputDevice() to get default)
//   - channels: Number of audio channels (1=mono, 2=stereo)
//   - sampleFormat: Sample format (e.g., SampleFmtInt16, SampleFmtFloat32)
//   - sampleRate: Sample rate in Hz (e.g., 44100, 48000)
//
// The returned stream is configured with:
//   - High latency mode (recommended for blocking I/O to avoid underruns)
//   - ClipOff flag (assumes samples are within valid range)
//
// Example:
//
//	device, _ := portaudio.DefaultOutputDevice()
//	stream, err := portaudio.NewOutputStream(device.Index, 2, portaudio.SampleFmtInt16, 44100)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer stream.Close()
//	stream.Open(512)
//	stream.StartStream()
func NewOutputStream(device int, channels int, sampleFormat PaSampleFormat, sampleRate float64) (*PaStream, error) {
	params := PaStreamParameters{
		DeviceIndex:  device,
		ChannelCount: channels,
		SampleFormat: sampleFormat,
	}

	stream, err := NewStream(params, sampleRate)
	if err != nil {
		return nil, err
	}

	// Configure for blocking I/O
	stream.UseHighLatency = true
	stream.StreamFlags = ClipOff

	return stream, nil
}

// NewCallbackStream creates a new output stream optimized for callback-based audio.
// This is a convenience function that sets appropriate defaults for real-time audio.
//
// Parameters:
//   - device: Device index for audio output
//   - channels: Number of audio channels (1=mono, 2=stereo)
//   - sampleFormat: Sample format (e.g., SampleFmtInt16, SampleFmtFloat32)
//   - sampleRate: Sample rate in Hz (e.g., 44100, 48000)
//
// The returned stream is configured with:
//   - Low latency mode (optimized for real-time audio callbacks)
//   - ClipOff flag (assumes samples are within valid range)
//
// After creating the stream, call OpenCallback() to register your audio callback,
// then StartStream() to begin audio processing.
//
// Example:
//
//	stream, err := portaudio.NewCallbackStream(1, 2, portaudio.SampleFmtFloat32, 44100)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer stream.CloseCallback()
//
//	stream.OpenCallback(512, func(input, output []byte, frameCount uint, ...) portaudio.StreamCallbackResult {
//	    // Generate or process audio
//	    return portaudio.Continue
//	})
//	stream.StartStream()
func NewCallbackStream(device int, channels int, sampleFormat PaSampleFormat, sampleRate float64) (*PaStream, error) {
	params := PaStreamParameters{
		DeviceIndex:  device,
		ChannelCount: channels,
		SampleFormat: sampleFormat,
	}

	stream, err := NewStream(params, sampleRate)
	if err != nil {
		return nil, err
	}

	// Configure for callback mode
	stream.UseHighLatency = false // Low latency for real-time audio
	stream.StreamFlags = ClipOff

	return stream, nil
}

// OpenDefaultStream opens a stream using the default output device with simplified parameters.
// This is the easiest way to get started with audio output.
//
// Parameters:
//   - channels: Number of audio channels (1=mono, 2=stereo)
//   - sampleFormat: Sample format (e.g., SampleFmtInt16, SampleFmtFloat32)
//   - sampleRate: Sample rate in Hz (e.g., 44100, 48000)
//   - framesPerBuffer: Buffer size in frames (e.g., 512, 1024)
//   - callback: Audio callback function (or nil for blocking I/O)
//
// If callback is nil, the stream is opened for blocking I/O (use Write() to send audio).
// If callback is provided, the stream is opened for callback mode.
//
// Returns an opened and ready-to-use stream. Call StartStream() to begin audio.
//
// Example (blocking I/O):
//
//	stream, err := portaudio.OpenDefaultStream(2, portaudio.SampleFmtInt16, 44100, 512, nil)
//	stream.StartStream()
//	stream.Write(frames, buffer)
//
// Example (callback mode):
//
//	stream, err := portaudio.OpenDefaultStream(2, portaudio.SampleFmtFloat32, 44100, 512,
//	    func(input, output []byte, frameCount uint, ...) portaudio.StreamCallbackResult {
//	        // Generate audio
//	        return portaudio.Continue
//	    })
//	stream.StartStream()
func OpenDefaultStream(channels int, sampleFormat PaSampleFormat, sampleRate float64, framesPerBuffer int, callback StreamCallback) (*PaStream, error) {
	// Get default output device
	device, err := DefaultOutputDevice()
	if err != nil {
		return nil, fmt.Errorf("failed to get default output device: %w", err)
	}

	var stream *PaStream
	if callback != nil {
		// Callback mode
		stream, err = NewCallbackStream(device.Index, channels, sampleFormat, sampleRate)
		if err != nil {
			return nil, err
		}
		if err := stream.OpenCallback(framesPerBuffer, callback); err != nil {
			return nil, err
		}
	} else {
		// Blocking I/O mode
		stream, err = NewOutputStream(device.Index, channels, sampleFormat, sampleRate)
		if err != nil {
			return nil, err
		}
		if err := stream.Open(framesPerBuffer); err != nil {
			return nil, err
		}
	}

	return stream, nil
}

func (s *PaStream) Open(framesPerBuffer int) error {
	if s.isOpen {
		return errors.New("stream already open")
	}

	if framesPerBuffer <= 0 {
		return errors.New("framesPerBuffer must be positive")
	}

	var inParams, outParams *C.PaStreamParameters

	// Setup input parameters if this is an input or duplex stream
	if s.InputParameters != nil {
		di, err := GetDeviceInfo(s.InputParameters.DeviceIndex)
		if err != nil {
			return err
		}

		latency := di.DefaultLowInputLatency
		if s.UseHighLatency {
			latency = di.DefaultHighInputLatency
		}

		inParams = &C.PaStreamParameters{
			device:           C.int(s.InputParameters.DeviceIndex),
			channelCount:     C.int(s.InputParameters.ChannelCount),
			sampleFormat:     C.PaSampleFormat(s.InputParameters.SampleFormat),
			suggestedLatency: C.double(latency),
		}
	}

	// Setup output parameters if this is an output or duplex stream
	if s.OutputParameters != nil {
		di, err := GetDeviceInfo(s.OutputParameters.DeviceIndex)
		if err != nil {
			return err
		}

		latency := di.DefaultLowOutputLatency
		if s.UseHighLatency {
			latency = di.DefaultHighOutputLatency
		}

		outParams = &C.PaStreamParameters{
			device:           C.int(s.OutputParameters.DeviceIndex),
			channelCount:     C.int(s.OutputParameters.ChannelCount),
			sampleFormat:     C.PaSampleFormat(s.OutputParameters.SampleFormat),
			suggestedLatency: C.double(latency),
		}
	}

	// Use configured stream flags, or NoFlag if not set
	streamFlags := s.StreamFlags
	if streamFlags == 0 {
		streamFlags = NoFlag
	}

	errCode := C.Pa_OpenStream(&s.stream,
		inParams,
		outParams,
		C.double(s.SampleRate),
		C.ulong(framesPerBuffer),
		C.ulong(streamFlags),
		nil,
		nil)

	if errCode != C.paNoError {
		return newError(errCode)
	}

	s.isOpen = true

	return nil
}

func (s *PaStream) Close() error {
	if !s.isOpen {
		return nil
	}

	errCode := C.Pa_CloseStream(s.stream)
	if errCode != C.paNoError {
		return newError(errCode)
	}

	s.isOpen = false

	return nil
}

func (s *PaStream) StartStream() error {
	if !s.isOpen {
		return &PaError{int(C.paBadStreamPtr)}
	}

	errCode := C.Pa_StartStream(s.stream)
	if errCode != C.paNoError {
		return newError(errCode)
	}

	return nil
}

func (s *PaStream) StopStream() error {
	if !s.isOpen {
		return &PaError{int(C.paBadStreamPtr)}
	}

	errCode := C.Pa_StopStream(s.stream)
	if errCode != C.paNoError {
		return newError(errCode)
	}

	return nil
}

func (s *PaStream) GetWriteAvailable() (int, error) {
	if !s.isOpen {
		return 0, &PaError{int(C.paBadStreamPtr)}
	}

	wa := C.Pa_GetStreamWriteAvailable(s.stream)
	if wa < 0 {
		return 0, &PaError{int(wa)}
	}

	return int(wa), nil
}

// GetSampleSize returns the size in bytes for a given sample format.
// Returns 0 for unknown formats.
func GetSampleSize(format PaSampleFormat) int {
	switch format {
	case SampleFmtFloat32:
		return 4
	case SampleFmtInt32:
		return 4
	case SampleFmtInt24:
		return 3
	case SampleFmtInt16:
		return 2
	case SampleFmtInt8:
		return 1
	case SampleFmtUInt8:
		return 1
	default:
		return 0
	}
}

// Write writes audio data to the stream.
// frames specifies the number of frames to write (not bytes).
// buf must contain exactly frames * channelCount * sampleSize bytes.
func (s *PaStream) Write(frames int, buf []byte) error {
	if !s.isOpen {
		return &PaError{int(C.paBadStreamPtr)}
	}

	if len(buf) == 0 {
		return errors.New("buffer is empty")
	}

	if frames <= 0 {
		return errors.New("frames must be positive")
	}

	// Calculate expected buffer size
	sampleSize := GetSampleSize(s.OutputParameters.SampleFormat)
	if sampleSize == 0 {
		return errors.New("unsupported sample format")
	}

	expectedSize := frames * s.OutputParameters.ChannelCount * sampleSize
	if len(buf) != expectedSize {
		return fmt.Errorf("buffer size mismatch: expected %d bytes for %d frames, got %d bytes",
			expectedSize, frames, len(buf))
	}

	// TODO: only interleaved frames supported

	buffer := unsafe.Pointer(&buf[0])

	errCode := C.Pa_WriteStream(s.stream, buffer, C.ulong(frames))
	if errCode != C.paNoError {
		return newError(errCode)
	}

	return nil
}
