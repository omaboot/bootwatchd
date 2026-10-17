#!/bin/bash
# Configures Cloudflare for boot.watch's test endpoints. Safe to re-run: every step looks up
# what exists and updates it in place.
#
#   tunnel "bootwatchd" → http://127.0.0.1:8470 for boot.watch/v1/* only (everything else 404s
#   at the tunnel); DNS boot.watch → the tunnel, proxied; the zone's redirect rules keep their
#   targets but leave boot.watch/v1/* and status.boot.watch (bootwatcherd's page) alone; Always
#   Use HTTPS off (the plain-HTTP echo is how a client spots a sign-in page; the redirect rules
#   still send every other plain-HTTP page away); minimum TLS 1.2; the tunnel token on the
#   droplet.
#
# Environment:
#   CF_API_TOKEN    API token: Account "Cloudflare Tunnel: Edit"; Zone (boot.watch): "Zone: Read",
#                   "DNS: Edit", "Zone Settings: Edit", "Single Redirect: Edit"
#   CF_ACCOUNT_ID   Cloudflare account ID (default: boot.watch's account)
#   DROPLET, DO_CONTEXT, ADMIN_KEY, ADMIN_USER as in provision.sh (or deploy/local.env)
set -euo pipefail
local_env="$(dirname "$0")/local.env"
[ ! -f "$local_env" ] || . "$local_env"
: "${DROPLET:?set DROPLET in deploy/local.env (copy deploy/local.env.example)}"
: "${DO_CONTEXT:?set DO_CONTEXT in deploy/local.env (copy deploy/local.env.example)}"
: "${ADMIN_KEY:?set ADMIN_KEY in deploy/local.env (copy deploy/local.env.example)}"

ZONE_NAME=boot.watch
TUNNEL_NAME=bootwatchd
ORIGIN=http://127.0.0.1:8470
: "${CF_API_TOKEN:?}"
API=https://api.cloudflare.com/client/v4

step() { echo "==> $*"; }

# cf METHOD URL [JSON]: call the API and print .result, failing on errors.
cf() {
	local out
	out=$(curl -sS -X "$1" "$2" -H "Authorization: Bearer $CF_API_TOKEN" -H "Content-Type: application/json" ${3:+--data "$3"})
	if ! jq -e .success >/dev/null <<<"$out"; then
		echo "Cloudflare API: $1 $2 failed:" >&2
		jq -c '.errors' <<<"$out" >&2 || echo "$out" >&2
		return 1
	fi
	jq -c .result <<<"$out"
}

step "zone"
zone_json=$(cf GET "$API/zones?name=$ZONE_NAME" | jq -c '.[0] // empty')
[ -n "$zone_json" ] || { echo "zone $ZONE_NAME not found for this token" >&2; exit 1; }
zone=$(jq -r .id <<<"$zone_json")
CF_ACCOUNT_ID=${CF_ACCOUNT_ID:-$(jq -r .account.id <<<"$zone_json")}
ACCT=$API/accounts/$CF_ACCOUNT_ID
echo "    $zone"

step "tunnel $TUNNEL_NAME"
tunnel=$(cf GET "$ACCT/cfd_tunnel?name=$TUNNEL_NAME&is_deleted=false" | jq -r '.[0].id // empty')
if [ -z "$tunnel" ]; then
	tunnel=$(cf POST "$ACCT/cfd_tunnel" "{\"name\":\"$TUNNEL_NAME\",\"config_src\":\"cloudflare\"}" | jq -r .id)
fi
echo "    $tunnel"
cf PUT "$ACCT/cfd_tunnel/$tunnel/configurations" "$(jq -nc --arg o "$ORIGIN" --arg z "$ZONE_NAME" '{config: {ingress: [
	{hostname: $z, path: "^/v1/", service: $o},
	{service: "http_status:404"}]}}')" >/dev/null

step "DNS"
# The apex points at the tunnel; whatever stood there for the redirect (A/AAAA/CNAME) goes.
target="$tunnel.cfargotunnel.com"
have=$(cf GET "$API/zones/$zone/dns_records?name=$ZONE_NAME")
jq -r --arg t "$target" '.[] | select(.type == "A" or .type == "AAAA" or (.type == "CNAME" and .content != $t)) | .id' <<<"$have" |
	while read -r id; do cf DELETE "$API/zones/$zone/dns_records/$id" >/dev/null; done
if ! jq -e --arg t "$target" 'any(.[]; .type == "CNAME" and .content == $t and .proxied)' <<<"$have" >/dev/null; then
	cf POST "$API/zones/$zone/dns_records" "$(jq -nc --arg n "$ZONE_NAME" --arg c "$target" \
		'{type: "CNAME", name: $n, content: $c, proxied: true, comment: "Cloudflare Tunnel bootwatchd"}')" >/dev/null
fi
echo "    $ZONE_NAME → tunnel"

step "zone settings"
for kv in always_use_https:off min_tls_version:1.2; do
	cf PATCH "$API/zones/$zone/settings/${kv%%:*}" "{\"value\":\"${kv#*:}\"}" >/dev/null
done

step "redirect rules leave boot.watch/v1/* and status.boot.watch alone"
# status.boot.watch is bootwatcherd's status page (its own repo and tunnel)
guard="not (http.host eq \"$ZONE_NAME\" and starts_with(http.request.uri.path, \"/v1/\")) and http.host ne \"status.$ZONE_NAME\""
old="not (http.host eq \"$ZONE_NAME\" and starts_with(http.request.uri.path, \"/v1/\"))" # before status.boot.watch
rs=$(cf GET "$API/zones/$zone/rulesets/phases/http_request_dynamic_redirect/entrypoint" 2>/dev/null || echo '{}')
n=$(jq '.rules // [] | length' <<<"$rs")
[ "$n" -gt 0 ] || { echo "    no redirect rules in the zone: the boot.watch redirect lives elsewhere (Page Rules or Bulk Redirects); exclude /v1/* there" >&2; exit 1; }
# a rule that matches everything ("true", which can't be combined) becomes the guard alone, as
# does a rule that was only the older guard
rules=$(jq -c --arg g "$guard" --arg old "$old" '[.rules[] | {description, expression, action, action_parameters, enabled}
	| if (.expression | contains($g)) then .
	  elif .expression == "true" or .expression == $old then .expression = $g
	  else .expression = "(" + .expression + ") and " + $g end]' <<<"$rs")
cf PUT "$API/zones/$zone/rulesets/phases/http_request_dynamic_redirect/entrypoint" "{\"rules\":$rules}" >/dev/null
jq -r '.[] | "    " + (.description // "rule") + ": " + .expression' <<<"$rules"

step "tunnel token → droplet"
token=$(cf GET "$ACCT/cfd_tunnel/$tunnel/token" | jq -r .)
name=${DROPLET:?set DROPLET in deploy/local.env}
ip=$(doctl --context "${DO_CONTEXT:?set DO_CONTEXT in deploy/local.env}" compute droplet list --tag-name bootwatch --format Name,PublicIPv4 --no-header | awk -v n="$name" '$1==n {print $2}')
[ -n "$ip" ] || { echo "droplet $name not found" >&2; exit 1; }
printf 'TUNNEL_TOKEN=%s\n' "$token" | ssh -o IdentitiesOnly=yes -i "${ADMIN_KEY:?set ADMIN_KEY in deploy/local.env}" "${ADMIN_USER:-$USER}@$ip" \
	"sudo install -m 0600 /dev/stdin /etc/bootwatch/tunnel.env && sudo systemctl enable --now cloudflared && sudo systemctl restart cloudflared"
echo "    cloudflared running on $name ($ip)"
