#!/bin/sh
# GCE startup script for both boxes: the liquidsoap Debian package of the
# given GitHub release, ffmpeg, icecast2, and the socket limits the load test
# needs. Runs at every boot and again from bench.sh, so it is idempotent and
# serialised on a lock.
set -eu
release="${liquidsoap_release}"
exec 9>/var/lock/icetest-provision
flock 9
if [ -f /var/lib/icetest-provisioned ]; then
  echo "already provisioned"
  exit 0
fi

cat > /etc/sysctl.d/90-icetest.conf <<'EOF'
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.core.netdev_max_backlog = 65535
fs.file-max = 4194304
fs.nr_open = 4194304
EOF
cat > /etc/security/limits.d/icetest.conf <<'EOF'
* soft nofile 1048576
* hard nofile 1048576
root soft nofile 1048576
root hard nofile 1048576
EOF
mkdir -p /etc/systemd/system.conf.d
cat > /etc/systemd/system.conf.d/icetest.conf <<'EOF'
[Manager]
DefaultLimitNOFILE=1048576
EOF
sysctl --system >/dev/null

systemctl disable --now unattended-upgrades apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ffmpeg icecast2 curl ca-certificates
systemctl disable --now icecast2 2>/dev/null || true
# The reference scenario's icecast.xml uses the Fedora layout of the web files.
[ -e /usr/share/icecast ] || ln -s /usr/share/icecast2 /usr/share/icecast

# The asset name carries the commit hash, so it is looked up by pattern.
arch="$(dpkg --print-architecture)"
codename="$(. /etc/os-release && echo "$VERSION_CODENAME")"
url="$(curl -fsSL "https://api.github.com/repos/savonet/liquidsoap/releases/tags/$release" \
  | grep -o "https://[^\"]*/liquidsoap-[0-9a-f]*_[0-9.]*-debian-$codename-ocaml[0-9.]*-[0-9]*_$arch\.deb" | head -1)"
[ -n "$url" ] || { echo "no liquidsoap package for debian-$codename $arch in $release" >&2; exit 1; }
echo "installing $url"
curl -fsSL -o /tmp/liquidsoap.deb "$url"
apt-get install -y /tmp/liquidsoap.deb
liquidsoap --version

touch /var/lib/icetest-provisioned
echo "provisioned"
