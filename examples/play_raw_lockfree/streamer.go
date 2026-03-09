package main

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/drgolem/go-portaudio/portaudio"
)

// AudioStreamer abstracts an audio output stream with internal buffering.
// Write pushes data into an internal buffer; the implementation plays it
// in the background. If the audioFormat changes between calls, the
// implementation drains buffered data in the old format, reconfigures the
// output, and continues with the new format.
type AudioStreamer interface {
	Open(maxSamplesPerBuffer int) error
	Close() error

	StartStream() error
	StopStream() error

	Write(audioFormat FrameFormat, samples int, audio []byte) error
	Buffered() int // unplayed bytes remaining in internal buffer
}

// ---------------------------------------------------------------------------
// PortAudio adapter — owns SPSC ring buffer, supports callback + stream modes
// ---------------------------------------------------------------------------

// paStreamer implements AudioStreamer using PortAudio. Both modes push data
// via Write() into an SPSC ring buffer. The consumer side differs:
//
// Callback mode (low latency):
//
//	Caller → Write() → SPSC → [PA callback] → audio hardware
//
// Stream mode (higher latency):
//
//	Caller → Write() → SPSC → [writer goroutine → Pa_WriteStream] → audio hardware
//
// When Write() receives a new FrameFormat, the streamer drains all remaining
// data in the old format, tears down the PA stream, and recreates it with
// the new parameters.
type paStreamer struct {
	stream         *portaudio.PaStream
	ring           *SPSCRingBuffer
	mode           PlayerMode
	device         int
	sampleFormat   SampleFormat
	channels       int
	bytesPerSample int
	sampleRate     int
	ringMs         int
	framesPerBuf   int

	closed     atomic.Bool
	writerDone chan struct{} // closed when writer goroutine exits (stream mode)

	// Diagnostics
	samplesPlayed atomic.Uint64
	underflows    atomic.Uint64
	silentBytes   atomic.Uint64
}

// NewPortAudioStreamer creates an AudioStreamer backed by PortAudio.
// mode selects callback (low latency) or stream (blocking I/O) playback.
// ringMs controls the size of the internal SPSC ring buffer in milliseconds.
func NewPortAudioStreamer(device, channels int, sampleFormat SampleFormat, sampleRate int, ringMs int, mode PlayerMode) (AudioStreamer, error) {
	stream, err := createPaStream(mode, device, channels, sampleFormat, sampleRate)
	if err != nil {
		return nil, err
	}
	return &paStreamer{
		stream:         stream,
		mode:           mode,
		device:         device,
		sampleFormat:   sampleFormat,
		channels:       channels,
		bytesPerSample: sampleFormat.BytesPerSample(),
		sampleRate:     sampleRate,
		ringMs:         ringMs,
	}, nil
}

func createPaStream(mode PlayerMode, device, channels int, sampleFormat SampleFormat, sampleRate int) (*portaudio.PaStream, error) {
	paFmt := sampleFormat.toPaSampleFormat()
	var stream *portaudio.PaStream
	var err error
	switch mode {
	case ModeStream:
		stream, err = portaudio.NewOutputStream(device, channels, paFmt, float64(sampleRate))
	default:
		stream, err = portaudio.NewCallbackStream(device, channels, paFmt, float64(sampleRate))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}
	return stream, nil
}

// Open creates the internal SPSC ring buffer and opens the PortAudio stream.
// In callback mode it registers an internal PA callback; in stream mode it
// opens a blocking output stream.
func (s *paStreamer) Open(maxSamplesPerBuffer int) error {
	ringSize := s.sampleRate * s.channels * s.bytesPerSample * s.ringMs / 1000
	s.ring = NewSPSCRingBuffer(ringSize)
	s.framesPerBuf = maxSamplesPerBuffer
	switch s.mode {
	case ModeStream:
		return s.stream.Open(maxSamplesPerBuffer)
	default:
		return s.stream.OpenCallback(maxSamplesPerBuffer, s.paCallback)
	}
}

