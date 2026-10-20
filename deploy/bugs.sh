#!/bin/bash
# Turns on the droplet's bug report endpoint (/v1/bugreport, bugs.go): the upload keys and the
# R2 bucket's write token go to /etc/bootwatch as root-only files, and a drop-in hands them to
# bootwatchd as systemd credentials. Run after provision.sh, and again when a key or the token
# changes:
#   deploy/bugs.sh
# Reads, from outside any repository (mode 600): BUG_R2 (the R2 env file: R2_ACCOUNT_ID,
# R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET) and BUG_KEYS (files of one key each, named
# for the images that carry them).
set -euo pipefail
cd "$(dirname "$0")"
[ -f local.env ] && . ./local.env
r2=${BUG_R2:-$HOME/.config/omaboot/bugs-write.env}
keys=${BUG_KEYS:-"$HOME/.config/omaboot/bugkey-dev $HOME/.config/omaboot/bugkey-release"}
for k in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET; do
	grep -q "^$k=." "$r2" || { echo "$r2: no $k" >&2; exit 1; }
done
name=${DROPLET:?set DROPLET in deploy/local.env}
ip=$(doctl --context "${DO_CONTEXT:?set DO_CONTEXT in deploy/local.env}" compute droplet list --tag-name bootwatch --format Name,PublicIPv4 --no-header | awk -v n="$name" '$1==n {print $2}')
[ -n "$ip" ] || { echo "no droplet $name" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077
for f in $keys; do # bugkey-dev → "dev <key>"
	printf '%s %s\n' "$(basename "$f" | sed 's/^bugkey-//')" "$(tr -d '\n' <"$f")"
done >"$work/bug-keys"
cp "$r2" "$work/bugs-r2.env"
cat >"$work/bugs.conf" <<'EOF'
# /etc/systemd/system/bootwatchd.service.d/bugs.conf, from deploy/bugs.sh: the bug report
# endpoint's keys and its R2 write token, as credentials only bootwatchd can read.
[Service]
LoadCredential=bug-keys:/etc/bootwatch/bug-keys
LoadCredential=bugs-r2:/etc/bootwatch/bugs-r2.env
EOF
tar -C "$work" -czf - bug-keys bugs-r2.env bugs.conf | ssh -o IdentitiesOnly=yes -i "${ADMIN_KEY:?set ADMIN_KEY in deploy/local.env}" "${ADMIN_USER:-$USER}@$ip" \
	'set -e; d=$(mktemp -d); tar -xzf - -C "$d"
	 sudo install -o root -g root -m 0600 "$d/bug-keys" /etc/bootwatch/bug-keys
	 sudo install -o root -g root -m 0600 "$d/bugs-r2.env" /etc/bootwatch/bugs-r2.env
	 sudo install -D -o root -g root -m 0644 "$d/bugs.conf" /etc/systemd/system/bootwatchd.service.d/bugs.conf
	 rm -r "$d"
	 sudo systemctl daemon-reload && sudo systemctl restart bootwatchd && sleep 1
	 sudo journalctl -u bootwatchd -n 1 --no-pager -o cat
	 curl -fsS 127.0.0.1:8470/v1/health'
