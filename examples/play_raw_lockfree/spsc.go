package main

import "sync/atomic"

// SPSCRingBuffer is a lock-free single-producer, single-consumer ring buffer.
//
// It uses two monotonically increasing atomic counters (writePos, readPos)
// and a power-of-2 sized buffer with bitwise masking. No mutexes, no
// TryLock, no CAS loops — just atomic loads and stores.
//
// Memory ordering: Go's sync/atomic provides sequential consistency.
// The producer stores writePos after writing data; the consumer loads
// writePos before reading data. This guarantees the consumer sees all
// data written before the position update.
//
// Thread assignment:
//   - Write + Free: producer goroutine only
//   - Read + Available: consumer (audio callback) only
type SPSCRingBuffer struct {
	// Separate cache lines to prevent false sharing between producer and consumer.
	// On most architectures a cache line is 64 bytes.
	writePos atomic.Uint64
	_pad1    [56]byte
	readPos  atomic.Uint64
	_pad2    [56]byte

	buf  []byte
	mask uint64
}

// NewSPSCRingBuffer creates a ring buffer with capacity rounded up to
// the next power of two.
func NewSPSCRingBuffer(minSize int) *SPSCRingBuffer {
	size := 1
	for size < minSize {
		size <<= 1
	}
	return &SPSCRingBuffer{
		buf:  make([]byte, size),
		mask: uint64(size - 1),
	}
}

// Write copies up to len(p) bytes into the buffer.
// Returns the number of bytes actually written.
// Non-blocking. Only call from the producer goroutine.
func (rb *SPSCRingBuffer) Write(p []byte) int {
	w := rb.writePos.Load()
	r := rb.readPos.Load()

	free := uint64(len(rb.buf)) - (w - r)
	if free == 0 {
		return 0
	}

	n := uint64(len(p))
	if n > free {
		n = free
	}

	pos := w & rb.mask
	// Copy in one or two segments depending on wrap-around
	first := uint64(len(rb.buf)) - pos
	if first >= n {
		copy(rb.buf[pos:pos+n], p[:n])
	} else {
		copy(rb.buf[pos:], p[:first])
		copy(rb.buf[:n-first], p[first:n])
	}

	rb.writePos.Store(w + n)
	return int(n)
}

// Read copies up to len(p) bytes from the buffer.
// Returns the number of bytes actually read.
// Non-blocking. Only call from the consumer (audio callback).
func (rb *SPSCRingBuffer) Read(p []byte) int {
	r := rb.readPos.Load()
	w := rb.writePos.Load()

	available := w - r
	if available == 0 {
		return 0
	}

	n := uint64(len(p))
	if n > available {
		n = available
	}

	pos := r & rb.mask
	first := uint64(len(rb.buf)) - pos
	if first >= n {
		copy(p[:n], rb.buf[pos:pos+n])
	} else {
		copy(p[:first], rb.buf[pos:])
		copy(p[first:n], rb.buf[:n-first])
	}

	rb.readPos.Store(r + n)
	return int(n)
}

// Available returns the number of bytes available to read.
func (rb *SPSCRingBuffer) Available() int {
	return int(rb.writePos.Load() - rb.readPos.Load())
}

// Free returns the number of bytes available to write.
func (rb *SPSCRingBuffer) Free() int {
	return len(rb.buf) - int(rb.writePos.Load()-rb.readPos.Load())
}

// Full returns true if the buffer has no space for writing.
func (rb *SPSCRingBuffer) Full() bool {
	return rb.writePos.Load()-rb.readPos.Load() == uint64(len(rb.buf))
}
