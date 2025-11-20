

package=rtsp-client
binprefix=rtsp-client

build_n_pack() {
	# using ldflags "-s -w" to remove debug info
	# using upx to compress the binary
  os=$1
  arch=$2
  GOOS=$1 GOARCH=$arch go build -ldflags="-s -w
     -X 'main.Version=$(git describe --tags --always --dirty)' \
    -X 'main.Commit=$(git rev-parse --short HEAD)' \
    -X 'main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)'
  " \
    -o bin/${binprefix}_${os}_${arch} $package \
    && upx --best --lzma bin/${binprefix}_${os}_${arch}
}

build() {
  os=$1
  arch=$2
  GOOS=$os GOARCH=$arch go build -ldflags="-s -w" \
    -o bin/${binprefix}_${os}_${arch} $package
}

$@


