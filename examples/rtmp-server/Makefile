
.PHONY: default
default: build

.PHONY: build
build:
	@go build -o bin/rtmp-server rtmp-server

.PHONY: platforms
platforms:
	CGO_ENABLED=0 gox -output="bin/{{.OS}}_{{.Arch}}/rtmp-server_{{.OS}}_{{.Arch}}" -ldflags="-w -s" -osarch="linux/amd64 linux/arm linux/arm64 darwin/amd64 darwin/arm64 windows/amd64" rtmp-server
	upx bin/*/rtmp-server_*

.PHONY: platforms-via-container
platforms-via-container:
	podman run --rm -it -v `pwd`/.:/build:z go-builder sh -c "cd /build &&  make platforms"

