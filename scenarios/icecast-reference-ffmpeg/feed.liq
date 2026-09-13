log.level := 3
server_host = environment.get(default="127.0.0.1", "ICETEST_HOST")
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))
s = mksafe(playlist(mode="randomize", environment.get("ICETEST_AUDIO_DIR")))
enabled = fun (name) -> environment.get(default="", "ICETEST_#{name}") != ""

if enabled("MP3") then
  output.icecast(
    host=server_host,
    port=port,
    password="hackme",
    mount="/mp3",
    %ffmpeg(format="mp3", %audio(codec="libmp3lame", b="128k")),
    s
  )
end
if enabled("AAC") then
  output.icecast(
    host=server_host,
    port=port,
    password="hackme",
    mount="/aac",
    %ffmpeg(format="adts", %audio(codec="aac", b="128k")),
    s
  )
end
