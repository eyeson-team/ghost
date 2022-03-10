# eyeson Ghost RTMP-Server

A small test example using a local rtmp-server and injecting that stream via
webrtc.

## Usage

```sh
$ ./rtmp-server
Usage:
  rtmp-server [flags] $API_KEY

Flags:
      --api-endpoint string       Set api-endpoint (default "https://api.eyeson.team")
  -h, --help                      help for rtmp-server
      --room-id string            Room ID. If left empty, a new room will be created on each request
      --rtmp-listen-addr string   rtmp address this server shall listen for (default "rtmp://127.0.0.1:1935")
      --user string               User name to use (default "rtmp-test")
```

Start an RTMP server by either providing an api key that starts a new meeting
or a guest link to join an existing one.

```sh
$ export API_KEY=<...>
$ ./rtmp-server $API_KEY|$GUEST_LINK --room-id rtmp-test
```

Test the RTMP using a stream build with `ffmpeg`.

```sh
ffmpeg -re -i https://jell.yfish.us/media/jellyfish-3-mbps-hd-h264.mkv \
  -vcodec libx264 -preset veryfast -g 30 -r 30 -f flv rtmp://127.0.0.1:1935
```

## Development

The used libary [aler9/gortsplib)(https://github.com/aler9/gortsplib) use an
rtpPayloadSize of 1460 which will exceed the MTU of lots of ISPs. To fix this,
the Makefile target `vendor` will vendor and patch this library.

```sh
make vendor # fetch dependencies
make [build] # build the project
make platforms # build platform specific executeable
```
