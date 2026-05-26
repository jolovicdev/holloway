BINDIR := bin
SERVER_PKG := ./cmd/holloway-server
CLIENT_PKG := ./cmd/holloway

.PHONY: all test build build-local build-linux build-darwin build-windows clean

all: test build

test:
	go test ./...

build: build-linux build-darwin build-windows

build-local:
	go build -o $(BINDIR)/holloway-server $(SERVER_PKG)
	go build -o $(BINDIR)/holloway $(CLIENT_PKG)

build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BINDIR)/linux-amd64/holloway-server $(SERVER_PKG)
	GOOS=linux GOARCH=amd64 go build -o $(BINDIR)/linux-amd64/holloway $(CLIENT_PKG)
	GOOS=linux GOARCH=arm64 go build -o $(BINDIR)/linux-arm64/holloway-server $(SERVER_PKG)
	GOOS=linux GOARCH=arm64 go build -o $(BINDIR)/linux-arm64/holloway $(CLIENT_PKG)

build-darwin:
	GOOS=darwin GOARCH=amd64 go build -o $(BINDIR)/darwin-amd64/holloway-server $(SERVER_PKG)
	GOOS=darwin GOARCH=amd64 go build -o $(BINDIR)/darwin-amd64/holloway $(CLIENT_PKG)
	GOOS=darwin GOARCH=arm64 go build -o $(BINDIR)/darwin-arm64/holloway-server $(SERVER_PKG)
	GOOS=darwin GOARCH=arm64 go build -o $(BINDIR)/darwin-arm64/holloway $(CLIENT_PKG)

build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BINDIR)/windows-amd64/holloway-server.exe $(SERVER_PKG)
	GOOS=windows GOARCH=amd64 go build -o $(BINDIR)/windows-amd64/holloway.exe $(CLIENT_PKG)
	GOOS=windows GOARCH=arm64 go build -o $(BINDIR)/windows-arm64/holloway-server.exe $(SERVER_PKG)
	GOOS=windows GOARCH=arm64 go build -o $(BINDIR)/windows-arm64/holloway.exe $(CLIENT_PKG)

clean:
	rm -rf $(BINDIR)
