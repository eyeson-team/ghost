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
  rtsp-client --check rtsp://cam.local:554/stream
  rtsp-client --check --h265 rtsp://user:pass@cam.local/stream

Flags:
      --api-endpoint string   Set api-endpoint (default "https://api.eyeson.team")
      --check                 Only check the RTSP source (reachability, audio/video data) without joining a meeting
      --custom-ca string      custom CA file
      --h265                  If true, expect h265 instead of h264
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

## Check mode

`--check` only tests the RTSP side and never connects to eyeson, so no
api key or guest link is needed:

```sh
$ ./rtsp-client --check RTSP_CONNECT_URL
$ ./rtsp-client --check --h265 rtsp://user:pass@cam.local/stream
```

It checks that the host/port is reachable, runs the RTSP handshake
(OPTIONS/DESCRIBE/SETUP/PLAY) and lists the described tracks. It then receives
the stream and finishes on its own as soon as every track delivered data and
the video track has a keyframe (after at least 1s of video, so frame rate and
bitrate are meaningful). If that does not happen within 15s, the check fails.
Either way it ends with a short state and summary, e.g.:

```
== Result
state:   OK
url:     rtsp://cam.local:554/stream
source:  reachable, server gortsplib
video:   H264 1280x720, ~25 fps, ~266 kbit/s
audio:   Opus, ~80 kbit/s
took:    1.86s
```

`--h265` selects which codec is expected, as in normal mode.

The report is written to stdout (`-q` silences only the log output, `-v` adds
the RTSP requests/responses). The exit code is `0` if the stream is usable
for forwarding (possibly with warnings) and `1` otherwise. The api key /
guest link argument may still be passed, it is ignored in check mode.

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