VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BINARY   := cap_browser
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"

.PHONY: build install clean tidy

build: tidy
	go build $(LDFLAGS) -o $(BINARY) .

install: tidy
	go install $(LDFLAGS) .

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