// Close releases the PortAudio stream resources.
func (s *paStreamer) Close() error {
	s.closed.Store(true)
	s.waitWriter()
	if s.stream == nil {
		return nil
	}
	var err error
	switch s.mode {
	case ModeStream:
		err = s.stream.Close()
	default:
		err = s.stream.CloseCallback()
	}
	s.stream = nil
	return err
}

// StartStream starts PortAudio playback. In stream mode it also launches
// an internal writer goroutine that drains the SPSC via Pa_WriteStream.
func (s *paStreamer) StartStream() error {
	if err := s.stream.StartStream(); err != nil {
		return err
	}
	if s.mode == ModeStream {
		s.writerDone = make(chan struct{})
		go s.writer()
	}
	return nil
}

// StopStream signals the writer goroutine to exit (stream mode) and
// then stops the PortAudio stream.
func (s *paStreamer) StopStream() error {
	s.closed.Store(true)
	s.waitWriter()
	return s.stream.StopStream()
}

// waitWriter blocks until the writer goroutine has exited (stream mode only).
func (s *paStreamer) waitWriter() {
	if s.mode == ModeStream && s.writerDone != nil {
		<-s.writerDone
	}
}

// formatChanged returns true if the incoming format differs from the current
// streamer configuration.
func (s *paStreamer) formatChanged(f FrameFormat) bool {
	return f.SampleRate != s.sampleRate ||
		f.Channels != s.channels ||
		f.SampleFormat != s.sampleFormat
}

// reconfigure drains all buffered audio in the current format, tears down
// the PortAudio stream, and recreates it with the new format parameters.
// Called from Write() when a format change is detected.
func (s *paStreamer) reconfigure(newFormat FrameFormat) error {
	slog.Info("reconfiguring audio stream",
		"old", fmt.Sprintf("%d:%d:%d", s.sampleRate, s.channels, s.bytesPerSample*8),
		"new", fmt.Sprintf("%d:%d:%d", newFormat.SampleRate, newFormat.Channels, newFormat.SampleFormat.BytesPerSample()*8),
	)

	// 1. Drain: wait for all buffered data to be played
	for s.ring.Available() > 0 {
		if s.closed.Load() {
			return fmt.Errorf("streamer closed during drain")
		}
		time.Sleep(500 * time.Microsecond)
	}

	// 2. Stop the PA stream (and wait for writer goroutine in stream mode)
	if err := s.stream.StopStream(); err != nil {
		return fmt.Errorf("stop stream: %w", err)
	}
	// Signal writer goroutine to exit and wait
	s.closed.Store(true)
	s.waitWriter()

	// 3. Close the PA stream
	switch s.mode {
	case ModeStream:
		if err := s.stream.Close(); err != nil {
			return fmt.Errorf("close stream: %w", err)
		}
	default:
		if err := s.stream.CloseCallback(); err != nil {
			return fmt.Errorf("close callback: %w", err)
		}
	}

	// 4. Create new PA stream with new format
	stream, err := createPaStream(s.mode, s.device, newFormat.Channels, newFormat.SampleFormat, newFormat.SampleRate)
	if err != nil {
		return fmt.Errorf("recreate stream: %w", err)
	}

	// 5. Update streamer state
	s.stream = stream
	s.sampleFormat = newFormat.SampleFormat
	s.channels = newFormat.Channels
	s.bytesPerSample = newFormat.SampleFormat.BytesPerSample()
	s.sampleRate = newFormat.SampleRate

	// 6. Create new ring buffer (size depends on new format)
	ringSize := s.sampleRate * s.channels * s.bytesPerSample * s.ringMs / 1000
	s.ring = NewSPSCRingBuffer(ringSize)

	// 7. Open the new stream
	switch s.mode {
	case ModeStream:
		if err := s.stream.Open(s.framesPerBuf); err != nil {
			return fmt.Errorf("open stream: %w", err)
		}
	default:
		if err := s.stream.OpenCallback(s.framesPerBuf, s.paCallback); err != nil {
			return fmt.Errorf("open callback: %w", err)
		}
	}

	// 8. Reset closed flag and start the new stream
	s.closed.Store(false)
	if err := s.stream.StartStream(); err != nil {
		return fmt.Errorf("start stream: %w", err)
	}
	if s.mode == ModeStream {
		s.writerDone = make(chan struct{})
		go s.writer()
	}

	slog.Info("audio stream reconfigured")
	return nil
}

