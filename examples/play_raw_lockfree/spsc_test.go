package main

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestNewSPSCRingBuffer(t *testing.T) {
	tests := []struct {
		input    int
		expected int
	}{
		{1, 1},
		{2, 2},
		{3, 4},
		{7, 8},
		{100, 128},
		{1024, 1024},
		{1025, 2048},
		{4096, 4096},
	}

	for _, tt := range tests {
		rb := NewSPSCRingBuffer(tt.input)
		if len(rb.buf) != tt.expected {
			t.Errorf("NewSPSCRingBuffer(%d): expected size %d, got %d", tt.input, tt.expected, len(rb.buf))
		}
		if rb.mask != uint64(tt.expected-1) {
			t.Errorf("NewSPSCRingBuffer(%d): expected mask %d, got %d", tt.input, tt.expected-1, rb.mask)
		}
	}
}

func TestWriteRead(t *testing.T) {
	rb := NewSPSCRingBuffer(16)

	data := []byte("hello")
	n := rb.Write(data)
	if n != len(data) {
		t.Errorf("Write: expected %d bytes, wrote %d", len(data), n)
	}

	if rb.Available() != len(data) {
		t.Errorf("Available: expected %d, got %d", len(data), rb.Available())
	}

	readBuf := make([]byte, 10)
	n = rb.Read(readBuf)
	if n != len(data) {
		t.Errorf("Read: expected %d bytes, read %d", len(data), n)
	}
	if !bytes.Equal(readBuf[:n], data) {
		t.Errorf("Read: expected %q, got %q", data, readBuf[:n])
	}
}

func TestWrapAround(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	// Fill partially and drain to move pointers
	data1 := []byte("abc")
	rb.Write(data1)
	readBuf := make([]byte, 3)
	rb.Read(readBuf)

	// Write data that wraps around the buffer boundary
	data2 := []byte("defgh")
	n := rb.Write(data2)
	if n != len(data2) {
		t.Errorf("Write: expected %d bytes, wrote %d", len(data2), n)
	}

	readBuf2 := make([]byte, 10)
	n = rb.Read(readBuf2)
	if n != len(data2) {
		t.Errorf("Read: expected %d bytes, read %d", len(data2), n)
	}
	if !bytes.Equal(readBuf2[:n], data2) {
		t.Errorf("Read after wrap: expected %q, got %q", data2, readBuf2[:n])
	}
}

func TestPartialWrite(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	// Try to write more than capacity — should write partial
	data := make([]byte, 12)
	for i := range data {
		data[i] = byte(i)
	}
	n := rb.Write(data)
	if n != 8 {
		t.Errorf("Write: expected 8 bytes (capacity), wrote %d", n)
	}

	// Verify the partial data
	readBuf := make([]byte, 8)
	n = rb.Read(readBuf)
	if n != 8 {
		t.Errorf("Read: expected 8 bytes, read %d", n)
	}
	if !bytes.Equal(readBuf, data[:8]) {
		t.Errorf("Read: data mismatch after partial write")
	}
}

func TestPartialRead(t *testing.T) {
	rb := NewSPSCRingBuffer(16)

	rb.Write([]byte("hi"))

	// Read with a bigger buffer — should only get 2 bytes
	readBuf := make([]byte, 10)
	n := rb.Read(readBuf)
	if n != 2 {
		t.Errorf("Read: expected 2 bytes, read %d", n)
	}
	if !bytes.Equal(readBuf[:n], []byte("hi")) {
		t.Errorf("Read: expected %q, got %q", "hi", readBuf[:n])
	}
}

func TestEmptyRead(t *testing.T) {
	rb := NewSPSCRingBuffer(16)

	readBuf := make([]byte, 5)
	n := rb.Read(readBuf)
	if n != 0 {
		t.Errorf("Read from empty buffer: expected 0 bytes, got %d", n)
	}
}

func TestFullWrite(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	// Fill the buffer
	data := make([]byte, 8)
	n := rb.Write(data)
	if n != 8 {
		t.Errorf("Write: expected 8 bytes, wrote %d", n)
	}

	// Write to full buffer — should return 0
	n = rb.Write([]byte{1})
	if n != 0 {
		t.Errorf("Write to full buffer: expected 0 bytes, wrote %d", n)
	}
}

