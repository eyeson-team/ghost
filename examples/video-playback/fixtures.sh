#!/usr/bin/env bash
# Generates the media files the tests expect in /tmp/media.
# Requires ffmpeg with libvpx, libaom-av1, libx264, libx265 and libopus.
set -euo pipefail

DIR="${1:-/tmp/media}"
mkdir -p "$DIR"

src_v() { echo "-f lavfi -i testsrc2=size=$1:rate=$2:duration=$3"; }
src_a() { echo "-f lavfi -i sine=frequency=440:duration=$1"; }

gen() { # gen <output> <args...>
  local out="$DIR/$1"; shift
  if [ -f "$out" ]; then echo "have $out"; return; fi
  echo "making $out"
  ffmpeg -y -loglevel error "$@" "$out"
}

# Supported codec combinations
gen vp8_opus.webm    $(src_v 320x240 25 2) $(src_a 2) -c:v libvpx      -b:v 300k -c:a libopus
gen vp9_opus.webm    $(src_v 320x240 25 2) $(src_a 2) -c:v libvpx-vp9  -b:v 300k -c:a libopus
gen av1_opus.webm    $(src_v 320x240 25 2) $(src_a 2) -c:v libsvtav1  -cpu-used 8 -b:v 200k -c:a libopus
gen h264_opus.mkv    $(src_v 320x240 25 2) $(src_a 2) -c:v libx264 -pix_fmt yuv420p -c:a libopus
gen h265.mkv         $(src_v 320x240 25 2)            -c:v libx265 -preset ultrafast -pix_fmt yuv420p -an

# Unsupported audio, unsupported container
gen vp9_vorbis.webm  $(src_v 320x240 25 2) $(src_a 2) -c:v libvpx-vp9  -b:v 300k -c:a libvorbis
gen h264_aac.mkv     $(src_v 320x240 25 2) $(src_a 2) -c:v libx264 -pix_fmt yuv420p -c:a aac
gen h264_aac.mp4     $(src_v 320x240 25 2) $(src_a 2) -c:v libx264 -pix_fmt yuv420p -c:a aac

# High bitrate, for burst and pacing behaviour
gen hibitrate_vp9.webm $(src_v 1280x720 30 5) -c:v libvpx-vp9 -b:v 25M -minrate 20M -maxrate 30M \
    -deadline realtime -cpu-used 8 -g 120 -an
gen bigframes.webm   $(src_v 1280x720 24 20) $(src_a 20) -c:v libvpx-vp9 -b:v 14M -minrate 12M \
    -maxrate 16M -deadline realtime -cpu-used 8 -g 48 -c:a libopus

# Wrapped block timecodes: ffmpeg produces negative block offsets when muxing
# video and audio together, which the parser has to unwrap.
gen negtc_audio.webm $(src_v 640x360 24 60) $(src_a 60) -c:v libvpx-vp9 -b:v 2M -maxrate 2.5M \
    -g 60 -deadline good -cpu-used 4 -c:a libopus

# A single keyframe at the start, so the whole file is one gap
gen longgop.webm     $(src_v 640x360 25 12) -c:v libvpx-vp9 -b:v 800k -deadline realtime \
    -cpu-used 8 -g 1000 -keyint_min 1000 -an

echo "fixtures ready in $DIR"
