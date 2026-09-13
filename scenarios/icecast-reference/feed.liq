log.level := 3
server_host = environment.get(default="127.0.0.1", "ICETEST_HOST")
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))
s = mksafe(playlist(mode="randomize", environment.get("ICETEST_AUDIO_DIR")))

output.icecast(host=server_host, port=port, password="hackme", mount="/mp3", %mp3(bitrate=128), s)
output.icecast(host=server_host, port=port, password="hackme", mount="/opus", %opus(bitrate=96), s)
output.icecast(host=server_host, port=port, password="hackme", mount="/flac", %ogg(%flac), s)
