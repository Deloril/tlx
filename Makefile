# Timeline explorer build targets.
#
# The engine (internal/model) is pure Go and needs no C toolchain. The GUI
# (internal/gui) uses Fyne, which needs cgo and the platform's GL/windowing
# headers. See README.md for the per-OS dependency list.

BIN := tlx
PKG := ./cmd/tlx

.PHONY: all build run test vet fmt engine-test clean mac mac-universal windows

all: build

# Native build for the current OS/arch.
build:
	CGO_ENABLED=1 go build -ldflags "-s -w" -o $(BIN) $(PKG)

run: build
	./$(BIN) $(FILE)

# Engine tests only — no display or C toolchain required.
engine-test:
	CGO_ENABLED=0 go test ./internal/model/...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

# --- macOS ---------------------------------------------------------------
# Run these ON a Mac (with Xcode command line tools). Cross-compiling a cgo GUI
# from Linux is not supported without osxcross plus the Apple SDK.
mac:
	CGO_ENABLED=1 go build -ldflags "-s -w" -o $(BIN)-darwin $(PKG)

# Universal (arm64 + amd64) binary, also Mac-only.
mac-universal:
	CGO_ENABLED=1 GOARCH=arm64 go build -ldflags "-s -w" -o $(BIN)-arm64 $(PKG)
	CGO_ENABLED=1 GOARCH=amd64 go build -ldflags "-s -w" -o $(BIN)-amd64 $(PKG)
	lipo -create -output $(BIN)-darwin $(BIN)-arm64 $(BIN)-amd64
	rm -f $(BIN)-arm64 $(BIN)-amd64

# --- Windows -------------------------------------------------------------
# Run on Windows (MSYS2/MinGW) or cross-compile with a mingw-w64 toolchain.
windows:
	CGO_ENABLED=1 GOOS=windows go build -ldflags "-s -w -H windowsgui" -o $(BIN).exe $(PKG)

clean:
	rm -f $(BIN) $(BIN).exe $(BIN)-darwin $(BIN)-arm64 $(BIN)-amd64
