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
| `--check` | Inspect the file and report whether it can be streamed, then exit. |

## Checking a file before streaming

`--check` inspects a file and reports whether it will stream, without
contacting the API or creating a meeting:

```sh
ghost-player --check clip.webm
```

```
clip.webm

  Container   webm, 10s
  Resolution  1280x720 @ 30.0 fps
  Video       AV1, supported
  Audio       Vorbis, NOT supported, will stream without audio
  Bitrate     25.2 Mbit/s average, 27.4 Mbit/s sustained (95th percentile second)
  Worst case  31.0 Mbit/s in one second, largest frame 210 KB (180 RTP packets)
  Keyframes   3, longest stretch without one 4s

  NOT RECOMMENDED
  At a sustained 27.4 Mbit/s this is far above what a meeting participant
  can send. Expect little or no picture. Re-encode before streaming.

  - audio codec "A_VORBIS" cannot be sent (only Opus is supported), streaming video only
  - the largest frame needs 180 RTP packets, which is a heavy burst

  Suggested:
    ffmpeg -i clip.webm -c:v libvpx-vp9 -b:v 2M -maxrate 2.5M -bufsize 2M -g 60 -c:a libopus out.webm
```

Everything reported comes from the file itself: the packet stream is scanned
once, so the bitrates are measured rather than taken from container metadata,
and the packet count for the worst frame is produced by the same payloader the
player uses.

The verdict is one of:

| Verdict | Meaning | Exit code |
| --- | --- | --- |
| `LOOKS GOOD` | should stream fine | 0 |
| `SHOULD WORK, WITH RISK` | plays, but a lost packet may freeze the picture | 0 |
| `NOT RECOMMENDED` | bitrate too high, expect little or no picture | 1 |
| `WILL NOT STREAM` | unsupported codec or unreadable container | 1 |

Exit codes make it usable as a gate:

```sh
ghost-player --check clip.webm && ghost-player $API_KEY clip.webm
```

Note that an unsupported *audio* codec is a warning, not a failure: the player
streams video only, so the exit code stays 0.

The suggested ffmpeg line only re-encodes what has to be re-encoded. If the
video codec is fine and only the audio is wrong, it copies the video stream.

### Can the player buffer, or drop frames, instead of re-encoding?

No, and it is worth knowing why, because both sound like they should work.

**Dropping frames does not reduce bitrate here.** Video codecs code most frames
as differences from earlier ones. Dropping a frame that a later frame refers to
corrupts everything until the next keyframe. The player passes through whatever
the file contains and cannot re-derive those references without decoding and
re-encoding, which is exactly the transcode being avoided. Frame rate is only
reducible at encode time, which is why the suggested command uses `-r 25` when
the source runs faster than the server's 25 fps.

**Buffering does not reduce it either.** Buffering smooths delivery but does
not change how many bits the file needs. The player already paces each frame
across its interval, and the reader runs ahead so neither track stalls the
other. Spreading further means falling behind: a 710 KB frame emitted at 3
Mbit/s takes nearly two seconds, and since this is a live meeting rather than a
download, that latency accumulates instead of being absorbed.

So the bits have to come down at encode time. Resolution is the strongest
lever, ahead of any bitrate flag: a meeting tile is nowhere near 1080p wide,
and halving the width cuts both sustained bitrate and keyframe size.

### Where the thresholds came from

The verdicts are calibrated against files actually streamed into a meeting,
not against theory:

| Sustained | Codec / size | Result |
| --- | --- | --- |
| 1.0 Mbit/s | VP9 1280x720 | clean |
| 1.5 Mbit/s | VP9 1280x720 | clean |
| 3.1 Mbit/s | H264 1280x534 | clean, 140 packet frames |
| 3.3 Mbit/s | VP9 1920x800 | clean video, 614 packet frames |
| 11.2 Mbit/s | VP8 1920x800 | no usable picture |
| 46.1 Mbit/s | VP9 1280x534 | no picture |

So anything a little over 3 Mbit/s is known good, and the failures begin an
order of magnitude higher. `LOOKS GOOD` runs to 4 Mbit/s sustained,
`NOT RECOMMENDED` starts at 8, and the band between covers the untested gap
rather than pretending to know exactly where it breaks. `calibration_test.go`
holds this table as assertions, so the thresholds cannot drift back without a
test failing.

Note the burst warning threshold in particular: files with 614 packet frames
have streamed cleanly since the pacer landed, so no failure has yet been traced
to burst size alone. It warns at 400 as a heads-up, not as a fault.

