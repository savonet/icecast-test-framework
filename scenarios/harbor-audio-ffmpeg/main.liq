log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))
s = mksafe(playlist(mode="randomize", environment.get("ICETEST_AUDIO_DIR")))

# Formats without a stream header, which a shared %ffmpeg encoder can serve to
# listeners joining mid-stream. The ogg family needs the native encoders.
output.harbor(
  port=port, mount="/mp3", %ffmpeg(format="mp3", %audio(codec="libmp3lame", b="128k")), s
)
output.harbor(port=port, mount="/aac", %ffmpeg(format="adts", %audio(codec="aac", b="128k")), s)
