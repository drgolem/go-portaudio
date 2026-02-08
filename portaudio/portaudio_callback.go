package portaudio

/*
#cgo pkg-config: portaudio-2.0
#include <portaudio.h>
#include <stdlib.h>
#include <stdint.h>

// Forward declaration of Go callback bridge
extern int goCallbackBridge(void *input, void *output,
                            unsigned long frameCount,
                            void *timeInfo,
                            unsigned long statusFlags,
                            long streamId);

// C wrapper that can be used as a PaStreamCallback function pointer
static int paStreamCallbackWrapper(const void *input, void *output,
                                   unsigned long frameCount,
                                   const PaStreamCallbackTimeInfo* timeInfo,
                                   PaStreamCallbackFlags statusFlags,
                                   void *userData) {
    // Cast userData (which contains a stream ID) to long
    long streamId = (long)(intptr_t)userData;
    return goCallbackBridge((void*)input, output, frameCount,
                           (void*)timeInfo, (unsigned long)statusFlags, streamId);
}

// Helper function to open a stream with our callback
static int openStreamWithCallback(void** stream,
                                  void* inputParameters,
                                  void* outputParameters,
                                  double sampleRate,
                                  unsigned long framesPerBuffer,
                                  unsigned long streamFlags,
                                  void *userData) {
    return Pa_OpenStream((PaStream**)stream,
                        (const PaStreamParameters*)inputParameters,
                        (const PaStreamParameters*)outputParameters,
                        sampleRate, framesPerBuffer,
                        (PaStreamFlags)streamFlags,
                        paStreamCallbackWrapper, userData);
}
*/
import "C"
import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"
)

// StreamCallback is the Go callback function type.
// It receives audio data and should fill the output buffer with audio samples.
//
// Parameters:
//   - input: input buffer (for recording, may be nil for output-only streams)
//   - output: output buffer to fill with audio samples
//   - frameCount: number of frames to process
//   - timeInfo: timing information about the stream
//   - statusFlags: status flags indicating stream conditions
//
// Returns:
//   - Continue (0) to keep the stream running
//   - Complete (1) to finish gracefully
//   - Abort (2) to stop immediately
//
// IMPORTANT: The callback runs in a real-time context. Avoid:
//   - Memory allocation/deallocation
//   - File I/O or console output
//   - Mutex locks or context switching
//   - Any operations that may block or take unbounded time
type StreamCallback func(
	input, output []byte,
	frameCount uint,
	timeInfo *StreamCallbackTimeInfo,
	statusFlags StreamCallbackFlags,
) StreamCallbackResult

// StreamCallbackResult indicates what the callback wants the stream to do
type StreamCallbackResult int

const (
	// Continue tells PortAudio to continue invoking the callback
	Continue StreamCallbackResult = 0
	// Complete tells PortAudio to finish playing remaining buffers then stop
	Complete StreamCallbackResult = 1
	// Abort tells PortAudio to stop immediately, discarding buffered data
	Abort StreamCallbackResult = 2
)

// StreamCallbackFlags provides information about the stream state
type StreamCallbackFlags uint

const (
	// InputUnderflow indicates input data was lost before callback was called
	InputUnderflow StreamCallbackFlags = 0x00000001
	// InputOverflow indicates input data was discarded after callback returned
	InputOverflow StreamCallbackFlags = 0x00000002
	// OutputUnderflow indicates output buffer had insufficient data
	OutputUnderflow StreamCallbackFlags = 0x00000004
	// OutputOverflow indicates output data was discarded
	OutputOverflow StreamCallbackFlags = 0x00000008
	// PrimingOutput indicates initial output is being generated
	PrimingOutput StreamCallbackFlags = 0x00000010
)

// StreamCallbackTimeInfo provides timing information for the callback
type StreamCallbackTimeInfo struct {
	InputBufferAdcTime  PaTime // Time when first sample of input buffer was captured
	CurrentTime         PaTime // Time when callback was invoked
	OutputBufferDacTime PaTime // Time when first sample of output buffer will be played
}

// streamCallbackInfo holds callback and stream parameters for proper buffer sizing
type streamCallbackInfo struct {
	callback       StreamCallback
	outputChannels int
	outputFormat   PaSampleFormat
	inputChannels  int
	inputFormat    PaSampleFormat
	hasInput       bool
}

// Callback registry to map stream IDs to Go callbacks and stream parameters
// Using integer IDs instead of pointers avoids Go pointer passing issues
var (
	callbackRegistry   = make(map[int]*streamCallbackInfo)
	callbackRegistryMu sync.RWMutex
	nextStreamID       = 1
)

// registerCallback stores a callback and stream info for a stream and returns the stream ID
func registerCallback(callback StreamCallback, info *streamCallbackInfo) int {
	callbackRegistryMu.Lock()
	defer callbackRegistryMu.Unlock()

	id := nextStreamID
	nextStreamID++
	info.callback = callback
	callbackRegistry[id] = info
	return id
}

// unregisterCallback removes a callback for a stream ID
func unregisterCallback(id int) {
	callbackRegistryMu.Lock()
	defer callbackRegistryMu.Unlock()
	delete(callbackRegistry, id)
}

