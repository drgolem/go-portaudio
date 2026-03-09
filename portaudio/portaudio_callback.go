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
    // userData points to a malloc'd long containing the stream ID
    long streamId = *(long*)userData;
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
	"runtime"
	"sync/atomic"
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

// streamCallbackInfo holds callback and stream parameters for proper buffer sizing.
// All sizes are pre-computed at OpenCallback time to avoid any work in the
// real-time callback hot path.
type streamCallbackInfo struct {
	callback       StreamCallback
	outputChannels int
	outputFormat   PaSampleFormat
	inputChannels  int
	inputFormat    PaSampleFormat
	hasInput       bool

	// Pre-computed sample sizes to avoid GetSampleSize() calls in the callback.
	outputSampleSize int
	inputSampleSize  int

	// timeInfo is pre-allocated to avoid allocation in the callback hot path.
	// Safe because PortAudio invokes callbacks sequentially per stream.
	timeInfo StreamCallbackTimeInfo
}

// maxCallbackStreams is the maximum number of concurrent callback streams.
// This allows replacing sync.Map with a fixed-size atomic pointer array,
// eliminating map lookup overhead in the real-time callback path.
const maxCallbackStreams = 64

// Callback registry: fixed-size array of atomic pointers for zero-overhead
// lookup in the real-time callback. Entries are set at OpenCallback and
// cleared at CloseCallback.
var (
	callbackSlots [maxCallbackStreams]atomic.Pointer[streamCallbackInfo]
	nextStreamID  atomic.Int64 // monotonically increasing stream ID
)

func init() {
	nextStreamID.Store(1)
}

// registerCallback stores callback info in an atomic slot and returns the slot index.
func registerCallback(callback StreamCallback, info *streamCallbackInfo) int {
	id := int(nextStreamID.Add(1) - 1)
	if id < 0 || id >= maxCallbackStreams {
		// Wrap around if we exhaust IDs (unlikely in practice).
		// Find a free slot.
		for i := range callbackSlots {
			if callbackSlots[i].Load() == nil {
				info.callback = callback
				callbackSlots[i].Store(info)
				return i
			}
		}
		// All slots full — this is a programming error.
		panic("portaudio: exceeded maximum concurrent callback streams")
	}
	info.callback = callback
	callbackSlots[id].Store(info)
	return id
}

// unregisterCallback clears the callback slot.
func unregisterCallback(id int) {
	if id >= 0 && id < maxCallbackStreams {
		callbackSlots[id].Store(nil)
	}
}

// getCallbackInfo retrieves callback info via a single atomic pointer load.
// This is the hot path — called on every audio callback invocation.
func getCallbackInfo(id int) (*streamCallbackInfo, bool) {
	if id < 0 || id >= maxCallbackStreams {
		return nil, false
	}
	info := callbackSlots[id].Load()
	return info, info != nil
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

	if framesPerBuffer < 0 {
		return errors.New("framesPerBuffer must be non-negative")
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
		info.inputSampleSize = GetSampleSize(s.InputParameters.SampleFormat)
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
		info.outputSampleSize = GetSampleSize(s.OutputParameters.SampleFormat)
	}

	// Use configured stream flags, or NoFlag if not set
	streamFlags := s.StreamFlags
	if streamFlags == 0 {
		streamFlags = NoFlag
	}

	// Register callback and get a unique stream ID
	// This avoids passing Go pointers to C (cgo safety)
	streamID := registerCallback(callback, info)

	// Allocate C memory for the stream ID to pass as userData.
	// This avoids unsafe.Pointer(uintptr(int)) which fails Go's checkptr
	// validation under -race. The C callback dereferences this pointer to
	// retrieve the stream ID.
	streamIDPtr := (*C.long)(C.malloc(C.size_t(unsafe.Sizeof(C.long(0)))))
	*streamIDPtr = C.long(streamID)

	// Open stream with callback using our C helper
	errCode := C.openStreamWithCallback(&s.stream,
		unsafe.Pointer(inParams),
		unsafe.Pointer(outParams),
		C.double(s.SampleRate),
		C.ulong(framesPerBuffer),
		C.ulong(streamFlags),
		unsafe.Pointer(streamIDPtr))

	if errCode != C.paNoError {
		C.free(unsafe.Pointer(streamIDPtr))
		unregisterCallback(streamID)
		return newError(C.PaError(errCode))
	}

	// Store the stream ID and C pointer for cleanup
	s.callbackID = streamID
	s.callbackIDPtr = unsafe.Pointer(streamIDPtr)
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
	if s.callbackIDPtr != nil {
		C.free(s.callbackIDPtr)
		s.callbackIDPtr = nil
	}
	return s.Close()
}

