

package=rtmp-server
binprefix=rtmp-server

build_n_pack() {
	# using ldflags "-s -w" to remove debug info
	# using upx to compress the binary
  os=$1
  arch=$2
  GOOS=$1 GOARCH=$arch go build -ldflags="-s -w" \
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


