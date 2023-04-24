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
	bash build.sh build darwin arm64
	bash build.sh build_n_pack darwin amd64
	bash build.sh build_n_pack windows amd64
	bash build.sh build_n_pack linux amd64
	bash build.sh build_n_pack linux arm
	bash build.sh build_n_pack linux arm64

