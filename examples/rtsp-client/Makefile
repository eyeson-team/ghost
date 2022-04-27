.PHONY: default
default: build

.PHONY: build
build:
	@go build -o bin/rtsp-client main.go

build-cgo:
	CGO_ENABLED=0 go build  -a -installsuffix cgo -o bin/rtsp-client *.go
	#CGO_ENABLED=0 go build -mod=vendor  -a -installsuffix cgo -o bin/rtsp-client *.go


.PHONY: platforms
platforms:
	CGO_ENABLED=0 gox -output="bin/{{.OS}}_{{.Arch}}/rtsp-client_{{.OS}}_{{.Arch}}" \
							-ldflags="-w -s" \
							-osarch="linux/amd64 linux/arm linux/arm64 darwin/amd64 darwin/arm64 windows/amd64" rtsp-client
	upx bin/*/rtsp-client_*

# Note, that an image go-builder is required which contains
# gox and upx.
.PHONY: platforms-via-container
platforms-via-container:
	podman run --rm -it -v `pwd`/.:/build:z go-builder sh -c "cd /build &&  make platforms"