// goCallbackBridge is called from the C PortAudio callback on the real-time
// audio thread. Every nanosecond counts here.
//
// Optimizations vs naive approach:
//   - runtime.LockOSThread: pins to the CoreAudio RT thread, prevents Go
//     scheduler from migrating the goroutine mid-callback
//   - Atomic pointer load instead of sync.Map: single atomic dereference
//   - Pre-computed sample sizes: no GetSampleSize() calls
//   - No defer/recover: eliminates deferred closure overhead (~50ns/call)
//
//export goCallbackBridge
func goCallbackBridge(input, output unsafe.Pointer,
	frameCount C.ulong,
	timeInfo unsafe.Pointer,
	statusFlags C.ulong,
	streamID C.long) C.int {

	// Pin this goroutine to the current OS thread (CoreAudio's RT thread).
	// This prevents the Go scheduler from migrating us to a different thread,
	// which would lose the real-time priority that CoreAudio set up.
	// Note: we don't UnlockOSThread — the thread returns to C after we return,
	// and the CGo runtime handles thread management.
	runtime.LockOSThread()

	// Get the callback info via single atomic pointer load.
	info, ok := getCallbackInfo(int(streamID))
	if !ok {
		return C.int(Abort)
	}

	callback := info.callback

	frameCountGo := uint(frameCount)

	var inputBuf []byte
	var outputBuf []byte

	// Create input buffer slice if stream has input (pre-computed sample size)
	if input != nil && info.hasInput {
		inputSize := int(frameCount) * info.inputChannels * info.inputSampleSize
		if inputSize > 0 && inputSize <= (1<<20) {
			inputBuf = (*[1 << 20]byte)(input)[:inputSize:inputSize]
		}
	}

	// Create output buffer slice (pre-computed sample size)
	if output != nil {
		outputSize := int(frameCount) * info.outputChannels * info.outputSampleSize
		if outputSize > 0 && outputSize <= (1<<20) {
			outputBuf = (*[1 << 20]byte)(output)[:outputSize:outputSize]
		}
	}

	// Convert timeInfo from C struct to Go struct (reuse pre-allocated struct)
	var timeInfoGo *StreamCallbackTimeInfo
	if timeInfo != nil {
		cTimeInfo := (*C.PaStreamCallbackTimeInfo)(timeInfo)
		info.timeInfo.InputBufferAdcTime = PaTime(cTimeInfo.inputBufferAdcTime)
		info.timeInfo.CurrentTime = PaTime(cTimeInfo.currentTime)
		info.timeInfo.OutputBufferDacTime = PaTime(cTimeInfo.outputBufferDacTime)
		timeInfoGo = &info.timeInfo
	}

	// Call the Go callback — if it panics, the process crashes (no defer/recover
	// overhead). Callbacks must not panic in real-time audio context.
	return C.int(callback(inputBuf, outputBuf, frameCountGo, timeInfoGo, StreamCallbackFlags(statusFlags)))
}

// goCallbackBridgeSafe is an alternative bridge with panic recovery.
// Not currently used — kept for reference. To enable, change the C wrapper
// to call goCallbackBridgeSafe instead of goCallbackBridge.
func goCallbackBridgeSafe(input, output unsafe.Pointer,
	frameCount C.ulong,
	timeInfo unsafe.Pointer,
	statusFlags C.ulong,
	streamID C.long) (result C.int) {

	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "PANIC in audio callback (stream %d): %v\n", streamID, r)
			result = C.int(Abort)
		}
	}()

	return goCallbackBridge(input, output, frameCount, timeInfo, statusFlags, streamID)
}
