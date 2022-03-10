
.PHONY: default
default: build

.PHONY: vendor
vendor:
	@go mod vendor

.PHONY: build
build:
	@go build -mod=vendor -o bin/rtmp-server main.go

.PHONY: platforms
platforms:
	CGO_ENABLED=0 gox -mod=vendor -output="bin/{{.OS}}_{{.Arch}}/rtmp-server" -ldflags="-w -s" -osarch="linux/amd64 darwin/amd64 windows/amd64" rtmp-server
	upx bin/*/rtmp-server
