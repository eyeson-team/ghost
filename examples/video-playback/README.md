# ghost-player (video-playback example)

Streams a local video file into an eyeson meeting as if it were a participant's
camera and microphone.

Nothing is transcoded. Frames are read out of the container and passed straight
through to WebRTC, so CPU usage stays low but the file's codecs have to be ones
the eyeson media server accepts.

## Supported input

Container: **WebM / Matroska** (`.webm`, `.mkv`).

| Video codec | Matroska CodecID | Status |
| --- | --- | --- |
| VP8 | `V_VP8` | supported |
| VP9 | `V_VP9` | supported |
| AV1 | `V_AV1` | supported |
| H264 | `V_MPEG4/ISO/AVC` | supported |
| H265 | `V_MPEGH/ISO/HEVC` | not supported |
| anything else | | rejected at startup |

| Audio codec | Matroska CodecID | Status |
| --- | --- | --- |
| Opus | `A_OPUS` | supported |
| AAC, Vorbis, MP3, ... | | dropped, video still plays |

If the video codec does not match, the player refuses to start and tells you
what it found. If only the *audio* codec does not match, playback continues
without audio and logs the reason. This is deliberate: a silent meeting is
better than no meeting.

## Usage

```
ghost-player [flags] $API_KEY|$GUEST_LINK VIDEO_FILE
```

Examples:

```sh
# start a new meeting from an API key
ghost-player $API_KEY clip.webm

# join an existing meeting via guest link
ghost-player 'https://app.eyeson.team/?guest=TOKEN' clip.mkv

# play once instead of looping, and never send audio
ghost-player --loop=false --no-audio $API_KEY clip.webm
```

Note that a meeting shuts down if no real participant is connected, so join the
GUI link that gets logged on startup.

### Flags

Beyond the existing ones (`--api-endpoint`, `--user`, `--room-id`, `--loop`,
`--widescreen`, `--verbose`, `--quiet`, `--custom-ca`, `--insecure`):

| Flag | Meaning |
| --- | --- |
| `--no-audio` | Never send audio, even when the codec would match. |

## Preparing arbitrary files

MP4, MOV, AVI and friends are not read by this player. Remux them into
Matroska first. A remux copies the video bytes untouched and runs much faster
than realtime:

```sh
# H264 video kept as-is, AAC audio converted to Opus
ffmpeg -i input.mp4 -c:v copy -c:a libopus out.mkv
```

Only if the video codec itself is unsupported do you need a real transcode:

```sh
ffmpeg -i input.mov -c:v libvpx-vp9 -b:v 2M -c:a libopus out.webm
```

To check what you have before converting:

```sh
ffprobe -v error -show_entries stream=codec_type,codec_name -of csv=p=0 input.mp4
```

## Building

The codec options this example uses (`WithForceVP8Codec`, `WithForceVP9Codec`)
were added to ghost after the version the example originally pinned, so the
dependency needs to be at **v2.9.9 or newer**:

```sh
go get github.com/eyeson-team/ghost/v2@v2.9.9
go build -o bin/ghost-player .
```

## Tests

`main_test.go` covers probing and packetizing against real files. Generate the
fixtures with ffmpeg first:

```sh
mkdir -p /tmp/media
for v in "libvpx vp8" "libvpx-vp9 vp9"; do
  set -- $v
  ffmpeg -y -f lavfi -i testsrc=size=320x240:rate=25:duration=2 \
         -f lavfi -i sine=frequency=440:duration=2 \
         -c:v $1 -b:v 300k -c:a libopus /tmp/media/${2}_opus.webm
done
ffmpeg -y -f lavfi -i testsrc=size=320x240:rate=25:duration=2 \
       -f lavfi -i sine=frequency=440:duration=2 \
       -c:v libvpx-vp9 -b:v 300k -c:a libvorbis /tmp/media/vp9_vorbis.webm
ffmpeg -y -f lavfi -i testsrc=size=320x240:rate=25:duration=2 \
       -f lavfi -i sine=frequency=440:duration=2 \
       -c:v libaom-av1 -cpu-used 8 -b:v 200k -c:a libopus /tmp/media/av1_opus.webm
for a in "libopus opus" "aac aac"; do
  set -- $a
  ffmpeg -y -f lavfi -i testsrc=size=320x240:rate=25:duration=2 \
         -f lavfi -i sine=frequency=440:duration=2 \
         -c:v libx264 -pix_fmt yuv420p -c:a $1 /tmp/media/h264_${2}.mkv
done
ffmpeg -y -f lavfi -i testsrc=size=320x240:rate=25:duration=2 \
       -f lavfi -i sine=frequency=440:duration=2 \
       -c:v libx264 -pix_fmt yuv420p -c:a aac /tmp/media/h264_aac.mp4

go test -v ./...
```

The tests are real-time paced, so the suite takes about 20 seconds.

## Troubleshooting

### It prints the codec lines and then appears to hang

```
INF Video: V_VP8 1920x800
WRN Audio: audio codec "A_VORBIS" cannot be sent ...
<nothing>
```

The player has joined and is waiting for the meeting to report itself ready.
`WaitReady` polls the API for up to 180 seconds, so an unreachable meeting looks
exactly like a freeze. Progress is now logged every 10 seconds.

The usual cause is a **stale guest link**. A guest link joins a meeting that is
already running - it does not create one - and eyeson shuts a meeting down
shortly after the last participant leaves. So a link generated earlier will be
accepted by the API (the token is still valid) while the meeting behind it is
gone.

Fixes, in order of convenience:

1. Pass an **API key** instead of a guest link. That creates a fresh meeting,
   and the GUI link is printed on startup.
2. Or open the guest link in a browser first and stay in the meeting, then
   start the player.

