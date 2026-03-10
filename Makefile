.PHONY: build test lint clean all

build:
	go build -trimpath -o radb ./cmd/radb

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -f radb radb.exe
	rm -f radb-agent radb-agent.exe radb-signal radb-signal.exe

all: lint test build
