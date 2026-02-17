.PHONY: build test lint vet clean

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

clean:
	go clean ./...
	rm -f play_raw play_raw_ringbuffer play_raw_lockfree play_raw_stream record_audio play_sine play_sine_callback audio_enum
