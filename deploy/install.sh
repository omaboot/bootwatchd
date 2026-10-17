#!/bin/sh
# Installs or updates bootwatchd as a plain-HTTP service on a LAN host (development):
#   sudo ./install.sh
# Run from a directory holding the bootwatchd binary and deploy/'s root/. Production (the
# droplet behind a Cloudflare Tunnel) is provision.sh's.
set -eu
cd "$(dirname "$0")"
install -m 0755 bootwatchd /usr/local/bin/bootwatchd
install -m 0644 root/etc/systemd/system/bootwatchd.service /etc/systemd/system/bootwatchd.service
systemctl daemon-reload
systemctl enable bootwatchd
systemctl restart bootwatchd
systemctl --no-pager --lines=3 status bootwatchd
