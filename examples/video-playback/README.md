# ghost-player

Streams a local video file into an eyeson meeting, as if it were a
participant's camera and microphone.

The file is passed through rather than converted, so playback is light on CPU
and the picture keeps its original quality.

## Usage

```
ghost-player [flags] $API_KEY|$GUEST_LINK VIDEO_FILE
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
| Video | VP8, VP9, AV1, H264, H265 |
| Audio | Opus |

If the audio is in another format the video still plays, without sound. If the
video is in another format, or the file is in another container such as MP4,
playback is refused.

H264 and H265 video must not contain **B-frames**: the eyeson media server
cannot display them, and the picture stays black. The player keeps streaming
such a file, but logs a warning. For a quick start, also use a keyframe every
2 seconds, otherwise the picture only appears at the next keyframe.

Any tool that writes Matroska can prepare a file, for example:

```sh
# already a supported codec without B-frames, just rewrap and convert the audio
ffmpeg -i input.mp4 -c:v copy -c:a libopus -ac 1 output.mkv

# anything else needs the video transcoded, as H264 ...
ffmpeg -i input.mp4 -c:v libx264 -bf 0 \
  -force_key_frames "expr:gte(t,n_forced*2)" -c:a libopus -ac 1 output.mkv

# ... or as H265
ffmpeg -i input.mp4 -c:v libx265 -x265-params "bframes=0:open-gop=0" \
  -force_key_frames "expr:gte(t,n_forced*2)" -forced-idr 1 -c:a libopus -ac 1 output.mkv
```

To check whether a file has B-frames:

```sh
ffprobe -v error -select_streams v:0 -show_entries stream=has_b_frames -of default=nw=1 input.mkv
```

## Flags

| Flag | |
| --- | --- |
| `--loop` | Restart playback at the end of the file. Default on. |
| `--no-audio` | Never send audio, even when the format matches. |
| `--start-delay` | Wait after connecting before playback starts, so the first keyframe isn't lost. Default `2s`. |
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