// Write pushes audio data into the SPSC ring buffer. Blocks (yields) until
// all bytes are written or the streamer is closed. If audioFormat differs
// from the current configuration, the streamer drains existing data and
// reconfigures PortAudio before writing.
func (s *paStreamer) Write(audioFormat FrameFormat, samples int, audio []byte) error {
	if s.formatChanged(audioFormat) {
		if err := s.reconfigure(audioFormat); err != nil {
			return fmt.Errorf("reconfigure for new format: %w", err)
		}
	}

	written := 0
	for written < len(audio) {
		if s.closed.Load() {
			return fmt.Errorf("streamer closed")
		}
		n := s.ring.Write(audio[written:])
		written += n
		if written < len(audio) {
			time.Sleep(500 * time.Microsecond)
		}
	}
	return nil
}

// Buffered returns the number of unplayed bytes in the internal ring buffer.
func (s *paStreamer) Buffered() int {
	if s.ring == nil {
		return 0
	}
	return s.ring.Available()
}

// SamplesPlayed returns the number of audio frames consumed by output.
func (s *paStreamer) SamplesPlayed() uint64 {
	return s.samplesPlayed.Load()
}

// Underflows returns the number of underflows detected.
func (s *paStreamer) Underflows() uint64 {
	return s.underflows.Load()
}

// paCallback is called by PortAudio on the real-time thread (callback mode).
// It reads from the SPSC ring buffer (lock-free: atomic loads/stores only).
//
// Frame alignment: the SPSC may contain a non-frame-aligned byte count
// if the producer's retry loop was interrupted mid-write. We only consume
// complete frames and leave any trailing partial-frame bytes in the ring
// for the next callback.
func (s *paStreamer) paCallback(
	input, output []byte,
	frameCount uint,
	timeInfo *portaudio.StreamCallbackTimeInfo,
	statusFlags portaudio.StreamCallbackFlags,
) portaudio.StreamCallbackResult {
	if statusFlags&portaudio.OutputUnderflow != 0 {
		s.underflows.Add(1)
	}

	frameSize := s.channels * s.bytesPerSample
	bytesNeeded := int(frameCount) * frameSize

	// Only read frame-aligned bytes from the ring buffer
	available := s.ring.Available()
	alignedAvailable := (available / frameSize) * frameSize
	toRead := min(bytesNeeded, alignedAvailable)

	n := 0
	if toRead > 0 {
		n = s.ring.Read(output[:toRead])
	}

	if n < bytesNeeded {
		clear(output[n:bytesNeeded])
		s.silentBytes.Add(uint64(bytesNeeded - n))
		if n == 0 {
			s.underflows.Add(1)
		}
	}

	s.samplesPlayed.Add(uint64(n / frameSize))
	return portaudio.Continue
}

// writer drains the SPSC ring buffer via Pa_WriteStream (stream mode).
// Runs in its own goroutine, started by StartStream.
func (s *paStreamer) writer() {
	defer close(s.writerDone)

	frameSize := s.channels * s.bytesPerSample
	bufSize := s.framesPerBuf * frameSize
	buf := make([]byte, bufSize)

	for !s.closed.Load() {
		// Only read frame-aligned data from the ring buffer
		available := s.ring.Available()
		aligned := (available / frameSize) * frameSize
		if aligned == 0 {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		toRead := min(aligned, bufSize)

		n := s.ring.Read(buf[:toRead])
		frames := n / frameSize
		if frames > 0 {
			if err := s.stream.Write(frames, buf[:frames*frameSize]); err != nil {
				s.underflows.Add(1)
			}
			s.samplesPlayed.Add(uint64(frames))
		}
	}
}
