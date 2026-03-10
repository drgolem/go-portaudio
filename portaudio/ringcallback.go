package portaudio

/*
#cgo pkg-config: portaudio-2.0
#include <portaudio.h>
#include <stdlib.h>
#include <string.h>
#include <stdatomic.h>

// C-side SPSC ring buffer for GC-free audio callback.
// The callback reads from this buffer entirely in C — no Go runtime involvement.
// The Go producer writes via exported functions.
typedef struct {
    unsigned char *buf;
    int            cap;       // total capacity in bytes
    _Atomic int    readPos;   // consumer (callback) position
    _Atomic int    writePos;  // producer (Go) position
    _Atomic long   underflows;
    _Atomic long   silentBytes;
    _Atomic long   samplesPlayed;
    _Atomic int    muted;     // when non-zero, callback outputs silence and drains ring
    int            frameSize; // bytes per frame (channels * bytesPerSample)
} CRingBuffer;

static CRingBuffer* cring_new(int capacity, int frameSize) {
    CRingBuffer *rb = (CRingBuffer*)calloc(1, sizeof(CRingBuffer));
    if (!rb) return NULL;
    rb->buf = (unsigned char*)calloc(1, capacity);
    if (!rb->buf) { free(rb); return NULL; }
    rb->cap = capacity;
    rb->frameSize = frameSize;
    atomic_store(&rb->readPos, 0);
    atomic_store(&rb->writePos, 0);
    atomic_store(&rb->underflows, 0);
    atomic_store(&rb->silentBytes, 0);
    atomic_store(&rb->samplesPlayed, 0);
    atomic_store(&rb->muted, 0);
    return rb;
}

static void cring_free(CRingBuffer *rb) {
    if (rb) {
        free(rb->buf);
        free(rb);
    }
}

// Mute makes the callback output silence and drain the ring buffer.
// Called from Go (producer) on stop/pause for immediate silence.
static void cring_mute(CRingBuffer *rb) {
    atomic_store_explicit(&rb->muted, 1, memory_order_release);
}

// Unmute resumes normal playback from the ring buffer.
// The caller should ensure the ring is drained (Available()==0) before
// unmuting to avoid playing stale audio.
static void cring_unmute(CRingBuffer *rb) {
    atomic_store_explicit(&rb->muted, 0, memory_order_release);
}

// Available bytes to read (called from callback — must be fast).
static inline int cring_available(CRingBuffer *rb) {
    int w = atomic_load_explicit(&rb->writePos, memory_order_acquire);
    int r = atomic_load_explicit(&rb->readPos, memory_order_relaxed);
    int avail = w - r;
    if (avail < 0) avail += rb->cap;
    return avail;
}

// Available space to write (called from Go producer).
static inline int cring_free_space(CRingBuffer *rb) {
    return rb->cap - 1 - cring_available(rb);
}

// Read n bytes from ring into dst. Returns bytes actually read.
// Only called from C callback (single consumer).
static int cring_read(CRingBuffer *rb, unsigned char *dst, int n) {
    int avail = cring_available(rb);
    if (n > avail) n = avail;
    if (n <= 0) return 0;

    int r = atomic_load_explicit(&rb->readPos, memory_order_relaxed);
    int firstChunk = rb->cap - r;
    if (firstChunk >= n) {
        memcpy(dst, rb->buf + r, n);
    } else {
        memcpy(dst, rb->buf + r, firstChunk);
        memcpy(dst + firstChunk, rb->buf, n - firstChunk);
    }

    int newR = (r + n) % rb->cap;
    atomic_store_explicit(&rb->readPos, newR, memory_order_release);
    return n;
}

// Write n bytes from src into ring. Returns bytes actually written.
// Called from Go producer (single producer).
static int cring_write(CRingBuffer *rb, const unsigned char *src, int n) {
    int space = cring_free_space(rb);
    if (n > space) n = space;
    if (n <= 0) return 0;

    int w = atomic_load_explicit(&rb->writePos, memory_order_relaxed);
    int firstChunk = rb->cap - w;
    if (firstChunk >= n) {
        memcpy(rb->buf + w, src, n);
    } else {
        memcpy(rb->buf + w, src, firstChunk);
        memcpy(rb->buf, src + firstChunk, n - firstChunk);
    }

    int newW = (w + n) % rb->cap;
    atomic_store_explicit(&rb->writePos, newW, memory_order_release);
    return n;
}

// Pure C callback — reads from SPSC ring buffer, never enters Go runtime.
// This avoids GC stop-the-world pauses that cause audio distortion.
static int paRingCallbackFunc(const void *input, void *output,
                              unsigned long frameCount,
                              const PaStreamCallbackTimeInfo* timeInfo,
                              PaStreamCallbackFlags statusFlags,
                              void *userData) {
    CRingBuffer *rb = (CRingBuffer*)userData;
    int bytesNeeded = (int)frameCount * rb->frameSize;

    // When muted, drain the ring buffer but output silence.
    if (atomic_load_explicit(&rb->muted, memory_order_acquire)) {
        // Advance readPos to writePos to discard buffered data.
        int w = atomic_load_explicit(&rb->writePos, memory_order_acquire);
        atomic_store_explicit(&rb->readPos, w, memory_order_release);
        memset(output, 0, bytesNeeded);
        return paContinue;
    }

    // Only read frame-aligned data
    int avail = cring_available(rb);
    int aligned = (avail / rb->frameSize) * rb->frameSize;
    int toRead = bytesNeeded < aligned ? bytesNeeded : aligned;

    int n = 0;
    if (toRead > 0) {
        n = cring_read(rb, (unsigned char*)output, toRead);
    }

    if (n < bytesNeeded) {
        // Fill remaining with silence
        memset((unsigned char*)output + n, 0, bytesNeeded - n);
        atomic_fetch_add(&rb->silentBytes, bytesNeeded - n);
        if (n == 0) {
            atomic_fetch_add(&rb->underflows, 1);
        }
    }

    if (statusFlags & paOutputUnderflow) {
        atomic_fetch_add(&rb->underflows, 1);
    }

    atomic_fetch_add(&rb->samplesPlayed, n / rb->frameSize);
    return paContinue;
}

// Open a stream with the pure-C ring callback.
static int openStreamWithRingCallback(void** stream,
                                      void* inputParameters,
                                      void* outputParameters,
                                      double sampleRate,
                                      unsigned long framesPerBuffer,
                                      unsigned long streamFlags,
                                      CRingBuffer *ring) {
    return Pa_OpenStream((PaStream**)stream,
                        (const PaStreamParameters*)inputParameters,
                        (const PaStreamParameters*)outputParameters,
                        sampleRate, framesPerBuffer,
                        (PaStreamFlags)streamFlags,
                        paRingCallbackFunc, (void*)ring);
}
*/
import "C"
import (
	"errors"
	"fmt"
	"unsafe"
)

