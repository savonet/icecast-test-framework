# icecast-test-framework

Measures how many concurrent listeners an Icecast-compatible server sustains,
and what it costs: aggregate network throughput, CPU, memory and file
descriptors, sampled while the listener count is ramped up until something
breaks.

It was written to size liquidsoap's own listener-facing outputs
(`output.harbor` and `icecast.server`), with a real Icecast as a reference
point, but any server that speaks Icecast-style HTTP streaming can be measured.

## Quick start

```sh
go build -o icetest .
./icetest run --liquidsoap /path/to/liquidsoap --audio /path/to/music \
  --with MP3,OPUS --start 500 --step 500 --max 10000 --hold 60s scenarios/harbor-audio
```

Results land in `results/<scenario>/<timestamp>/`:

- `report.md`, the human summary (also printed at the end)
- `report.html`, the same plus graphs over time: listeners, throughput, CPU,
  memory, file descriptors, per-mount listener rate, lagging listeners and
  TTFB per step. Self-contained, so it can be attached to an issue.
- `run.json`, everything: per-step results, per-second process samples, per-tick
  per-mount rates, canaries, log alerts, listener events
- `<process>.log`, stdout and stderr of every managed process

`./icetest report <run-dir>` re-renders both reports from `run.json`.

Video scenarios need a file: `assets/fetch.sh` pulls a Creative-Commons one
with yt-dlp, then pass it with `--video`.

## How a run works

1. The scenario's processes are started in order (after their `prepare`
   commands; liquidsoap scripts run `--cache-only` first so the type-checking
   burst does not land in the measurement).
2. Every mount is polled until it returns `200` and a first body byte.
3. `--warmup` idle time.
4. Listeners are ramped: `--start`, then `+--step` per step, up to `--max`.
   Each step waits for the target to be reached, then holds for `--hold`.
   Halfway through the hold, one ffmpeg canary per mount connects and decodes
   `--canary` seconds of stream.
5. A step passes when every check below holds. The first failing step ends
   the run; the largest passing target is the ceiling.

`--step 0` runs a single step, which is how to soak at a known level.

## What "supported" means

A listener is a raw TCP connection sending an HTTP/1.0 `GET` and reading the
body forever (or for an exponentially distributed session when `--churn` is
set). Half of them (`--icy`) request ICY metadata and strip the interleaved
blocks, checking each one parses. Every listener counts stream bytes, and
once per second the pool measures every listener's rate over the last
`--window` seconds.

A listener fails when:

- the connection or response headers take longer than `--connect-timeout`
- the status is not 200, or the `Content-Type` differs from the scenario's
- nothing arrives for `--stall`
- the server closes the connection
- its rate stays below the mount median by more than `--lag-tolerance` for
  `--lag-ticks` consecutive seconds. This is the early signal: the server is
  starving that socket, and would eventually drop it when its per-client
  buffer fills.

A failed listener reconnects after `--retry-delay`, like a real client.

A step fails when:

- failed listeners exceed `--fail-threshold` of the peak live count
- the target was not reached (connect errors)
- a mount's median rate is below 90% of its `nominal_kbps`
- an ICY metadata block was malformed
- a canary could not open or decode the stream. Canaries are `ffmpeg -xerror`
  runs, so they also prove that a listener joining mid-stream gets a playable
  stream from its very first bytes, which is the point of per-listener
  remuxing. A canary that cannot open the stream is retried
  `--canary-retries` times (default 1), since a join can land on a track
  boundary of a chained ogg stream; every attempt is kept in the report. `--canary-lenient` drops `-xerror`, tolerating a stream whose
  first bytes fall mid-frame as long as ffmpeg can open it and keep decoding.
- the server log matched an alert pattern (liquidsoap's clock catch-up
  warning, source leak warning, ...; `--ignore-alerts` demotes these to a
  column in the report)
- a managed process exited

## Metrics

