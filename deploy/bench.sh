#!/bin/sh
# Runs the listener tests on Google Cloud VMs, one server and one or more load
# boxes, and tears them down.
#
#   deploy/bench.sh up                      create the boxes, provision, copy binary, scenarios and audio
#   deploy/bench.sh run <scenario>...       serve on the server box, ramp from every load box, fetch and combine
#   deploy/bench.sh liquidsoap <file.deb>   install a liquidsoap package on the server box (e.g. a CI artifact)
#   deploy/bench.sh down                    destroy every box
#
# Environment:
#   GCP_PROJECT   (required)          AUDIO      local audio directory (required by up)
#   GCP_ZONE      us-central1-a       VIDEO      local video file, copied when set
#   MACHINE_TYPE  c3-standard-8       LOAD_COUNT 1     LOAD_MACHINE_TYPE c3-standard-8
#   SPOT          true                LIQUIDSOAP_RELEASE  rolling-release-v2.5.x
#   START 1000  STEP 1000  MAX 30000  HOLD 60s   the ramp, in listeners over all load boxes
#   RUN_FLAGS     extra icetest run flags, e.g. --connect-rate 250 --churn 10m
#   WITH          optional scenario parts for serve and run, e.g. MP3,OPUS
set -eu
cd "$(dirname "$0")/.."
: "${GCP_PROJECT:?set GCP_PROJECT}"
ZONE="${GCP_ZONE:-us-central1-a}"
LOAD_COUNT="${LOAD_COUNT:-1}"
START="${START:-1000}"; STEP="${STEP:-1000}"; MAX="${MAX:-30000}"; HOLD="${HOLD:-60s}"
RUN_FLAGS="${RUN_FLAGS:-}"
WITH_FLAG=""; [ -z "${WITH:-}" ] || WITH_FLAG="--with $WITH"
TF="tofu -chdir=deploy/gcp"
TFVARS="-var project=$GCP_PROJECT -var zone=$ZONE -var machine_type=${MACHINE_TYPE:-c3-standard-8} -var load_count=$LOAD_COUNT -var load_machine_type=${LOAD_MACHINE_TYPE:-c3-standard-8} -var spot=${SPOT:-true} -var liquidsoap_release=${LIQUIDSOAP_RELEASE:-rolling-release-v2.5.x}"

ssh_box() { # ssh_box <role> <command>
  box="icetest-$1"; shift
  gcloud compute ssh "$box" --project "$GCP_PROJECT" --zone "$ZONE" --quiet --command "$*"
}
scp_to() { # scp_to <role> <local> <remote>
  gcloud compute scp "$2" "icetest-$1:$3" --project "$GCP_PROJECT" --zone "$ZONE" --quiet
}
push_tar() { # push_tar <role> <local dir> <remote dir>
  echo "  $(du -sm "$2" | cut -f1) MB to icetest-$1:$3"
  tar c --checkpoint=1000 --checkpoint-action=ttyout=. -C "$2" . | gcloud compute ssh "icetest-$1" --project "$GCP_PROJECT" --zone "$ZONE" --quiet --command "mkdir -p '$3' && tar x -C '$3'"
  echo
}
load_boxes() { $TF output -json load_boxes | python3 -c "import json,sys; print(' '.join(json.load(sys.stdin)))"; }

# The alias addresses of a load box, one per line. Read from the instance:
# the OpenTofu state only knows the allocated range after a refresh.
alias_addresses() { # alias_addresses <role>
  range="$(gcloud compute instances describe "icetest-$1" --project "$GCP_PROJECT" --zone "$ZONE" --format='value(networkInterfaces[0].aliasIpRanges[0].ipCidrRange)')"
  python3 -c "import ipaddress, sys; print('\n'.join(str(h) for h in ipaddress.ip_network(sys.argv[1]).hosts()))" "$range"
}

# Every listener source address of a load box: the primary one plus the aliases.
bind_addresses() { # bind_addresses <role>
  { ssh_box "$1" "hostname -I | cut -d' ' -f1"; alias_addresses "$1"; } | tr -d '\r' | paste -sd, -
}

