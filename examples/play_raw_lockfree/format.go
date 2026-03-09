package main

import "github.com/drgolem/go-portaudio/portaudio"

// PlayerMode selects the internal PortAudio playback strategy.
// Both modes use the same SPSC ring buffer and Write() API; only the
// consumer side differs.
type PlayerMode string

const (
	// ModeCallback uses a PortAudio callback to drain the SPSC (low latency).
	ModeCallback PlayerMode = "callback"
	// ModeStream uses an internal goroutine + Pa_WriteStream (higher latency).
	ModeStream PlayerMode = "stream"
)

// SampleFormat describes the PCM sample encoding.
type SampleFormat int

const (
	SampleFmtInt8    SampleFormat = iota + 1
	SampleFmtInt16
	SampleFmtInt24
	SampleFmtInt32
	SampleFmtFloat32
)

// BytesPerSample returns the number of bytes occupied by one sample.
func (f SampleFormat) BytesPerSample() int {
	switch f {
	case SampleFmtInt8:
		return 1
	case SampleFmtInt16:
		return 2
	case SampleFmtInt24:
		return 3
	case SampleFmtInt32, SampleFmtFloat32:
		return 4
	default:
		return 0
	}
}

func (f SampleFormat) toPaSampleFormat() portaudio.PaSampleFormat {
	switch f {
	case SampleFmtInt8:
		return portaudio.SampleFmtInt8
	case SampleFmtInt16:
		return portaudio.SampleFmtInt16
	case SampleFmtInt24:
		return portaudio.SampleFmtInt24
	case SampleFmtInt32:
		return portaudio.SampleFmtInt32
	case SampleFmtFloat32:
		return portaudio.SampleFmtFloat32
	default:
		return portaudio.SampleFmtInt16
	}
}

// FrameFormat describes the audio format of a frame.
type FrameFormat struct {
	SampleRate   int
	Channels     int
	SampleFormat SampleFormat
}