Per step, the report shows peak live listeners, average and peak Mbit/s
received by the listeners (this is the server's outbound bandwidth), TTFB
percentiles, failures, the maximum number of lagging listeners seen at once,
and for each managed process the average and peak cores consumed, peak RSS
and peak file descriptors. Processes are measured as process groups, so a
wrapper (`opam exec`, `dune exec`) and ffmpeg children are included. The
`icetest` column shows what the load generator itself costs; system-wide CPU
is reported separately.

## Scenarios

A scenario is a directory with a `scenario.json` and whatever it references:

```json
{
  "description": "...",
  "templates": ["icecast.xml.tmpl"],
  "alert_patterns": ["Latency is too high"],
  "processes": [
    {"name": "liquidsoap", "server": true,
     "prepare": [["${LIQUIDSOAP}", "--cache-only", "main.liq"]],
     "cmd": ["${LIQUIDSOAP}", "main.liq"]},
    {"name": "feed-mp3", "cmd": ["ffmpeg", "...", "icecast://source:hackme@${HOST}:${PORT}/mp3"]}
  ],
  "mounts": [
    {"path": "/mp3", "content_type": "audio/mpeg", "nominal_kbps": 128, "weight": 1}
  ]
}
```

Commands run with the scenario directory as working directory. `${VAR}` is
expanded in commands and templates, and every variable is also in the
environment as `ICETEST_<VAR>`:

| variable | value |
|---|---|
| `PORT`, `HOST` | where the listeners connect |
| `LIQUIDSOAP` | `--liquidsoap` |
| `AUDIO_DIR` | `--audio` |
| `AUDIO_CONCAT` | an ffmpeg concat playlist of `AUDIO_DIR`, written into the run dir |
| `VIDEO` | `--video` |
| `RUN_DIR` | the results directory of this run |
| `SCENARIO_DIR` | the scenario directory |

A mount or process with `"when": "MP3"` is only part of the run when
`--with MP3` is passed (to both `run` and `serve`, comma-separated for
several); the name is exported to the scripts as `ICETEST_MP3=1` so they can
gate an output on it. Every format of the audio scenarios is optional this
way, so a run measures exactly the mix of mounts a deployment serves.

Templates are rendered into the run directory with the `.tmpl` suffix
dropped. The process marked `server` has its log scanned for
`alert_patterns` (defaults cover liquidsoap's clock and source-leak warnings).
Listeners are split across mounts by `weight`.

Shipped scenarios:

| scenario | what is measured |
|---|---|
| `harbor-audio` | liquidsoap encodes and serves the listeners itself (`output.harbor`); `--with MP3,OPUS,FLAC` picks the mounts |
| `harbor-audio-ffmpeg` | the same through `%ffmpeg`; `--with MP3,AAC` |
| `icecast-server-audio` | ffmpeg source clients push into `icecast.server`; each listener gets a live remux (`dedicated_encoder=true`); `--with MP3,OPUS,FLAC` |
| `icecast-reference` | the same encoders pushed with `output.icecast` into a real Icecast 2.4 serving the listeners; `--with MP3,OPUS,FLAC` |
| `icecast-reference-ffmpeg` | the reference with the `%ffmpeg` encoders; `--with MP3,AAC` |
| `harbor-video-remux` | liquidsoap loops a video file without decoding it and remuxes it to matroska per listener |
| `icecast-server-video` | an ffmpeg source client pushes matroska into `icecast.server`, remuxed per listener |

Flac is served as ogg/flac in the audio scenarios: liquidsoap's native `%flac`
encoder has no stream header, so a listener joining a shared native-flac
mount mid-stream never sees the `fLaC` marker and strict decoders refuse it.
The `%ffmpeg` scenarios stick to mp3 and AAC for the same reason: the
`%ffmpeg` encoder cannot replay the ogg header pages to a late joiner, so the
ogg family stays with the native encoders.

## Two machines

Load generation competes with the server for CPU on a single box, which
lowers the ceiling. For a credible number run the server on its own machine
and the listeners elsewhere:

```sh
# server box: start the scenario and record its processes until stopped
./icetest serve --liquidsoap liquidsoap --audio /music scenarios/harbor-audio
# load box:
./icetest run --remote server:8000 --start 1000 --step 1000 --max 20000 scenarios/harbor-audio
# afterwards, on either box: fold the server recording into the run
./icetest report --merge results/harbor-audio/serve-<time>/serve.json results/harbor-audio/<time>
```

`serve` writes a `ready` marker once every mount is up and `serve.json` when
it receives SIGINT or SIGTERM. The merge recomputes every process figure of
each step from the server's samples, so the report reads as if both had run
on one machine.

## Google Cloud

`deploy/bench.sh` does the two-machine run on two Spot VMs in one zone,
talking over internal addresses, and destroys them afterwards. Nothing is
scheduled; each verb is a command you type.

```sh
export GCP_PROJECT=my-project
AUDIO=/path/to/music deploy/bench.sh up   # two c3-standard-8 Debian 13 boxes, liquidsoap from the rolling release
WITH=MP3 deploy/bench.sh run harbor-audio icecast-reference icecast-server-audio
deploy/bench.sh down                       # tofu destroy
```

Needs `gcloud` logged in, `tofu`, and `go`. `up` provisions both boxes through
`deploy/provision.sh` (the liquidsoap `.deb` of `LIQUIDSOAP_RELEASE`, ffmpeg,
icecast2, socket limits), cross-builds `icetest`, and copies it with the
scenarios to both boxes and the audio set to the server. `run` serves each scenario on the server box, ramps
from the load box with `RUN_FLAGS`, fetches both result directories into
`results/cloud/<scenario>/<time>/` and merges them. `MACHINE_TYPE`, `GCP_ZONE`,
`SPOT` and `VIDEO` are the other knobs; see the header of the script.

The ceiling in the cloud is usually the NIC: egress is 2 Gbit/s per vCPU, so
an 8 vCPU box tops out near 16 Gbit/s, about 45k listeners on the three audio
mounts. Pick a larger shape for more.

## Load-generator limits

- One source IP gives about 28k ephemeral ports by default. Raise it with
  `sysctl -w net.ipv4.ip_local_port_range="1024 65535"`, and spread
  connections over several addresses with `--bind 127.0.0.2,127.0.0.3,...`
  (every `127.0.0.0/8` address works on the loopback without configuration).
- `--churn` produces client-side `TIME_WAIT` sockets; `net.ipv4.tcp_tw_reuse=1`
  keeps them from exhausting ports.
- The file-descriptor soft limit is raised to the hard limit at start, for the
  load generator and every process it launches. Check `ulimit -Hn` covers the
  target listener count.
- Memory per listener is about 16 KB plus kernel socket buffers.