func TestAvailableFree(t *testing.T) {
	rb := NewSPSCRingBuffer(16)

	if rb.Available() != 0 {
		t.Errorf("Available on empty: expected 0, got %d", rb.Available())
	}
	if rb.Free() != 16 {
		t.Errorf("Free on empty: expected 16, got %d", rb.Free())
	}

	rb.Write([]byte("hello"))
	if rb.Available() != 5 {
		t.Errorf("Available after write: expected 5, got %d", rb.Available())
	}
	if rb.Free() != 11 {
		t.Errorf("Free after write: expected 11, got %d", rb.Free())
	}

	readBuf := make([]byte, 3)
	rb.Read(readBuf)
	if rb.Available() != 2 {
		t.Errorf("Available after partial read: expected 2, got %d", rb.Available())
	}
	if rb.Free() != 14 {
		t.Errorf("Free after partial read: expected 14, got %d", rb.Free())
	}
}

func TestFull(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	if rb.Full() {
		t.Error("Full on empty buffer: expected false")
	}

	rb.Write(make([]byte, 4))
	if rb.Full() {
		t.Error("Full on half-full buffer: expected false")
	}

	rb.Write(make([]byte, 4))
	if !rb.Full() {
		t.Error("Full on full buffer: expected true")
	}

	readBuf := make([]byte, 1)
	rb.Read(readBuf)
	if rb.Full() {
		t.Error("Full after reading 1 byte: expected false")
	}
}

func TestExactCapacity(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	// Fill to exact capacity
	data := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	n := rb.Write(data)
	if n != 8 {
		t.Fatalf("Write: expected 8, got %d", n)
	}
	if rb.Available() != 8 {
		t.Errorf("Available: expected 8, got %d", rb.Available())
	}
	if rb.Free() != 0 {
		t.Errorf("Free: expected 0, got %d", rb.Free())
	}

	// Read it all back
	readBuf := make([]byte, 8)
	n = rb.Read(readBuf)
	if n != 8 {
		t.Fatalf("Read: expected 8, got %d", n)
	}
	if !bytes.Equal(readBuf, data) {
		t.Errorf("Read: data mismatch")
	}
	if rb.Available() != 0 {
		t.Errorf("Available after full read: expected 0, got %d", rb.Available())
	}
	if rb.Free() != 8 {
		t.Errorf("Free after full read: expected 8, got %d", rb.Free())
	}
}

func TestSingleByte(t *testing.T) {
	rb := NewSPSCRingBuffer(4)

	for i := 0; i < 100; i++ {
		val := byte(i % 256)
		n := rb.Write([]byte{val})
		if n != 1 {
			t.Fatalf("Write byte %d: expected 1, got %d", i, n)
		}

		readBuf := make([]byte, 1)
		n = rb.Read(readBuf)
		if n != 1 {
			t.Fatalf("Read byte %d: expected 1, got %d", i, n)
		}
		if readBuf[0] != val {
			t.Fatalf("Byte %d: expected %d, got %d", i, val, readBuf[0])
		}
	}
}

func TestMultipleWrapArounds(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	// Do many write/read cycles to wrap around the buffer multiple times,
	// stressing the monotonic counter arithmetic.
	for cycle := 0; cycle < 1000; cycle++ {
		data := []byte{byte(cycle % 256), byte((cycle + 1) % 256), byte((cycle + 2) % 256)}
		n := rb.Write(data)
		if n != 3 {
			t.Fatalf("Cycle %d: Write expected 3, got %d", cycle, n)
		}

		readBuf := make([]byte, 3)
		n = rb.Read(readBuf)
		if n != 3 {
			t.Fatalf("Cycle %d: Read expected 3, got %d", cycle, n)
		}
		if !bytes.Equal(readBuf, data) {
			t.Fatalf("Cycle %d: data mismatch: expected %v, got %v", cycle, data, readBuf)
		}
	}
}

