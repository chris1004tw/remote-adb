.PHONY: build test lint clean all

build:
	go build ./cmd/...

test:
	go test -race ./...

lint:
	golangci-lint run

clean:
	rm -f radb radb-agent radb-signal
	rm -f radb.exe radb-agent.exe radb-signal.exe

all: lint test build
