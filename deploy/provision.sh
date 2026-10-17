#!/bin/bash
# Creates (or reconfigures) the bootwatch droplet on DigitalOcean, then builds bootwatchd and
# runs setup.sh on it. Safe to re-run: it's also how a new bootwatchd is deployed.
#
#   deploy/provision.sh [NAME]          default name: $DROPLET
#
# Your deployment's names come from the environment, or from deploy/local.env (untracked; copy
# deploy/local.env.example):
#   DROPLET      the droplet's name
#   DO_CONTEXT   doctl profile
#   DO_PROJECT   DigitalOcean project for the droplet (must exist)
#   DO_REGION    DigitalOcean region, e.g. nyc3
#   ADMIN_KEY    admin public key, registered with DigitalOcean
#   ADMIN_USER   admin login on the droplet (default: $USER)
set -euo pipefail
cd "$(dirname "$0")"
[ ! -f local.env ] || . ./local.env

NAME=${1:-${DROPLET:?set DROPLET in deploy/local.env (copy deploy/local.env.example)}}
CTX=${DO_CONTEXT:?set DO_CONTEXT in deploy/local.env}
PROJECT=${DO_PROJECT:?set DO_PROJECT in deploy/local.env}
REGION=${DO_REGION:?set DO_REGION in deploy/local.env}
ADMIN_USER=${ADMIN_USER:-$USER}
ADMIN_KEY=${ADMIN_KEY:?set ADMIN_KEY in deploy/local.env}
FIREWALL=bootwatch-ssh
TAG=bootwatch
SIZE=s-1vcpu-512mb-10gb # $4 a month

doctl() { command doctl --context "$CTX" "$@"; }
step() { echo "==> $*"; }

[ -f "$ADMIN_KEY" ] || { echo "no admin key at $ADMIN_KEY" >&2; exit 1; }
fp=$(ssh-keygen -E md5 -lf "$ADMIN_KEY" | awk '{print $2}' | sed 's/^MD5://')
doctl compute ssh-key list --format FingerPrint --no-header | grep -qx "$fp" ||
	{ echo "admin key $fp is not registered with DigitalOcean" >&2; exit 1; }

project=$(doctl projects list --format ID,Name --no-header | awk -v p="$PROJECT" '{id=$1; $1=""; sub(/^ /,"")} $0==p {print id}')
[ -n "$project" ] || { echo "DigitalOcean project \"$PROJECT\" not found" >&2; exit 1; }

# Production runs releases: a clean tree at a tag (vX.Y.Z), so /v1/health names exactly what runs.
if [ "${FORCE:-}" != 1 ]; then
	[ -z "$(git -C .. status --porcelain)" ] || { echo "uncommitted changes: commit and tag a release first (or FORCE=1)" >&2; exit 1; }
	git -C .. describe --tags --exact-match HEAD >/dev/null 2>&1 || { echo "HEAD isn't a release tag: git tag vX.Y.Z && git push --tags (or FORCE=1)" >&2; exit 1; }
fi
step "building bootwatchd $(git -C .. describe --tags --always --dirty)"
(cd .. && make build >/dev/null)

ip=$(doctl compute droplet list --tag-name $TAG --format Name,PublicIPv4 --no-header | awk -v n="$NAME" '$1==n {print $2}')
if [ -z "$ip" ]; then
	step "creating droplet $NAME ($REGION, $SIZE, debian-13-x64)"
	ud=$(mktemp)
	trap 'rm -f "$ud"' EXIT
	sed -e "s|ADMIN_USER|$ADMIN_USER|" -e "s|ADMIN_KEY|$(cat "$ADMIN_KEY")|" cloud-init.yaml >"$ud"
	doctl compute droplet create "$NAME" --region "$REGION" --size $SIZE --image debian-13-x64 \
		--ssh-keys "$fp" --tag-name $TAG --project-id "$project" --enable-monitoring --enable-ipv6 \
		--user-data-file "$ud" --wait >/dev/null
	ip=$(doctl compute droplet list --tag-name $TAG --format Name,PublicIPv4 --no-header | awk -v n="$NAME" '$1==n {print $2}')
fi
echo "    $NAME is at $ip"

if ! doctl compute firewall list --format Name --no-header | grep -qx $FIREWALL; then
	step "creating firewall $FIREWALL (inbound SSH only: Cloudflare comes in through the tunnel)"
	doctl compute firewall create --name $FIREWALL --tag-names $TAG \
		--inbound-rules "protocol:tcp,ports:22,address:0.0.0.0/0,address:::/0" \
		--outbound-rules "protocol:tcp,ports:all,address:0.0.0.0/0,address:::/0 protocol:udp,ports:all,address:0.0.0.0/0,address:::/0 protocol:icmp,address:0.0.0.0/0,address:::/0" >/dev/null
fi

# Offer only the admin key: the droplet allows 3 auth attempts.
ssh_opts=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -o IdentitiesOnly=yes -i "$ADMIN_KEY")
step "waiting for SSH and cloud-init"
for _ in $(seq 60); do
	ssh "${ssh_opts[@]}" "$ADMIN_USER@$ip" true 2>/dev/null && break
	sleep 5
done
ssh "${ssh_opts[@]}" "$ADMIN_USER@$ip" "cloud-init status --wait >/dev/null; true"

step "running setup.sh"
tar -czf - setup.sh root -C ../dist bootwatchd | ssh "${ssh_opts[@]}" "$ADMIN_USER@$ip" \
	"rm -rf /tmp/bootwatch-setup && mkdir /tmp/bootwatch-setup && tar -xzf - -C /tmp/bootwatch-setup &&
	 sudo ADMIN_USER='$ADMIN_USER' /tmp/bootwatch-setup/setup.sh && rm -rf /tmp/bootwatch-setup"
step "done: ssh $ADMIN_USER@$ip"
