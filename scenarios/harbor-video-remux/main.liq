log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))

# Encoded packets pass through untouched; each listener gets its own muxer,
# so a stream starts with a fresh container header wherever the listener joins.
video =
  (single(environment.get("ICETEST_VIDEO")) : source(audio=ffmpeg.copy, video=ffmpeg.copy))

output.harbor(
  port=port,
  mount="/video",
  format="video/x-matroska",
  fallible=true,
  dedicated_encoder=true,
  %ffmpeg(format="matroska", live=1, %audio.copy, %video.copy),
  video
)