A file that streams cleanly is given no ffmpeg suggestion at all.

### Re-encoding: use x264, not libvpx-vp9

The suggested commands use `libx264` when the video has to be re-encoded, even
though the eyeson media server takes VP8, VP9 and AV1 too.

`libvpx-vp9` in single pass VBR is unreliable about hitting a bitrate target.
A 12 minute 1080p source asked for `-b:v 2M` came back at 31 Mbit/s in a 2.8 GB
file, having ignored `-maxrate` and `-bufsize` entirely, and took 43 minutes to
do it. The same material through x264 holds the target and encodes many times
faster. Measured on a 30 second 1920x800 source scaled to 1280 wide:

| | libvpx-vp9 | libx264 |
| --- | --- | --- |
| speed | ~0.3x realtime | ~32x realtime |
| bitrate asked / got | 2 Mbit/s / 31 Mbit/s | 2 Mbit/s / 2.1 Mbit/s |
| largest frame | 459 KB (397 packets) | 160 KB (140 packets) |

A 12 minute 1080p source took 43 minutes through libvpx-vp9 and 22 seconds
through x264, on the same machine and the same ffmpeg build.

Baseline profile is specified deliberately: it keeps B-frames out of the
stream, so container timecodes stay monotonic.

If you would rather stay with VP9, use two-pass encoding and verify the result
with `--check` before streaming. Do not trust a single pass VBR target.

### Why the verdict uses the 95th percentile and not the peak

A feature film contains scene cuts, and the keyframe at a cut can be many times
the size of an ordinary frame. One second of a perfectly reasonable 2 Mbit/s
file can therefore measure ten times that. Judging the file by its worst second
condemns material that plays fine apart from a brief stutter at the cut.

So the verdict is based on the higher of the average and the 95th percentile
second, which is what the file asks for continuously. The peak second and the
largest frame are still reported, and a peak far above the sustained rate
becomes a warning rather than a rejection.

If you do want to flatten the spikes, note that `-maxrate` alone does very
little: it needs `-bufsize` to constrain the rate control window, and
`-max-intra-rate` caps how far a keyframe may exceed the average frame size.
In libvpx's VBR mode even those are advisory, which is why the suggested
commands reach for resolution and x264 instead.

The thresholds behind the verdicts (2.5 and 6 Mbit/s sustained) are starting
estimates, not measurements against the media server. They are constants at the
top of `check.go`; once you have streamed enough files to know where the line
really falls, move them.

```sh
ffmpeg -i in.webm -c:v libvpx-vp9 -b:v 2M -maxrate 2.5M -bufsize 2M \
  -max-intra-rate 300 -g 60 -c:a copy out.webm
```

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

### The numbers from --check look impossible

Symptoms: a duration longer than the file really is, a frame rate lower than
the real one, a peak bitrate many times the average, or a keyframe gap of
around 65 seconds.

These all come from the same upstream parsing bug: `ebml-go` reads Matroska
block timecodes as unsigned when they are signed, so negative offsets arrive
65536ms too large. The player corrects for this now. If you see numbers like
these from an older build, they are artefacts rather than a problem with the
file, and `--verbose` reports how many timecodes were corrected.

Files muxed by ffmpeg with an audio track are the common case. A file that
looks fine in `ffprobe` but reports a wildly inflated peak bitrate here was
almost certainly hitting this.

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

**Block timecodes need unwrapping.** Matroska stores a block's timecode as a
signed 16 bit offset from its cluster timecode, but `ebml-go` parses it as
unsigned, so every negative offset arrives 65536ms too large. ffmpeg produces
such offsets routinely when muxing with audio, because a cluster begins on a
video keyframe while an audio block in it can carry a slightly earlier
presentation time. Uncorrected, playback sleeps for a minute on the bad packet
and the RTP timestamp jumps 65 seconds forward and back, so the receiver
decodes nothing. `timecodeFixer` subtracts the wrap from any jump past half its
size; real gaps between blocks are never that large.

**Audio is paced on its own goroutine, with the reader running ahead.** Pacing
a large video frame occupies most of a frame interval, and a 1080p frame can
take over 30ms. If reading happened on the same goroutine, no audio packet
could be read during that window, so it would already be late when sent. Opus
wants a packet every 20ms, and the result is audibly scratchy. Measured on a
14 Mbit/s 720p file, the 95th percentile gap between audio packets was 37.9ms
before and 20.8ms after, with the maximum dropping from 58.0ms to 21.1ms. Both
goroutines pace against the same start time, so the tracks stay in sync.

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
