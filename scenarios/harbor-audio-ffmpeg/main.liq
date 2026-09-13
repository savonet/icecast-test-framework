log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))
s = mksafe(playlist(mode="randomize", environment.get("ICETEST_AUDIO_DIR")))
enabled = fun (name) -> environment.get(default="", "ICETEST_#{name}") != ""

# Formats without a stream header, which %ffmpeg can serve to listeners
# joining mid-stream. The ogg family needs the native encoders.
if enabled("MP3") then
  output.harbor(
    port=port, mount="/mp3", %ffmpeg(format="mp3", %audio(codec="libmp3lame", b="128k")), s
  )
end
if enabled("AAC") then
  output.harbor(port=port, mount="/aac", %ffmpeg(format="adts", %audio(codec="aac", b="128k")), s)
end
