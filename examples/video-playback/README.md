# ghost-player

Streams a local video file into an eyeson meeting, as if it were a
participant's camera and microphone.

The file is passed through rather than converted, so playback is light on CPU
and the picture keeps its original quality.

## Usage

```
ghost-player [flags] $API_KEY|$GUEST_LINK VIDEO_FILE
ghost-player --check VIDEO_FILE
```

```sh
# start a new meeting from an API key
ghost-player $API_KEY clip.webm

# join an existing meeting with a guest link
ghost-player 'https://app.eyeson.team/?guest=TOKEN' clip.mkv
```

Quote guest links: `?` is a shell special character.

The meeting's GUI link is printed on startup. Open it and stay in the meeting
while the file plays, because a meeting with no participants shuts itself down.

## Supported files

Container: **WebM** or **Matroska** (`.webm`, `.mkv`).

| | Supported |
| --- | --- |
| Video | VP8, VP9, AV1, H264 |
| Audio | Opus |

If the audio is in another format the video still plays, without sound. If the
video is in another format, or the file is in another container such as MP4,
playback is refused.

H265/HEVC is not on the list: pion has no payloader for it, and sending it
would mean maintaining one here. Transcode such a file, or wait for pion to
support it.

Any tool that writes Matroska can prepare a file, for example:

```sh
# already a supported codec, just rewrap and convert the audio
ffmpeg -i input.mp4 -c:v copy -c:a libopus -ac 1 output.mkv

# anything else, H265 included, needs the video transcoded
ffmpeg -i input.mp4 -c:v libx264 -c:a libopus -ac 1 output.mkv
```

## Checking a file first

`--check` reports what a file contains and how well it is likely to stream. It
reads the file only, and never joins a meeting:

```sh
ghost-player --check clip.mkv
```

```
clip.mkv

  Container   matroska, 12m14s
  Resolution  1280x534 @ 24.0 fps
  Video       H264, supported
  Audio       Opus, supported
  Bitrate     2.1 Mbit/s average, 3.1 Mbit/s sustained (95th percentile second)
  Worst case  4.2 Mbit/s in one second, largest frame 160 KB (140 RTP packets)
  Keyframes   453, longest stretch without one 2.1s

  READY TO STREAM
  A sustained 3.1 Mbit/s is comfortably within what a meeting participant
  can send.
```

The verdict is one of:

| Verdict | Meaning | Exit code |
| --- | --- | --- |
| `READY TO STREAM` | should play well | 0 |
| `SHOULD STREAM, WITH SOME RISK` | plays, but a lost packet may briefly freeze the picture | 0 |
| `TOO HEAVY TO STREAM` | more bitrate than a participant can send; expect little or no picture | 1 |
| `CANNOT BE STREAMED` | unsupported codec or container | 1 |

Anything else worth knowing is listed underneath as a short note: audio that
cannot be sent, a frame rate above the 25 fps the media server uses, long gaps
between keyframes, or an unusually heavy second.

The exit codes make it usable as a gate:

```sh
ghost-player --check clip.mkv && ghost-player $API_KEY clip.mkv
```

## Flags

| Flag | |
| --- | --- |
| `--check` | Report on the file and exit, without connecting. |
| `--loop` | Restart playback at the end of the file. Default on. |
| `--no-audio` | Never send audio, even when the format matches. |
| `--room-id` | Join a specific room instead of creating a meeting. |
| `--user` | Display name in the meeting. Default `ghost-player`. |
| `--widescreen` | Start the room in widescreen. Default on. |
| `--verbose`, `--quiet` | More or less logging. |

`--api-endpoint`, `--custom-ca` and `--insecure` are available for non-default
deployments.

## Building

```sh
go build -o bin/ghost-player .
```

Requires `github.com/eyeson-team/ghost/v2` v2.9.9 or newer.

## Tests

`fixtures.sh` generates the media the tests need with ffmpeg. The tests play
files in real time, so the suite takes a couple of minutes.

```sh
./fixtures.sh
go test ./...
```
