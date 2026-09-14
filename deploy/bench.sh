#!/bin/sh
# Runs the listener tests on two Google Cloud VMs and tears them down.
#
#   deploy/bench.sh up                      create both boxes, provision, copy binary, scenarios and audio
#   deploy/bench.sh run <scenario>...       serve on the server box, ramp from the load box, fetch results
#   deploy/bench.sh liquidsoap <file.deb>   install a liquidsoap package on the server box (e.g. a CI artifact)
#   deploy/bench.sh down                    destroy both boxes
#
# Environment:
#   GCP_PROJECT   (required)          AUDIO      local audio directory (required by up)
#   GCP_ZONE      us-central1-a       VIDEO      local video file, copied when set
#   MACHINE_TYPE  c3-standard-8       RUN_FLAGS  icetest run flags (default: --start 1000 --step 1000 --max 30000 --hold 60s)
#   LOAD_MACHINE_TYPE c3-standard-22  BIND       source addresses for the listeners (default: the load box's alias range)
#   SPOT          true                LIQUIDSOAP_RELEASE  rolling-release-v2.5.x
#   WITH          optional scenario parts for serve and run, e.g. FLAC
set -eu
cd "$(dirname "$0")/.."
: "${GCP_PROJECT:?set GCP_PROJECT}"
ZONE="${GCP_ZONE:-us-central1-a}"
RUN_FLAGS="${RUN_FLAGS:---start 1000 --step 1000 --max 30000 --hold 60s}"
WITH_FLAG=""; [ -z "${WITH:-}" ] || WITH_FLAG="--with $WITH"
TF="tofu -chdir=deploy/gcp"
TFVARS="-var project=$GCP_PROJECT -var zone=$ZONE -var machine_type=${MACHINE_TYPE:-c3-standard-8} -var load_machine_type=${LOAD_MACHINE_TYPE:-c3-standard-22} -var spot=${SPOT:-true} -var liquidsoap_release=${LIQUIDSOAP_RELEASE:-rolling-release-v2.5.x}"

ssh_box() { # ssh_box <role> <command>
  box="icetest-$1"; shift
  gcloud compute ssh "$box" --project "$GCP_PROJECT" --zone "$ZONE" --quiet --command "$*"
}
# The alias addresses of the load box, one per line.
alias_addresses() {
  python3 -c "import ipaddress, sys; print('\n'.join(str(h) for h in ipaddress.ip_network(sys.argv[1]).hosts()))" "$($TF output -raw load_alias_range)"
}

# Every listener source address: the primary one plus the aliases.
bind_addresses() {
  { ssh_box load "hostname -I | cut -d' ' -f1"; alias_addresses; } | tr -d '\r' | paste -sd, -
}

push_tar() { # push_tar <role> <local dir> <remote dir>
  # Uncompressed: the payload is mostly mp3, and a dot per 10 MB shows the upload moving.
  echo "  $(du -sm "$2" | cut -f1) MB to icetest-$1:$3"
  tar c --checkpoint=1000 --checkpoint-action=ttyout=. -C "$2" . | gcloud compute ssh "icetest-$1" --project "$GCP_PROJECT" --zone "$ZONE" --quiet --command "mkdir -p '$3' && tar x -C '$3'"
  echo
}

