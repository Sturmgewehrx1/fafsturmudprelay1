.PHONY: all build test bench clean server server-linux adapter stress wrapper

GOOS     ?= windows
GOARCH   ?= amd64
LDFLAGS  := -s -w
GOFLAGS  := -trimpath -ldflags "$(LDFLAGS)"

all: build

build: server server-linux adapter stress wrapper

server:
	@mkdir -p build/server
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -o build/server/faf-sturm-relay-server.exe ./cmd/relay-server/

server-linux:
	@mkdir -p build/server_linux
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o build/server_linux/faf-sturm-relay-server ./cmd/relay-server/

adapter:
	@mkdir -p build/client
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -o build/client/faf-sturm-relay-adapter.exe ./cmd/relay-adapter/

stress:
	@mkdir -p build/server
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -o build/server/faf-sturm-server-stress-test.exe ./cmd/stress-test/


wrapper:
	@if [ -f wrapper/faf-ice-adapter.jar ]; then \
		mkdir -p build/client && \
		cp wrapper/faf-ice-adapter.jar build/client/faf-ice-adapter.jar && \
		echo "Copied faf-ice-adapter.jar to build/client/"; \
	fi

test:
	go test ./... -v -race -count=1

bench:
	go test ./internal/protocol/ -bench=. -benchmem
	go test ./internal/relay/ -bench=. -benchmem

clean:
	rm -rf build/ bin/
