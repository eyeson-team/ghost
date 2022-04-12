
.PHONY: default
default: build

.PHONY: build
build:
	@go build -o bin/rtmp-server rtmp-server

.PHONY: platforms
platforms:
	CGO_ENABLED=0 gox -output="bin/{{.OS}}_{{.Arch}}/rtmp-server" -ldflags="-w -s" -osarch="linux/amd64 darwin/amd64 windows/amd64" rtmp-server
	upx bin/*/rtmp-server
