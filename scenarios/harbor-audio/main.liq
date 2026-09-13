log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))
s = mksafe(playlist(mode="randomize", environment.get("ICETEST_AUDIO_DIR")))

output.harbor(port=port, mount="/mp3", %mp3(bitrate=128), s)
output.harbor(port=port, mount="/opus", %opus(bitrate=96), s)
output.harbor(port=port, mount="/flac", %ogg(%flac), s)
