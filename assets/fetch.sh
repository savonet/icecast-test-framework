#!/bin/sh
# Pulls Creative-Commons test videos into assets/ with yt-dlp, as 720p h264/aac mp4,
# which icecast-server-video pushes with -c copy and harbor-video-remux plays untouched.
set -eu
cd "$(dirname "$0")"
# Big Buck Bunny (Blender Foundation, CC-BY 3.0)
for id in aqz-KE-bpKQ; do
  yt-dlp -f "bv*[height<=720][vcodec^=avc1]+ba[acodec^=mp4a]/b[height<=720][ext=mp4]" \
    --merge-output-format mp4 -o "%(title)s.%(ext)s" "https://www.youtube.com/watch?v=$id"
done
