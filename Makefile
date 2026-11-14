BINARY := directcrate
DIST_DIR := dist
LDFLAGS := -s -w

.PHONY: build build-all clean icons test test-all

icons:
	go run scripts/gen_resources.go

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test -v -race ./...

test-all:
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

build-all: icons
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe .
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-android-arm64 .

clean:
	rm -rf $(DIST_DIR) $(BINARY)