// getCallbackInfo retrieves callback info for a stream ID
func getCallbackInfo(id int) (*streamCallbackInfo, bool) {
	callbackRegistryMu.RLock()
	defer callbackRegistryMu.RUnlock()
	info, ok := callbackRegistry[id]
	return info, ok
}

// OpenCallback opens the stream with a callback function.
// The callback will be invoked by PortAudio to generate or process audio.
//
// Unlike blocking I/O, callback-based streams run in real-time and provide
// better performance and lower latency. However, the callback must follow
// strict real-time constraints (see StreamCallback documentation).
//
// For callback mode, UseHighLatency is typically set to false (low latency).
func (s *PaStream) OpenCallback(framesPerBuffer int, callback StreamCallback) error {
	if s.isOpen {
		return errors.New("stream already open")
	}

	if framesPerBuffer <= 0 {
		return errors.New("framesPerBuffer must be positive")
	}

	if callback == nil {
		return errors.New("callback cannot be nil")
	}

	var inParams, outParams *C.PaStreamParameters
	info := &streamCallbackInfo{}

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

		info.hasInput = true
		info.inputChannels = s.InputParameters.ChannelCount
		info.inputFormat = s.InputParameters.SampleFormat
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

		info.outputChannels = s.OutputParameters.ChannelCount
		info.outputFormat = s.OutputParameters.SampleFormat
	}

	// Use configured stream flags, or NoFlag if not set
	streamFlags := s.StreamFlags
	if streamFlags == 0 {
		streamFlags = NoFlag
	}

	// Register callback and get a unique stream ID
	// This avoids passing Go pointers to C (cgo safety)
	streamID := registerCallback(callback, info)

	// Open stream with callback using our C helper
	// Pass the stream ID as userData (cast to void*)
	// Note: Converting int → uintptr → unsafe.Pointer is safe here because
	// streamID is an integer value, not a Go pointer. We're using the pointer
	// as an integer tag (userData), which is a standard C pattern.
	errCode := C.openStreamWithCallback(&s.stream,
		unsafe.Pointer(inParams),
		unsafe.Pointer(outParams),
		C.double(s.SampleRate),
		C.ulong(framesPerBuffer),
		C.ulong(streamFlags),
		unsafe.Pointer(uintptr(streamID))) //nolint:govet // Safe: integer as user data, not a pointer

	if errCode != C.paNoError {
		unregisterCallback(streamID)
		return &PaError{int(errCode)}
	}

	// Store the stream ID in the PaStream struct so we can clean up later
	s.callbackID = streamID
	s.isOpen = true

	return nil
}

// CloseCallback closes a callback stream and unregisters the callback.
// This should be used instead of Close() for streams opened with OpenCallback().
func (s *PaStream) CloseCallback() error {
	if s.callbackID != 0 {
		unregisterCallback(s.callbackID)
		s.callbackID = 0
	}
	return s.Close()
}

//export goCallbackBridge
func goCallbackBridge(input, output unsafe.Pointer,
	frameCount C.ulong,
	timeInfo unsafe.Pointer,
	statusFlags C.ulong,
	streamID C.long) (result C.int) {

	// Panic recovery - critical for callback stability
	// If a panic occurs in the callback, we log it and abort the stream
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "PANIC in audio callback (stream %d): %v\n", streamID, r)
			result = C.int(Abort)
		}
	}()

	// Get the callback info from registry using the stream ID
	info, ok := getCallbackInfo(int(streamID))
	if !ok {
		// No callback registered, tell PortAudio to abort
		return C.int(Abort)
	}

	callback := info.callback

	// Calculate buffer sizes
	frameCountGo := uint(frameCount)

	var inputBuf []byte
	var outputBuf []byte

	// Create input buffer slice if stream has input
	if input != nil && info.hasInput {
		inputSampleSize := GetSampleSize(info.inputFormat)
		inputSize := int(frameCount) * info.inputChannels * inputSampleSize
		if inputSize > 0 && inputSize <= (1<<20) { // Sanity check: max 1MB
			inputBuf = (*[1 << 20]byte)(input)[:inputSize:inputSize]
		}
	}

	// Create output buffer slice with proper sizing based on stream parameters
	if output != nil {
		outputSampleSize := GetSampleSize(info.outputFormat)
		outputSize := int(frameCount) * info.outputChannels * outputSampleSize
		if outputSize > 0 && outputSize <= (1<<20) { // Sanity check: max 1MB
			outputBuf = (*[1 << 20]byte)(output)[:outputSize:outputSize]
		}
	}

	// Convert timeInfo from C struct to Go struct
	var timeInfoGo *StreamCallbackTimeInfo
	if timeInfo != nil {
		cTimeInfo := (*C.PaStreamCallbackTimeInfo)(timeInfo)
		timeInfoGo = &StreamCallbackTimeInfo{
			InputBufferAdcTime:  PaTime(cTimeInfo.inputBufferAdcTime),
			CurrentTime:         PaTime(cTimeInfo.currentTime),
			OutputBufferDacTime: PaTime(cTimeInfo.outputBufferDacTime),
		}
	}

	// Call the Go callback
	result = C.int(callback(inputBuf, outputBuf, frameCountGo, timeInfoGo, StreamCallbackFlags(statusFlags)))

	return result
}
