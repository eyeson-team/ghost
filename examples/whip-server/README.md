# eyeson Ghost WHIP-Server

A WHIP ingest endpoint that forwards a WebRTC stream into an eyeson meeting.

```
OBS / GStreamer / browser ──WHIP──▶ whip-server ──ghost──▶ eyeson meeting
```

WHIP ([WebRTC-HTTP Ingestion Protocol, RFC 9725](https://datatracker.ietf.org/doc/rfc9725/))
is a small HTTP handshake in front of a plain WebRTC connection: the sender posts
an SDP offer, the server answers, and from then on it is just WebRTC.

The incoming media already arrives as RTP in a codec the meeting understands, so
the packets are passed straight through. No decoding, no re-encoding, no quality
loss, and very little CPU.

## Quick start

Start the server with an API key, which creates or joins a meeting:

```sh
$ export API_KEY=<your api key>
$ ./whip-server $API_KEY --room-id whip-test --bearer-token s3cr3t
```

A guest link works as well, if you want to publish into a meeting that already
exists:

```sh
$ ./whip-server "https://app.eyeson.team/?guest=<token>"
```

The server prints the links to the meeting and opens the endpoint:

```
Guest-link: https://app.eyeson.team/?guest=...
GUI-link: https://app.eyeson.team/...
WHIP endpoint listening on http://:8100/whip
Waiting for a WHIP sender, accepted video codecs: vp9,av1,vp8,h264,h265
```

Open the guest link in a browser, then point your sender at
`http://<host>:8100/whip` and start streaming.

## Sending to it

### OBS Studio

WHIP output is available since OBS 30. In **Settings → Stream**:

| Field        | Value                                                          |
| ------------ | -------------------------------------------------------------- |
| Service      | `WHIP`                                                          |
| Server       | `http://<host>:8100/whip`                                       |
| Bearer Token | whatever you passed to `--bearer-token` (leave empty if unset)  |

In **Settings → Output**, switch to *Advanced* and set:

| Setting            | Value | Why                                                    |
| ------------------ | ----- | ------------------------------------------------------ |
| B-frames           | `0`   | the eyeson media server does not support B-frames      |
| Keyframe interval  | `1` s | new participants need a keyframe to start rendering    |
| Total layers       | `1`   | simulcast layers are not used and only cost bandwidth  |

The B-frame setting is called *B-frames* for x264, *Max B-frames* for NVENC, and
*B-Frames* for the Apple and AMD encoders. Leave it at `0` for all of them.

OBS offers H264, HEVC and AV1 for WHIP, and all three are accepted out of the
box. Audio is Opus, which OBS selects automatically.

### GStreamer

VP9 in, VP9 all the way into the meeting:

```sh
gst-launch-1.0 \
  videotestsrc is-live=true ! videoconvert ! vp9enc deadline=1 keyframe-max-dist=60 ! \
  rtpvp9pay ! 'application/x-rtp,media=video,encoding-name=VP9,payload=98,clock-rate=90000' ! whip.sink_0 \
  audiotestsrc is-live=true ! audioconvert ! opusenc ! rtpopuspay ! \
  'application/x-rtp,media=audio,encoding-name=OPUS,payload=111,clock-rate=48000,encoding-params=(string)2' ! whip.sink_1 \
  whipsink name=whip whip-endpoint=http://127.0.0.1:8100/whip auth-token=s3cr3t
```

### Anything else

Any WHIP client works: `ffmpeg -f whip`, Broadcast Box, or a few lines of browser
JavaScript. A browser is the easiest way to send VP9 or AV1.

When you encode H264 or H265 yourself, turn B-frames off there too, e.g. `-bf 0`
in ffmpeg or `bframes=0` on the GStreamer `x264enc`.

## Usage hints

* **Turn off B-frames.** The eyeson media server does not support them. This
  applies to H264 and H265; VP8, VP9 and AV1 do not use B-frames in WebRTC.
* **Join the meeting before you start streaming.** A meeting with nobody in it
  shuts down after a short while, and this server only joins once a sender
  publishes.
* **Keep the keyframe interval short**, one or two seconds. Participants who join
  later see video as soon as the next keyframe arrives. `--pli-interval` also
  asks the sender for one every few seconds.
* **Restart the stream after you change the codec.** The video codec is agreed on
  when the sender connects, so a new encoder setting only takes effect on the
  next publish. Changing it makes the participant briefly rejoin the meeting.
* **Give it a moment on the first publish.** The connection into the meeting is
  established while your sender is already connecting, so the first video frames
  reach the meeting a second or two after the sender reports success.
* **Set bitrate and resolution in the sender.** Whatever it produces is what the
  meeting gets, unchanged.
* **One sender at a time.** A new publish takes over and closes the previous one,
  which is what you want when a sender crashes and reconnects.
* **Send a single layer.** Simulcast layers beyond the first are ignored.
* **Fix the room** with `--room-id` if you want every run to land in the same
  meeting, and protect the endpoint with `--bearer-token`.

## Codecs

The video codec is not fixed, it is agreed on with the sender. The first codec
from `--video-codecs` that the sender can also produce wins:

```
--video-codecs vp9,av1,vp8,h264,h265   (default)
```

The server preference comes first, so a browser that would rather send H264 but
can do VP9 is answered with VP9. Accepted names are `vp9`, `av1`, `vp8`, `h264`
and `h265`. Senders that announce H265 as `HEVC` are understood as well.

VP9 is used in profile 0, the profile every VP9 sender supports. Senders that
offer several profiles are pinned to it automatically.

Audio is always Opus. Every WebRTC sender offers it, so there is nothing to
configure. If a sender offers no Opus at all, the session comes up video only.

## Network setup

The sender connects to this server, so this server has to be reachable. On a
local network that works out of the box. Two cases need a flag:

* **Cloud VM** (AWS, GCP, Hetzner and friends): the machine only knows its
  private address, so tell it the public one with `--public-ip 1.2.3.4`.
* **Firewall**: `--udp-port-range 50000-50100` keeps the media on a range you can
  open.

```sh
$ ./whip-server $API_KEY --public-ip 1.2.3.4 --udp-port-range 50000-50100
```

By default the STUN and TURN servers of the eyeson meeting are used, which is the
right choice in almost every setup. They can be replaced, with credentials given
inline:

```sh
--ice-servers stun:stun.l.google.com:19302
--ice-servers turn:user:pass@turn.example.com:3478?transport=udp,stun:stun.example.com:3478
--ice-servers none     # server is directly reachable
```

`--advertise-ice` additionally offers these servers to the sender. It is off by
default, because senders differ in how well they handle it.

To serve the endpoint over https, pass `--tls-cert` and `--tls-key`.

## All options

```sh
$ ./whip-server --help
Usage:
  whip-server [flags] $API_KEY|$GUEST_LINK

Flags:
      --advertise-ice             send the ice servers to senders via WHIP Link headers
      --api-endpoint string       Set api-endpoint (default "https://api.eyeson.team")
      --bearer-token string       if set, senders must provide this token as an Authorization: Bearer header
      --custom-ca string          custom CA file
      --exit-on-disconnect        terminate the meeting when the WHIP sender disconnects
      --ice-servers string        comma separated ice servers, e.g. turn:user:pass@host:3478. defaults to the ones the eyeson api returns, "none" disables them
      --insecure                  if true don't verify remote tls certificates
      --no-audio                  do not forward the audio track
      --pli-interval int32        interval in ms to request a keyframe from the WHIP sender, 0 disables it (default 3000)
      --public-ip string          public ip to use in host candidates, for servers behind 1:1 NAT
  -q, --quiet                     no logging output
      --room-id string            Room ID. If left empty, a new meeting will be created on each request
      --tls-cert string           certificate file to serve the WHIP endpoint via https
      --tls-key string            key file to serve the WHIP endpoint via https
      --trace                     everything --verbose has, plus the exchanged sdp and the data channel messages
      --udp-port-range string     restrict ice to a udp port range, e.g. 50000-50100
      --user string               User name to use (default "whip-test")
      --user-id string            User id to use
  -v, --verbose                   per session detail: timings, dropped packets, ice gathering
      --video-codecs string       accepted video codecs, most preferred first (default "vp9,av1,vp8,h264,h265")
      --whip-listen-addr string   address the WHIP endpoint listens on (default ":8100")
      --whip-path string          http path of the WHIP endpoint (default "/whip")
      --widescreen                start room in widescreen mode (default true)
```

## Endpoint

| Method    | Path         | Meaning                                                      |
| --------- | ------------ | ------------------------------------------------------------ |
| `POST`    | `/whip`      | publish, body is the SDP offer, answers `201` with a `Location` header |
| `OPTIONS` | `/whip`      | CORS preflight, advertises the STUN servers via `Link` headers |
| `DELETE`  | `/whip/<id>` | stop publishing                                               |
| `PATCH`   | `/whip/<id>` | `405`, all candidates are already in the answer               |

`POST` answers `415` when the sender offers no usable codec, and `503` when the
meeting could not be joined.

## Troubleshooting

Run with `-v` first, the log usually points straight at the cause. `-v` shows the
detail of each session, `--trace` adds the SDP that was exchanged with the
sender.

**Nothing connects from another machine.** The sender dials this server, so this
server's addresses have to be reachable: `--public-ip` on a cloud VM,
`--udp-port-range` plus a firewall rule, or a TURN server.

**The sender connects but the meeting stays empty.** Check the log for
`WHIP track received` and the codec that was picked. If your sender uses
simulcast, set it to a single layer.

**Participants who join later see a black tile.** Shorten the keyframe interval
in the sender, or lower `--pli-interval`.

**Video stutters or shows artefacts.** Make sure B-frames are turned off in the
encoder.

**The meeting ends on its own.** Join it with the guest link before streaming, an
empty meeting closes after a short while.

## Development

```sh
go mod tidy    # fetch dependencies and create go.sum
make           # build into bin/whip-server
make build-platforms
```

The tests need neither an API key nor network access.

| File        | What is in it                                               |
| ----------- | ----------------------------------------------------------- |
| `main.go`   | cli, room handling, wiring                                   |
| `codecs.go` | codec table, SDP offer parsing, preference resolution        |
| `eyeson.go` | the ghost client lifecycle                                   |
| `ice.go`    | stun/turn config, NAT and port range settings, Link headers  |
| `whip.go`   | the WHIP http endpoint and the RTP forwarding                |

One publish runs as: parse the offer, pick the codec, connect ghost with that
codec, answer the sender, then copy the incoming RTP packets onto the ghost
tracks.