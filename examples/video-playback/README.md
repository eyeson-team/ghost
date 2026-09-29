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

Any tool that writes Matroska can prepare a file, for example:

```sh
# already a supported codec, just rewrap and convert the audio
ffmpeg -i input.mp4 -c:v copy -c:a libopus -ac 1 output.mkv

# anything else, H265 included, needs the video transcoded
ffmpeg -i input.mp4 -c:v libx264 -c:a libopus -ac 1 output.mkv
```

## Checking a file first

`--check` reports what a file contains and whether it can be played. It reads
the file header only, and never joins a meeting:

```sh
ghost-player --check clip.mkv
```

```
clip.mkv

  Container   matroska, 12m14s
  Resolution  1280x534 @ 24.00 fps
  Video       H264, supported
  Audio       Opus, supported

  PLAYABLE
```

The frame rate is shown only if the file's header declares one.

| Result | Meaning | Exit code |
| --- | --- | --- |
| `PLAYABLE` | container and video codec are supported | 0 |
| `NOT PLAYABLE` | unsupported container or video codec | 1 |

An unsupported audio codec does not make a file unplayable: it is reported,
and the video plays without sound.

The exit codes make it usable as a gate:

```sh
ghost-player --check clip.mkv && ghost-player $API_KEY clip.mkv
```

## Flags

| Flag | |
| --- | --- |
| `--check` | Report the file's container, resolution and codecs, then exit without connecting. |
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