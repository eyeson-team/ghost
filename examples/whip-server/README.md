# eyeson Ghost WHIP-Server

A WHIP ingest endpoint that forwards a WebRTC stream into an eyeson meeting.

```
OBS / GStreamer / browser ──WHIP──▶ whip-server ──ghost (SEPP + WebRTC)──▶ eyeson meeting
```

WHIP ([WebRTC-HTTP Ingestion Protocol, RFC 9725](https://datatracker.ietf.org/doc/rfc9725/))
is a very small HTTP handshake in front of a plain WebRTC connection: the sender
POSTs an SDP offer, the server answers with an SDP answer, and from then on it is
just WebRTC. That is the whole protocol - there is no WHIP media format, no WHIP
packetisation.

That is what makes this example cheap: the incoming media is already RTP with
codecs the meeting server understands. The packets are handed straight to the
ghost tracks - no decoding, no re-encoding, no jitter buffer. CPU usage is close
to a memcpy.

## Codecs

The video codec is negotiated with the sender rather than hardcoded. On a
`POST`, the offer is parsed, the first codec from `--video-codecs` that the
sender also offers wins, and the eyeson connection is then built with the
matching ghost option (`WithForceVP9Codec`, `WithForceAV1Codec`, ...).

```
--video-codecs vp9,av1,vp8,h264   (default)
```

The server preference deliberately beats the sender preference: a browser that
lists H264 first but can do VP9 will be answered with VP9. Supported names are
`vp9`, `av1`, `vp8`, `h264` and `h265`.

Audio is always Opus, and there is no negotiation to do: WebRTC senders offer
Opus (plus G.711 and telephone-event), and AAC is not a WebRTC audio codec at
all - OBS hardcodes Opus for its WHIP output. If a sender ever does offer
something else and no Opus, the audio section is answered with port `0`, the
session comes up video only, and a warning is logged:

```
WARN Sender offers audio as [PCMU], only Opus can be forwarded. Publishing without audio.
```

Forwarding G.711 is not an option here: the ghost audio track is created as an
Opus track in `client.go`, so anything else would have to be transcoded.

Because ghost fixes the codec when the client is created, the meeting is joined
**lazily** - only when the first sender publishes. If a later session needs a
different codec, the ghost client is rebuilt, which means the participant briefly
leaves and rejoins the meeting. Same codec, same session: the connection is
reused.

## Reachability, STUN and TURN

The sender dials this server, so the connection stands or falls with **our**
candidates being reachable - not the sender's. By default the STUN and TURN
servers the eyeson API returned for the room are reused for the ingest peer
connection. They are already in hand, and unlike a bare public STUN server the
TURN entry still works when both ends sit behind a symmetric NAT.

The same servers can be handed to the sender via WHIP `Link` headers (RFC 9725
§4.4) with `--advertise-ice`, on both the `OPTIONS` and the `POST` response. It
is **off by default**: senders rarely need them for WHIP, and OBS is known to
handle advertised TURN servers badly - it reads the headers but still prefers
direct candidates and has no relay-only mode, so a TURN-only setup can hang for
minutes before failing ([obs-studio#12790](https://github.com/obsproject/obs-studio/issues/12790)).
Turn it on when your senders are ones that make good use of it.

Servers are given as one list, credentials inline:

```sh
--ice-servers stun:stun.l.google.com:19302
--ice-servers turn:user:pass@turn.example.com:3478?transport=udp,stun:stun.example.com:3478
--ice-servers none     # directly reachable, do not gather anything else
```

Setting the flag replaces the eyeson servers rather than adding to them.

Answering a publish does not wait for ICE gathering to finish. A TURN server
that is slow to allocate would otherwise delay every `POST`, so gathering is
bounded at two seconds and the answer carries whatever has been gathered by
then - host candidates are there immediately.

Two deployment cases need more than that:

* **Cloud VM behind 1:1 NAT** (AWS, GCP, Hetzner cloud): the machine only sees
  its private address, so every host candidate is useless. `--public-ip 1.2.3.4`
  rewrites them.
* **Firewall**: `--udp-port-range 50000-50100` pins ICE to a range you can open.

```sh
$ ./whip-server $API_KEY --public-ip 1.2.3.4 --udp-port-range 50000-50100
```

## Usage

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
      --trace                     trace output
      --udp-port-range string     restrict ice to a udp port range, e.g. 50000-50100
      --user string               User name to use (default "whip-test")
      --user-id string            User id to use
  -v, --verbose                   verbose output
      --video-codecs string       accepted video codecs, most preferred first (default "vp9,av1,vp8,h264")
      --whip-listen-addr string   address the WHIP endpoint listens on (default ":8100")
      --whip-path string          http path of the WHIP endpoint (default "/whip")
      --widescreen                start room in widescreen mode (default true)
```

Start it with either an API key (creates/joins a meeting) or a guest link (joins
an existing one):

```sh
$ export API_KEY=<...>
$ ./whip-server $API_KEY --room-id whip-test --bearer-token s3cr3t
```

It prints the GUI and guest links and opens the WHIP endpoint:

```
WHIP endpoint listening on http://:8100/whip
Waiting for a WHIP sender, accepted video codecs: vp9,av1,vp8,h264
```

Note that the meeting has to be kept busy - if nobody is connected it shuts down
after a short while. Since this example only joins once a sender publishes, that
matters more than in the other examples: join with the printed guest link (muted)
before you start streaming.

## Sending to it

### OBS Studio

WHIP output is available since OBS 30. In `Settings → Stream`:

| Field        | Value                            |
| ------------ | -------------------------------- |
| Service      | `WHIP`                           |
| Server       | `http://<host>:8100/whip`        |
| Bearer Token | whatever you passed to `--bearer-token` (leave empty if unset) |

In `Settings → Output` (Advanced):

* **Video encoder**: OBS offers H264, HEVC and AV1 for WHIP. H264 works out of
  the box; AV1 requires `av1` to be in `--video-codecs` (it is, by default) and
  the meeting server to accept it. OBS cannot send VP8 or VP9.
* **Keyframe interval**: 1 or 2 seconds. WebRTC receivers need a keyframe to
  start rendering.
* **Total layers**: 1. OBS 32.1+ can send simulcast; extra layers are read and
  dropped here, they would only waste upstream bandwidth.

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

Any WHIP client works as long as it offers one of the configured codecs:
`ffmpeg -f whip`, Broadcast Box, or a few lines of browser JavaScript - a browser
sender is the easiest way to get VP9 or AV1 in.

## Endpoint

| Method    | Path            | Meaning                                        |
| --------- | --------------- | ---------------------------------------------- |
| `POST`    | `/whip`         | publish, body is the SDP offer, answers `201` with a `Location` header |
| `OPTIONS` | `/whip`         | CORS preflight, advertises the eyeson STUN servers via `Link` headers |
| `DELETE`  | `/whip/<id>`    | stop publishing                                |
| `PATCH`   | `/whip/<id>`    | `405` - all candidates are already in the answer, no trickle ICE needed |

`POST` answers `415` when nothing forwardable is on offer, and `503` when the
meeting connection could not be established. Both `OPTIONS` and `POST` carry the
ICE servers in `Link` headers.

Only one publisher is active at a time. A new `POST` takes over and closes the
previous session, which is what you want when a sender crashes and reconnects.

## Files

| File           | What is in it                                                  |
| -------------- | -------------------------------------------------------------- |
| `main.go`      | cli, room handling, wiring                                      |
| `codecs.go`    | codec table, SDP offer parsing, preference resolution           |
| `eyeson.go`    | the ghost client lifecycle, rebuilt when the codec changes      |
| `ice.go`       | stun/turn config, NAT and port range settings, Link headers     |
| `whip.go`      | the WHIP http endpoint and the RTP forwarding                   |

The flow for one publish is: parse the offer → pick the codec → connect ghost
with that codec and get the tracks → build the ingest peer connection with only
that codec registered → answer → `OnTrack` copies RTP packets to the tracks. pion
rewrites SSRC and payload type on write, so nothing else has to be touched.

RTP header extensions are stripped before forwarding. They carry ids from the
WHIP negotiation (transport-cc, abs-send-time, mid, rid) which mean nothing on
the eyeson side, and ghost does not negotiate extensions at all.

## Limitations, and where they come from

* **Keyframes are requested on a timer.** When a participant joins the meeting,
  the eyeson server sends a keyframe request (PLI) to ghost - but ghost drops the
  `RTPSender` returned by `AddTrack` in `client.go`, so that request cannot be
  forwarded to the WHIP sender. The workaround is `--pli-interval`, which asks
  for a keyframe every few seconds regardless. Exposing the sender's RTCP stream
  in ghost (something like `SetVideoKeyframeRequestedHandler`) would replace the
  timer with the real thing and save bandwidth. That is a small change in the
  ghost library and probably the single most useful one for this example.
* **A codec change costs a rejoin.** Ghost builds its `MediaEngine` and
  `PeerConnection` in `NewClient`, so the codec cannot be changed on a live
  client. If ghost grew a renegotiation path, the codec swap would become
  invisible in the meeting.
* **The first seconds of a publish are silent.** The WHIP answer is sent
  immediately and the meeting is joined in parallel, because senders time out
  long before a cold ghost connect finishes. Packets that arrive before the
  eyeson tracks exist are counted and dropped, so the first moment or two of a
  cold start does not reach the meeting. Run with `-v` to see how long it took
  and how many packets went:

  ```
  INFO  Meeting connection ready after 1.83s, forwarding starts now
  DEBUG Dropped 182 video packets while the meeting connection was coming up
  ```
* **No bandwidth adaptation.** Bitrate and resolution are whatever the sender
  produces, same as the rtmp-server example. REMB/TWCC feedback from eyeson is
  not relayed back to the WHIP sender.
* **Profiles are not checked.** Only codec names are matched, not fmtp details
  such as VP9 `profile-id` or H264 `profile-level-id`. Ghost offers an empty
  fmtp line to eyeson, so this has not been a problem, but an exotic sender
  profile could still trip up the meeting server's decoder.
* **Reconnects restart the RTP sequence.** A new session brings new sequence
  numbers and timestamps on the same outgoing SSRC. Receivers usually recover
  within a keyframe, but it is not seamless.
* **Audio is Opus or nothing.** See above - transcoding is out of scope, and
  ghost's audio track is fixed to Opus anyway.
* **This is ingest only.** Pulling the meeting out again over WHEP would be a
  separate example, built on `SetVideoReceivedHandler` /
  `SetAudioReceivedHandler`.

## Troubleshooting

Run with `-v` first, the timings in the log usually point straight at the cause.

**The sender times out on the first publish but works on the second.** The
sender gave up waiting for the answer while the meeting connection or ICE
gathering was still running. Both are bounded now, but if you still see it,
check how long `Meeting connection ready after ...` reports, and try
`--ice-servers none` to take gathering out of the picture entirely.

**The sender connects but nothing shows in the meeting.** Check that a track was
claimed (`WHIP track received`) and that the codec line matches what you expect.
If the sender publishes simulcast, set it to a single layer.

**Black tile for participants who join late.** Lower `--pli-interval`, or set a
shorter keyframe interval in the sender.

**Nothing connects at all from another machine.** The sender dials this server,
so it is our candidates that must be reachable: `--public-ip` on a cloud VM,
`--udp-port-range` plus a firewall rule, TURN if neither is possible.

## Development

```sh
go mod tidy    # fetch dependencies and create go.sum
make           # build into bin/whip-server
make test      # unit tests plus an end-to-end publish over loopback
make build-platforms
```

### Binary size

`make` produces an 18 MB binary, which looks alarming next to the ~5 MB
binaries published for the other examples. They are not built the same way:

| build                                 | size   |
| ------------------------------------- | ------ |
| `make` (plain `go build`)              | 18 MB  |
| `go build -ldflags="-s -w"`            | 12 MB  |
| `make build-platforms` (`-s -w` + upx) | 3.7 MB |

`make build-platforms` is what produces release binaries, and it strips debug
info and packs with upx. Built the same way, the video-playback example comes
out at 17 MB plain and 12 MB stripped - within a rounding error of this one.

There is no transcoding here to pay for. The weight is pion/webrtc and its DTLS
and SRTP crypto, which every ghost example links in, plus the Go runtime. This
example adds nothing heavy on top: `pion/sdp` and `pion/interceptor` already
come in through pion/webrtc, and the only non-pion additions are cobra and
zerolog, which the other examples use too.

The tests need no API key and no network. They cover the SDP parsing, codec
selection, Link header rendering and port range parsing, assert that a publish
is answered without waiting for the meeting connection, and start the endpoint
four times - with an OBS-like H264 sender, a VP9-only sender, and a sender with
G.711 audio - to check that the right codec is picked, that a sender without
Opus still gets a working video-only session, and that the RTP packets arrive at
the target tracks.