up() {
  : "${AUDIO:?set AUDIO to the local directory of audio files}"
  [ -d "$AUDIO" ] || { echo "AUDIO=$AUDIO is not a directory" >&2; exit 2; }
  [ -z "${VIDEO:-}" ] || [ -f "$VIDEO" ] || { echo "VIDEO=$VIDEO is not a file" >&2; exit 2; }
  $TF init -input=false >/dev/null
  $TF apply -input=false -auto-approve $TFVARS
  for role in server $(load_boxes); do
    printf 'waiting for ssh on icetest-%s ' "$role"
    until ssh_box "$role" true >/dev/null 2>&1; do printf .; sleep 5; done
    echo
    # The startup script also runs at boot; the run below waits on its lock
    # and exits at once if the boot run already finished.
    echo "provisioning icetest-$role (apt, ffmpeg, icecast2, the liquidsoap .deb: a few minutes)"
    ssh_box "$role" 'sudo google_metadata_script_runner startup 2>&1 | grep -v "^$"'
  done
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/icetest-linux .
  for role in server $(load_boxes); do
    scp_to "$role" /tmp/icetest-linux icetest
    push_tar "$role" scenarios scenarios
  done
  for role in $(load_boxes); do
    # Alias addresses are routed to the box; the kernel also has to own them
    # to bind them as sources.
    ssh_box "$role" "dev=\$(ip route | awk '/default/ {print \$5; exit}'); for ip in $(alias_addresses "$role" | paste -sd' ' -); do sudo ip addr add \$ip/32 dev \$dev 2>/dev/null || true; done; echo \"icetest-$role: \$(ip -4 addr show dev \$dev | grep -c inet) addresses\""
  done
  echo "copying audio"
  push_tar server "$AUDIO" audio
  if [ -n "${VIDEO:-}" ]; then scp_to server "$VIDEO" video; fi
  ssh_box server 'liquidsoap --version | head -1; ulimit -n'
}

# Each load box drives an equal share of every step's target.
share() { echo $(( ($1 + LOAD_COUNT - 1) / LOAD_COUNT )); }

run() {
  server_ip="$($TF output -raw server_internal_ip)"
  boxes="$(load_boxes)"
  for scenario in "$@"; do
    stamp="$(date +%Y%m%d-%H%M%S)"
    out="results/cloud/$scenario/$stamp"
    mkdir -p "$out"
    echo "== $scenario: serving on icetest-server"
    video_flag=""; [ -z "${VIDEO:-}" ] || video_flag="--video video"
    ssh_box server "rm -rf results/$scenario; nohup ./icetest serve --liquidsoap liquidsoap --audio audio $video_flag $WITH_FLAG --port 8000 scenarios/$scenario > serve.log 2>&1 &"
    until ssh_box server "ls results/$scenario/serve-*/ready" >/dev/null 2>&1; do sleep 5; done
    echo "== $scenario: ramping $START +$STEP up to $MAX over $LOAD_COUNT load box(es), hold $HOLD"
    for role in $boxes; do
      bind="$(bind_addresses "$role")"
      ssh_box "$role" "rm -rf results/$scenario; ./icetest run --remote $server_ip:8000 --bind $bind --start $(share "$START") --step $(share "$STEP") --max $(share "$MAX") --hold $HOLD $RUN_FLAGS $WITH_FLAG scenarios/$scenario" > "$out/$role.log" 2>&1 &
    done
    # Step lines of every box, as they land; the full output stays in the logs.
    ( tail -q -n +1 -F $(for role in $boxes; do echo "$out/$role.log"; done) 2>/dev/null | grep --line-buffered -E "^step|^  (pass|FAIL)|^icetest:" ) &
    tail_pid=$!
    wait $(jobs -p | grep -v "^$tail_pid$")
    pkill -P $tail_pid 2>/dev/null; kill $tail_pid 2>/dev/null
    ssh_box server "pkill -INT -x icetest; while pgrep -x icetest >/dev/null; do sleep 1; done"
    for role in server $boxes; do
      gcloud compute scp --recurse "icetest-$role:results/$scenario" "$out/$role" --project "$GCP_PROJECT" --zone "$ZONE" --quiet
    done
    serve_json="$(ls "$out"/server/serve-*/serve.json | head -1)"
    run_dirs=""; for role in $boxes; do run_dirs="$run_dirs $(ls -d "$out/$role"/2* | head -1)"; done
    ./icetest combine -o "$out/combined" $run_dirs >/dev/null
    ./icetest report --merge "$serve_json" "$out/combined" | sed -n '/^## Result/,/^## Steps/p'
    echo "report: $out/combined/report.html"
    echo "server log: $(dirname "$serve_json")/liquidsoap.log"
  done
}

install_liquidsoap() {
  deb="$1"
  [ -f "$deb" ] || { echo "$deb is not a file" >&2; exit 2; }
  scp_to server "$deb" liquidsoap.deb
  ssh_box server 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-downgrades ./liquidsoap.deb >/dev/null && liquidsoap --version | head -1'
}

down() {
  $TF destroy -input=false -auto-approve $TFVARS
}

case "${1:-}" in
  up) up ;;
  run) shift; [ $# -gt 0 ] || { echo "usage: bench.sh run <scenario>..." >&2; exit 2; }; run "$@" ;;
  liquidsoap) [ -n "${2:-}" ] || { echo "usage: bench.sh liquidsoap <file.deb>" >&2; exit 2; }; install_liquidsoap "$2" ;;
  down) down ;;
  *) sed -n '2,18p' "$0"; exit 2 ;;
esac
