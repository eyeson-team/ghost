# eyeson Ghost RTMP-Server

A small test example using a local rtmp-server and injecting that stream via
webrtc.

## Usage

```sh
$ ./rtmp-server
Usage:
  rtmp-server [flags] $API_KEY|$GUEST_LINK

Flags:
      --api-endpoint string       Set api-endpoint (default "https://api.eyeson.team")
  -h, --help                      help for rtmp-server
      --room-id string            Room ID. If left empty, a new room will be created on each request
      --rtmp-listen-addr string   rtmp address this server shall listen for (default "rtmp://127.0.0.1:1935")
      --user string               User name to use (default "rtmp-test")
      --dry-run                   don't join a meeting, only run the rtmp server and show incoming connections and stream stats
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

## Dry-run

To test an RTMP client (OBS, ffmpeg, a hardware encoder, ...) without joining a
meeting, start the server with `--dry-run`. No api key or guest link is needed.

```sh
$ ./rtmp-server --dry-run
INF DRY-RUN: not connecting to any meeting
INF RTMP server listening on port 1935, reachable via:
INF     rtmp://127.0.0.1:1935   (lo)
INF     rtmp://192.168.1.23:1935   (eth0)
INF STATE: waiting for incoming RTMP connection ... (ctrl-c to quit)
```

All IPv4 addresses of the active interfaces are listed when listening on
`0.0.0.0`. For every client the server shows the connection state (connected,
publishing with stream path, receiving, stalled, disconnected), the H264 and
AAC stream parameters, and every 2 seconds the received video/audio packets,
keyframes, bitrate and frame rate. When the client disconnects a summary is
printed and the server waits for the next connection. Stop it with ctrl-c.

## Development

```sh
make [build] # build the project
make platforms # build platform specific executeable
```