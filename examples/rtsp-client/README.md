# eyeson Ghost RTSP-Client

A small test example on connecting to an RTSP-Server (IP-Cam, etc.)
and injecting that stream via webrtc in an eyeson meeting.

## Usage

```sh
$ ./rtsp-client --help
Usage:
  rtsp-client [flags] $API_KEY|$GUEST_LINK RTSP_CONNECT_URL

Examples:
  rtsp-client $API_KEY rtsp://cam.local:554/stream
  rtsp-client $GUEST_LINK rtsp://user:pass@cam.local/stream

Flags:
      --api-endpoint string   Set api-endpoint (default "https://api.eyeson.team")
      --custom-ca string      custom CA file
  -h, --help                  help for rtsp-client
      --insecure              if true don't verify remote tls certificates
      --passthrough           if true just passthrough all H264 NAL-Units
  -q, --quiet                 no logging output
      --room-id string        Room ID. If left empty, a new meeting will be created on each request
      --trace                 trace output
      --user string           User name to use (default "rtsp-test")
      --user-id string        User id to use
  -v, --verbose               verbose output
      --version               version for rtsp-client
      --widescreen            start room in widescreen mode (default true)
```

Start this RTSP-Client example by either providing an api key that starts a new meeting
or a guest link to join an existing one.

```sh
$ export API_KEY=<...>
$ ./rtsp-client $API_KEY|$GUEST_LINK RTSP_CONNECT_URL
```

The RTSP source is asked for its tracks before the meeting is joined, and the
meeting connection is set up with the matching video codec. H264 and H265 are
supported (H264 is used if the source offers both). Audio is not forwarded.

In order to have an RTSP-Server for testing use vlc to make a webcam
available via RTSP:

```sh
vlc v4l2:///dev/video0 --sout '#transcode{vcodec=h264{bframes=0},acodec=mpga,ab=128,channels=2,samplerate=44100,scodec=none}:rtp{sdp=rtsp://:8554/stream}'
```


## Development

```sh
make [build] # build the project
make platforms # build platform specific executeable
```