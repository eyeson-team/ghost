.PHONY: default
default: build

.PHONY: build
build:
	@go build -o bin/rtsp-client main.go

build-cgo:
	CGO_ENABLED=0 go build  -a -installsuffix cgo -o bin/rtsp-client *.go
	#CGO_ENABLED=0 go build -mod=vendor  -a -installsuffix cgo -o bin/rtsp-client *.go


.PHONY: build-platforms
build-platforms:
	# build mac without upx on arm64 cause it would crash
	# using ldflags "-s -w" to remove debug info
	# using upx to compress the binary
	OOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o bin/rtsp-client_darwin_arm64 rtsp-client
	OOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o bin/rtsp-client_darwin_amd64 rtsp-client && upx --best --lzma bin/rtsp-client_darwin_amd64
	OOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/rtsp-client_linux_amd64 rtsp-client && upx --best --lzma bin/rtsp-client_linux_amd64
	OOS=linux GOARCH=arm go build -ldflags="-s -w" -o bin/rtsp-client_linux_arm rtsp-client && upx --best --lzma bin/rtsp-client_linux_arm
	OOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/rtsp-client_linux_arm64 rtsp-client && upx --best --lzma bin/rtsp-client_linux_arm64
	OOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/rtsp-client_windows_amd64 rtsp-client && upx --best --lzma bin/rtsp-client_windows_amd64

