log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))

# The default matroska muxer options include dash=1, which ffmpeg's muxer
# rejects for a file with more than one stream (audio + video).
def format_options(container) =
  if container == "matroska" or container == "webm" then [("live", "1")] else [] end
end

icecast.server(
  port=port, password="hackme", dedicated_encoder=true, format_options=format_options
)