func TestWrapAroundDataIntegrity(t *testing.T) {
	// Use a small buffer to force wrap-around on every write
	rb := NewSPSCRingBuffer(8)

	// Move write pointer to position 6 (near end)
	rb.Write([]byte("aaaaaa"))
	tmp := make([]byte, 6)
	rb.Read(tmp)

	// Now write 5 bytes that must wrap: 2 at end + 3 at start
	data := []byte{10, 20, 30, 40, 50}
	n := rb.Write(data)
	if n != 5 {
		t.Fatalf("Write: expected 5, got %d", n)
	}

	readBuf := make([]byte, 5)
	n = rb.Read(readBuf)
	if n != 5 {
		t.Fatalf("Read: expected 5, got %d", n)
	}
	if !bytes.Equal(readBuf, data) {
		t.Errorf("Data integrity after wrap: expected %v, got %v", data, readBuf)
	}
}

func TestConcurrentProducerConsumer(t *testing.T) {
	rb := NewSPSCRingBuffer(1024)

	const iterations = 10000
	const chunkSize = 32

	var wg sync.WaitGroup
	wg.Add(2)

	errCh := make(chan string, 1)

	// Producer goroutine
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			data := make([]byte, chunkSize)
			for j := range data {
				data[j] = byte(i % 256)
			}

			// Retry write if buffer is full
			written := 0
			for written < chunkSize {
				n := rb.Write(data[written:])
				written += n
				if written < chunkSize {
					time.Sleep(time.Microsecond)
				}
			}
		}
	}()

	// Consumer goroutine
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			readBuf := make([]byte, chunkSize)
			read := 0
			for read < chunkSize {
				n := rb.Read(readBuf[read:])
				read += n
				if read < chunkSize {
					time.Sleep(time.Microsecond)
				}
			}

			// Verify data integrity
			expected := byte(i % 256)
			for j := 0; j < chunkSize; j++ {
				if readBuf[j] != expected {
					select {
					case errCh <- "":
					default:
					}
					t.Errorf("Data corruption at iteration %d, byte %d: expected %d, got %d",
						i, j, expected, readBuf[j])
					return
				}
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(10 * time.Second):
		t.Fatal("Test timeout — possible deadlock")
	}
}

