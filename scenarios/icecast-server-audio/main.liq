log.level := 3
port = int_of_string(environment.get(default="8000", "ICETEST_PORT"))

icecast.server(port=port, password="hackme", dedicated_encoder=true)
