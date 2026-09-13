log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))
s = mksafe(playlist(mode="randomize", environment.get("ICETEST_AUDIO_DIR")))
enabled = fun (name) -> environment.get(default="", "ICETEST_#{name}") != ""

if enabled("MP3") then output.harbor(port=port, mount="/mp3", %mp3(bitrate=128), s) end
if enabled("OPUS") then output.harbor(port=port, mount="/opus", %opus(bitrate=96), s) end
if enabled("FLAC") then output.harbor(port=port, mount="/flac", %ogg(%flac), s) end
