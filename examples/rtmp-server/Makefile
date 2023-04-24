
.PHONY: default
default: build

.PHONY: build
build:
	@go build -o bin/rtmp-server rtmp-server

.PHONY: build-platforms
build-platforms:
	# build mac without upx on arm64 cause it would crash
	# using ldflags "-s -w" to remove debug info
	# using upx to compress the binary
	OOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o bin/rtmp-server_darwin_arm64 rtmp-server
	OOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o bin/rtmp-server_darwin_amd64 rtmp-server && upx --best --lzma bin/rtmp-server_darwin_amd64
	OOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/rtmp-server_linux_amd64 rtmp-server && upx --best --lzma bin/rtmp-server_linux_amd64
	OOS=linux GOARCH=arm go build -ldflags="-s -w" -o bin/rtmp-server_linux_arm rtmp-server && upx --best --lzma bin/rtmp-server_linux_arm
	OOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/rtmp-server_linux_arm64 rtmp-server && upx --best --lzma bin/rtmp-server_linux_arm64
	OOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/rtmp-server_windows_amd64 rtmp-server && upx --best --lzma bin/rtmp-server_windows_amd64


build-alpine:
	CGO_ENABLED=0 go build -a -installsuffix cgo -o bin/rtmp-server rtmp-server