func TestConcurrentLargePayload(t *testing.T) {
	// 64KB buffer, 4KB chunks — mimics actual audio use
	rb := NewSPSCRingBuffer(64 * 1024)

	const iterations = 500
	const chunkSize = 4096

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer
	go func() {
		defer wg.Done()
		chunk := make([]byte, chunkSize)
		for i := 0; i < iterations; i++ {
			// Fill chunk with a marker byte per iteration
			for j := range chunk {
				chunk[j] = byte(i % 256)
			}
			written := 0
			for written < chunkSize {
				n := rb.Write(chunk[written:])
				written += n
				if written < chunkSize {
					time.Sleep(10 * time.Microsecond)
				}
			}
		}
	}()

	// Consumer
	go func() {
		defer wg.Done()
		chunk := make([]byte, chunkSize)
		for i := 0; i < iterations; i++ {
			read := 0
			for read < chunkSize {
				n := rb.Read(chunk[read:])
				read += n
				if read < chunkSize {
					time.Sleep(10 * time.Microsecond)
				}
			}

			expected := byte(i % 256)
			for j := 0; j < chunkSize; j++ {
				if chunk[j] != expected {
					t.Errorf("Large payload corruption at iteration %d, byte %d: expected %d, got %d",
						i, j, expected, chunk[j])
					return
				}
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Test timeout — possible deadlock")
	}
}

func TestAudioSimulation(t *testing.T) {
	// Simulate audio scenario: 44.1kHz, 16-bit stereo = 176,400 bytes/sec
	// Ring buffer holds ~250ms = 44,100 bytes, rounds up to 65,536
	rb := NewSPSCRingBuffer(44100)

	const sampleRate = 44100
	const channels = 2
	const bytesPerSample = 2
	const callbackMs = 10 // 10ms chunks (like framesPerBuffer=441 at 44.1kHz)
	const callbacks = 100 // 1 second of audio

	chunkSize := (sampleRate * channels * bytesPerSample * callbackMs) / 1000

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer: file reader goroutine
	go func() {
		defer wg.Done()
		chunk := make([]byte, chunkSize)
		for i := 0; i < callbacks; i++ {
			for j := range chunk {
				chunk[j] = byte((i + j) % 256)
			}

			written := 0
			for written < len(chunk) {
				n := rb.Write(chunk[written:])
				written += n
				if written < len(chunk) {
					time.Sleep(time.Millisecond)
				}
			}
		}
	}()

	// Consumer: audio callback
	go func() {
		defer wg.Done()
		readBuf := make([]byte, chunkSize)
		for i := 0; i < callbacks; i++ {
			read := 0
			for read < chunkSize {
				n := rb.Read(readBuf[read:])
				read += n
				if read < chunkSize {
					time.Sleep(time.Millisecond)
				}
			}

			// Verify first byte of each chunk
			expected := byte((i) % 256)
			if readBuf[0] != expected {
				t.Errorf("Audio chunk %d: expected first byte %d, got %d", i, expected, readBuf[0])
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Audio simulation timeout")
	}
}

func TestAvailableFreeConsistency(t *testing.T) {
	rb := NewSPSCRingBuffer(16)

	// At all times: Available + Free == capacity
	capacity := 16
	check := func(label string) {
		a := rb.Available()
		f := rb.Free()
		if a+f != capacity {
			t.Errorf("%s: Available(%d) + Free(%d) = %d, expected %d", label, a, f, a+f, capacity)
		}
	}

	check("empty")

	rb.Write([]byte("test"))
	check("after write 4")

	tmp := make([]byte, 2)
	rb.Read(tmp)
	check("after read 2")

	rb.Write([]byte("more data here"))
	check("after write 14 (wraps)")

	tmp = make([]byte, 16)
	rb.Read(tmp)
	check("after drain")
}

func TestZeroLengthOperations(t *testing.T) {
	rb := NewSPSCRingBuffer(8)

	// Write zero bytes
	n := rb.Write([]byte{})
	if n != 0 {
		t.Errorf("Write(empty): expected 0, got %d", n)
	}

	// Read zero bytes
	n = rb.Read([]byte{})
	if n != 0 {
		t.Errorf("Read(empty): expected 0, got %d", n)
	}

	// Buffer should still be empty
	if rb.Available() != 0 {
		t.Errorf("Available after zero ops: expected 0, got %d", rb.Available())
	}
}

// Benchmarks

func BenchmarkWrite(b *testing.B) {
	rb := NewSPSCRingBuffer(64 * 1024)
	data := make([]byte, 256)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rb.Free() < len(data) {
			// Drain to make space
			tmp := make([]byte, rb.Available())
			rb.Read(tmp)
		}
		rb.Write(data)
	}
}

func BenchmarkRead(b *testing.B) {
	rb := NewSPSCRingBuffer(64 * 1024)
	data := make([]byte, 256)

	// Pre-fill
	for rb.Free() >= len(data) {
		rb.Write(data)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rb.Available() < len(data) {
			for rb.Free() >= len(data) {
				rb.Write(data)
			}
		}
		rb.Read(data)
	}
}

func BenchmarkConcurrentReadWrite(b *testing.B) {
	rb := NewSPSCRingBuffer(64 * 1024)
	data := make([]byte, 256)

	b.ResetTimer()

	var wg sync.WaitGroup
	wg.Add(2)

	half := b.N / 2

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < half; i++ {
			for rb.Free() < len(data) {
				// spin
			}
			rb.Write(data)
		}
	}()

	// Consumer
	go func() {
		defer wg.Done()
		buf := make([]byte, 256)
		for i := 0; i < half; i++ {
			for rb.Available() < len(buf) {
				// spin
			}
			rb.Read(buf)
		}
	}()

	wg.Wait()
}

func BenchmarkWriteSmall(b *testing.B) {
	rb := NewSPSCRingBuffer(64 * 1024)
	data := make([]byte, 4) // Tiny writes like the producer uses

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rb.Free() < len(data) {
			tmp := make([]byte, rb.Available())
			rb.Read(tmp)
		}
		rb.Write(data)
	}
}

func BenchmarkReadAudioFrame(b *testing.B) {
	// Simulate reading one audio callback worth of data
	// 512 frames * 2 channels * 2 bytes = 2048 bytes
	rb := NewSPSCRingBuffer(64 * 1024)
	frameSize := 512 * 2 * 2
	data := make([]byte, frameSize)

	// Pre-fill
	for rb.Free() >= len(data) {
		rb.Write(data)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rb.Available() < len(data) {
			for rb.Free() >= len(data) {
				rb.Write(data)
			}
		}
		rb.Read(data)
	}
}
