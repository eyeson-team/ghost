# eyeson Ghost RTSP-Client

A small test example on connecting to an RTSP-Server (webcam, etc.)
and injecting that stream via webrtc in an eyeson meeting.

## Usage

```sh
$ ./rtmp-client
Usage:
  rtsp-client [flags] $API_KEY|$GUEST_LINK RTSP_CONNECT_URL

Flags:
      --api-endpoint string   Set api-endpoint (default "https://api.eyeson.team")
  -h, --help                  help for rtsp-client
      --room-id string        Room ID. If left empty, a new meeting will be created on each request
      --user string           User name to use (default "rtsp-test")
  -v, --verbose               verbose output
```

Start this RTSP-Client example by either providing an api key that starts a new meeting
or a guest link to join an existing one.

```sh
$ export API_KEY=<...>
$ ./rtmp-client $API_KEY|$GUEST_LINK RTSP_CONNECT_URL
```

In order to have an RTSP-Server for testing use vlc to make a webcam
available via RTSP:

```sh
vlc v4l2:///dev/video0 --sout '#transcode{vcodec=h264,acodec=mpga,ab=128,channels=2,samplerate=44100,scodec=none}:rtp{sdp=rtsp://:8554/stream}' \
--sout-x264-bframes=0 --sout-x264-keyint=2
```


## Development

```sh
make [build] # build the project
make platforms # build platform specific executeable
```