Either way, keep a real participant in the meeting for the whole playback,
muted if you like. If everybody leaves, the meeting shuts down under the
player.

Also quote guest links on the command line - `?` is a shell glob character:

```sh
./bin/ghost-player 'https://app.eyeson.team/?guest=TOKEN' clip.webm
```

### "Failed to get room"

The API rejected the join outright: wrong or expired token, wrong
`--api-endpoint`, or no network. This one fails immediately rather than
waiting.

### Video is black, or freezes and never recovers

Almost always a **bitrate** problem rather than a codec problem. A useful check:
if the same clip fails in two different codecs, the codec is not the cause.

```sh
ffprobe -v error -show_entries format=bit_rate -of csv=p=0 clip.webm
```

Rough guidance for a WebRTC participant:

| Source bitrate | Expectation |
| --- | --- |
| up to ~2.5 Mbit/s | fine |
| ~2.5-6 Mbit/s | works, but a lost packet can freeze the picture until the next keyframe |
| above ~6 Mbit/s | do not expect usable video, transcode instead |

Why it fails so completely rather than just looking worse: a 25 Mbit/s 720p file
produces over a hundred RTP packets per frame. The ghost client advertises
`nack` and `nack pli` in its SDP but registers no NACK responder, so a packet
lost in that burst is never retransmitted. Video codecs code most frames as
deltas against earlier ones, so a single missing packet in a keyframe means
nothing decodes until the *next* keyframe arrives intact. With a long GOP that
can be many seconds away, or never.

If a clip takes a while to appear, or shows and then freezes for good, that is
this mechanism.

The player paces each frame's packets across the frame interval to reduce the
burst, but pacing cannot make a 25 Mbit/s source acceptable. Transcode it:

```sh
# 720p at 2 Mbit/s, keyframe every 2s to bound recovery time
ffmpeg -i Big_Buck_Bunny_720_10s_30MB.webm \
  -c:v libvpx-vp9 -b:v 2M -maxrate 2.5M -g 60 \
  -c:a libopus out.webm
```

Short keyframe intervals (`-g`) matter as much as bitrate: they cap how long a
freeze can last after a loss.

### Ctrl-C does not stop the player

Fixed, but worth knowing why, because the failure mode was a process that only
`kill -9` could stop.

Two separate causes, both triggered by losing the connection to the meeting:

`Client.TerminateCall` sends a terminate message over the signalling websocket
and then waits for the server to confirm, using `context.Background()`. A
background context has a nil `Done` channel, so that wait has no timeout. When
the websocket is down the message is dropped by the sender goroutine and the
confirmation never arrives, so the call blocks forever. Shutdown now runs with
a 5 second cap and exits regardless.

Separately, `signal.Notify` takes SIGINT away from the runtime for the whole
process. Once the first Ctrl-C had been consumed, further presses were
delivered to a channel nobody was reading rather than killing the process. The
player now calls `signal.Stop` as soon as it starts shutting down, which
restores default handling, so a second Ctrl-C terminates immediately.

### "read failed with: websocket: close 1006"

Expected on a flaky connection, and handled. gosepp reconnects: a read failure
breaks its inner loop and the outer loop dials again, retrying every 2 seconds
indefinitely. The `failed to send ping` warnings every 3 seconds are the
keepalive failing against a socket that has not recovered yet.

What the player does *not* currently do is give up on its own after a long
outage. If ICE reaches `failed` the stream is dead but the process stays up
until you stop it.

## Implementation notes

Things that are easy to get wrong here, kept as notes so they don't get
re-broken:

**RTP timestamps come from the container timecode.** An earlier version
advanced the timestamp by a fixed 90000 ticks per frame, which is one full
second per frame at a 90kHz clock. Video-only that mostly goes unnoticed; with
audio it destroys lip sync. Timestamps are now computed as an absolute offset
from the packetizer's random start value.

**H264 timecodes go backwards.** B-frames mean the container timecode is not
monotonic. Accumulating deltas and clamping negatives inflates the stream
duration (a 2s clip measured 3.3s). The absolute-offset approach handles it,
because a backwards jump produces a `uint32` that wraps to the right value.

**H264 in Matroska is AVCC, not Annex-B.** NAL units are length-prefixed and
SPS/PPS live in `CodecPrivate` as an `avcC` record. pion's `H264Payloader`
expects Annex-B and harvests SPS/PPS from the stream, so the parameter sets are
injected in front of every keyframe. In the `avcC` layout, byte 5 holds the SPS
*count*; the length-prefixed sets start at byte 6.

**AV1 needs OBU splitting.** Matroska stores one whole temporal unit per block,
while pion's `AV1Payloader` expects a single OBU per call. Handed a temporal
unit it mistakes the whole thing for a sequence header, caches it, and then
panics with a slice-bounds error on the next frame. `av1TemporalUnitPayloader`
splits the unit and drops temporal delimiters and padding, as the AV1 RTP spec
requires.

**`webm.Parse` starts a background goroutine.** It keeps reading from the file
and parks at EOF waiting for a seek command. Closing the file without calling
`Shutdown()` and draining `reader.Chan` either leaks the goroutine (one per
playback loop, each holding a file handle) or panics on a closed file.

**Frames are paced, not burst.** A frame's RTP packets are released in ~1ms
slices across the frame interval rather than written back to back. The budget
is bounded by the frame duration, so pacing never causes playback to fall
behind - it uses time the ingest loop would have spent sleeping.

**Audio and video share one wall clock.** Both tracks are paced from the same
`started` timestamp in a single loop over the reader channel, which is what
keeps them in sync relative to each other.

**Looping offsets timecodes.** Container timecodes restart at zero on each
pass, so `ingestControl` carries an accumulating offset to keep RTP timestamps
monotonic across loops rather than jumping backwards.