// CRing is a C-allocated SPSC ring buffer for use with the pure-C PortAudio
// callback. The callback reads from this buffer entirely in C, avoiding Go
// runtime GC pauses that cause audio distortion at high sample rates.
//
// Usage:
//
//	ring := NewCRing(capacity, frameSize)
//	defer ring.Free()
//	stream.OpenRingCallback(framesPerBuffer, ring)
//	stream.StartStream()
//	// Producer loop:
//	ring.Write(audioData)
type CRing struct {
	rb *C.CRingBuffer
}

// NewCRing creates a C-allocated SPSC ring buffer.
// capacity is the total size in bytes.
// frameSize is bytes per audio frame (channels * bytesPerSample).
func NewCRing(capacity, frameSize int) *CRing {
	rb := C.cring_new(C.int(capacity), C.int(frameSize))
	if rb == nil {
		panic("failed to allocate C ring buffer")
	}
	return &CRing{rb: rb}
}

// Free releases the C-allocated memory. Must be called when done.
func (r *CRing) Free() {
	if r.rb != nil {
		C.cring_free(r.rb)
		r.rb = nil
	}
}

// Mute makes the callback output silence and drain buffered audio.
// Call on stop/pause for immediate silence. Safe to call from Go (producer).
func (r *CRing) Mute() {
	if r.rb != nil {
		C.cring_mute(r.rb)
	}
}

// Unmute resumes normal playback from the ring buffer.
// Call before writing new audio data. Safe to call from Go (producer).
func (r *CRing) Unmute() {
	if r.rb != nil {
		C.cring_unmute(r.rb)
	}
}

// Write writes audio data into the ring buffer. Returns bytes written.
// Safe to call from Go (single producer).
func (r *CRing) Write(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return int(C.cring_write(r.rb, (*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data))))
}

// Available returns the number of unread bytes in the ring buffer.
func (r *CRing) Available() int {
	return int(C.cring_available(r.rb))
}

// FreeSpace returns the number of bytes that can be written.
func (r *CRing) FreeSpace() int {
	return int(C.cring_free_space(r.rb))
}

// Cap returns the ring buffer capacity in bytes.
func (r *CRing) Cap() int {
	return int(r.rb.cap)
}

// Underflows returns the cumulative underflow count.
func (r *CRing) Underflows() int64 {
	return int64(r.rb.underflows)
}

// SilentBytes returns the cumulative count of silence bytes inserted.
func (r *CRing) SilentBytes() int64 {
	return int64(r.rb.silentBytes)
}

// SamplesPlayed returns the cumulative count of frames played.
func (r *CRing) SamplesPlayed() int64 {
	return int64(r.rb.samplesPlayed)
}

// OpenRingCallback opens the stream with a pure-C callback that reads from
// the provided CRing buffer. The callback never enters Go runtime, making it
// immune to GC stop-the-world pauses.
//
// The caller is responsible for writing audio data to the CRing from Go.
// The C callback will read from it on the PortAudio real-time thread.
func (s *PaStream) OpenRingCallback(framesPerBuffer int, ring *CRing) error {
	if s.isOpen {
		return errors.New("stream already open")
	}
	if framesPerBuffer < 0 {
		return errors.New("framesPerBuffer must be non-negative")
	}
	if ring == nil || ring.rb == nil {
		return errors.New("ring buffer cannot be nil")
	}

	var inParams, outParams *C.PaStreamParameters

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

	streamFlags := s.StreamFlags
	if streamFlags == 0 {
		streamFlags = NoFlag
	}

	errCode := C.openStreamWithRingCallback(&s.stream,
		unsafe.Pointer(inParams),
		unsafe.Pointer(outParams),
		C.double(s.SampleRate),
		C.ulong(framesPerBuffer),
		C.ulong(streamFlags),
		ring.rb)

	if errCode != C.paNoError {
		return fmt.Errorf("open ring callback stream: %w", newError(C.PaError(errCode)))
	}

	s.isOpen = true
	return nil
}
