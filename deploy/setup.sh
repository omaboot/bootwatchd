#!/bin/bash
# Configures a Debian 13 droplet for bootwatchd behind a Cloudflare Tunnel. Idempotent: run it
# again after changing anything under deploy/ or to install a new binary. Run as root from a
# directory holding this script, root/ and the bootwatchd binary; deploy/provision.sh does it.
#
#   ADMIN_USER   the admin login created by cloud-init (required)
set -euo pipefail
cd "$(dirname "$0")"

: "${ADMIN_USER:?set ADMIN_USER}"
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ -x bootwatchd ] || { echo "no bootwatchd binary next to setup.sh" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive

step() { echo "==> $*"; }

step "packages"
apt-get update -q
apt-get install -yq --no-install-recommends ca-certificates curl gnupg unattended-upgrades apt-listchanges

step "cloudflared (Cloudflare's apt repository)"
install -d -m 0755 /usr/share/keyrings
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" >/etc/apt/sources.list.d/cloudflared.list
apt-get update -q
apt-get install -yq cloudflared

step "swap (512 MB)"
if ! swapon --show=NAME --noheadings | grep -qx /swapfile; then
	[ -f /swapfile ] || { fallocate -l 512M /swapfile; chmod 600 /swapfile; mkswap /swapfile; }
	swapon /swapfile
	grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >>/etc/fstab
fi
echo 'vm.swappiness=10' >/etc/sysctl.d/60-swappiness.conf
sysctl -q -p /etc/sysctl.d/60-swappiness.conf

step "files"
install -d -o root -g root -m 0750 /etc/bootwatch
install -m 0755 bootwatchd /usr/local/bin/bootwatchd
(cd root && find . -type f) | while read -r f; do
	f=${f#./}
	install -D -o root -g root -m 0644 "root/$f" "/$f"
done
sed -i "s/ADMIN_USER/$ADMIN_USER/" /etc/ssh/sshd_config.d/50-bootwatch.conf
sshd -t
systemctl reload ssh
systemctl restart systemd-journald
systemctl daemon-reload

step "unattended upgrades"
echo 'unattended-upgrades unattended-upgrades/enable_auto_updates boolean true' | debconf-set-selections
dpkg-reconfigure -f noninteractive unattended-upgrades

step "services"
systemctl enable bootwatchd.service
systemctl restart bootwatchd.service
if [ -f /etc/bootwatch/tunnel.env ]; then
	systemctl enable cloudflared.service
	systemctl restart cloudflared.service
else
	echo "  cloudflared: waiting for /etc/bootwatch/tunnel.env (deploy/cloudflare.sh)"
fi
step "done"