up() {
  : "${AUDIO:?set AUDIO to the local directory of audio files}"
  [ -d "$AUDIO" ] || { echo "AUDIO=$AUDIO is not a directory" >&2; exit 2; }
  [ -z "${VIDEO:-}" ] || [ -f "$VIDEO" ] || { echo "VIDEO=$VIDEO is not a file" >&2; exit 2; }
  $TF init -input=false >/dev/null
  $TF apply -input=false -auto-approve $TFVARS
  for role in server load; do
    printf 'waiting for ssh on icetest-%s ' $role
    until ssh_box $role true >/dev/null 2>&1; do printf .; sleep 5; done
    echo
    # The startup script also runs at boot; the run below waits on its lock
    # and exits at once if the boot run already finished.
    echo "provisioning icetest-$role (apt, ffmpeg, icecast2, the liquidsoap .deb: a few minutes)"
    ssh_box $role 'sudo google_metadata_script_runner startup 2>&1 | grep -v "^$"'
  done
  # Alias addresses are routed to the box; the kernel also has to own them to
  # bind them as sources.
  ssh_box load "dev=\$(ip route | awk '/default/ {print \$5; exit}'); for ip in $(alias_addresses | paste -sd' ' -); do sudo ip addr add \$ip/32 dev \$dev 2>/dev/null || true; done; ip -4 addr show dev \$dev | grep -c inet"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/icetest-linux .
  for role in server load; do
    gcloud compute scp /tmp/icetest-linux "icetest-$role:icetest" --project "$GCP_PROJECT" --zone "$ZONE" --quiet
    push_tar $role scenarios scenarios
  done
  echo "copying audio"
  push_tar server "$AUDIO" audio
  if [ -n "${VIDEO:-}" ]; then
    gcloud compute scp "$VIDEO" "icetest-server:video" --project "$GCP_PROJECT" --zone "$ZONE" --quiet
  fi
  ssh_box server 'liquidsoap --version; ulimit -n'
}

run() {
  server_ip="$($TF output -raw server_internal_ip)"
  bind="${BIND:-$(bind_addresses)}"
  for scenario in "$@"; do
    stamp="$(date +%Y%m%d-%H%M%S)"
    out="results/cloud/$scenario/$stamp"
    mkdir -p "$out"
    echo "== $scenario: serving on icetest-server"
    video_flag=""; [ -z "${VIDEO:-}" ] || video_flag="--video video"
    ssh_box server "rm -rf results/$scenario; nohup ./icetest serve --liquidsoap liquidsoap --audio audio $video_flag $WITH_FLAG --port 8000 scenarios/$scenario > serve.log 2>&1 &"
    until ssh_box server "ls results/$scenario/serve-*/ready" >/dev/null 2>&1; do sleep 5; done
    echo "== $scenario: ramping from icetest-load"
    ssh_box load "rm -rf results/$scenario; ./icetest run --remote $server_ip:8000 --bind $bind $RUN_FLAGS $WITH_FLAG scenarios/$scenario" || true
    ssh_box server "pkill -INT -x icetest; while pgrep -x icetest >/dev/null; do sleep 1; done"
    for role in load server; do
      gcloud compute scp --recurse "icetest-$role:results/$scenario" "$out/$role" --project "$GCP_PROJECT" --zone "$ZONE" --quiet
    done
    run_dir="$(ls -d "$out"/load/2* | head -1)"
    serve_json="$(ls "$out"/server/serve-*/serve.json | head -1)"
    ./icetest report --merge "$serve_json" "$run_dir" | sed -n '/^## Result/,/^## Steps/p'
    echo "report: $run_dir/report.html"
    echo "server log: $(dirname "$serve_json")/liquidsoap.log"
  done
}

install_liquidsoap() {
  deb="$1"
  [ -f "$deb" ] || { echo "$deb is not a file" >&2; exit 2; }
  gcloud compute scp "$deb" "icetest-server:liquidsoap.deb" --project "$GCP_PROJECT" --zone "$ZONE" --quiet
  ssh_box server 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-downgrades ./liquidsoap.deb >/dev/null && liquidsoap --version'
}

down() {
  $TF destroy -input=false -auto-approve $TFVARS
}

case "${1:-}" in
  up) up ;;
  run) shift; [ $# -gt 0 ] || { echo "usage: bench.sh run <scenario>..." >&2; exit 2; }; run "$@" ;;
  liquidsoap) [ -n "${2:-}" ] || { echo "usage: bench.sh liquidsoap <file.deb>" >&2; exit 2; }; install_liquidsoap "$2" ;;
  down) down ;;
  *) sed -n '2,14p' "$0"; exit 2 ;;
esac
