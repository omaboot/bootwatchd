#!/bin/sh
# The scrub check: fails when a file that goes public holds something that belongs to the
# maintainers' private setup. It scans tracked files and untracked ones that aren't ignored, in
# two parts:
#
#  1. Generic checks, the same for everyone: private-network (RFC 1918) addresses other than the
#     usual example ones (192.168.1.x, 10.0.x.x, 10.1.1.x, 172.16.0.x, 172.16.255.x, and the
#     phone-tethering defaults 172.20.10.x and 192.168.42.x), home-directory paths with a real
#     user name (/home/NAME/, /Users/NAME/; placeholders like "user" or "you" are fine), private
#     key blocks, and DoesItOmarchy report keys (doi_ and a long key; doi_example is fine).
#  2. The maintainers' own names (servers, hosts, home network, machine paths): one extended
#     regular expression per line in a file kept outside the repo, so this check never names
#     them itself: $OMABOOT_SCRUB_PATTERNS, default ~/.config/omaboot/scrub-patterns.txt. Blank
#     lines and lines starting with # are ignored. Without that file this part is skipped, with
#     a notice, and the generic part still runs.
#
# Usage: scrub-check.sh [-x MANIFEST] [-b BASE] [PATHSPEC...]
#   -x MANIFEST  a file (path from the repo's top) listing paths, one per line, # comments, that
#                stay private and are left out of what goes public: both parts skip them.
#   -b BASE      the generic part checks only files changed since BASE (a fork: the upstream
#                files are upstream's). Skipped, with a notice, when BASE isn't in this clone.
#   PATHSPEC     limit the check to these paths (git pathspecs).
#
# A line that must stay as it is can carry the marker "scrub:allow" (prefer rewording it).
# Same file in every OmaBoot? repo; change it in all of them.
set -eu

manifest= base=
while getopts x:b: opt; do
	case $opt in
	x) manifest=$OPTARG ;;
	b) base=$OPTARG ;;
	*) echo "usage: $0 [-x MANIFEST] [-b BASE] [PATHSPEC...]" >&2; exit 2 ;;
	esac
done
shift $((OPTIND - 1))

here=$(cd "$(dirname "$0")" && pwd -P)
top=$(git rev-parse --show-toplevel)
top=$(cd "$top" && pwd -P)
self=${here#"$top"/}/$(basename "$0")
cd "$top"

# The paths every part skips: this script (it spells out the generic patterns) and the manifest's.
skips=":(exclude)$self"
if [ -n "$manifest" ]; then
	[ -r "$manifest" ] || { echo "scrub check: no manifest $manifest" >&2; exit 2; }
	skips="$skips
:(exclude)$manifest
$(grep -v -E '^[[:space:]]*(#|$)' "$manifest" | sed 's/^/:(exclude)/')"
fi

# git grep over the given pathspecs minus the skipped paths; "file:line:text" per hit, marked
# lines dropped. Paths come one per line in $1 (no spaces in our paths' names).
search() { # search PATHS GREP-ARGS...
	sp=$1
	shift
	# shellcheck disable=SC2086 # one pathspec per line, not globbed here
	(set -f; IFS='
'
		git grep -n -I --untracked "$@" -- $sp) | grep -v 'scrub:allow' || true
}

paths=$(printf '%s\n' "$@")
[ -n "$paths" ] || paths=.
paths="$paths
$skips"

fail=0
oct='[0-9][0-9]?[0-9]?' # no {m,n}: some awks (older mawk) don't take intervals

# 1. Generic.
gpaths=$paths
if [ -n "$base" ]; then
	if git rev-parse -q --verify "$base^{commit}" >/dev/null; then
		changed=$( (git diff --name-only --diff-filter=d "$(git merge-base "$base" HEAD)" -- "$@"
			git ls-files --others --exclude-standard -- "$@") | sort -u)
		gpaths="$changed
$skips"
		[ -n "$changed" ] || gpaths=
	else
		echo "scrub check: generic part skipped: $base isn't in this clone"
		gpaths=
	fi
fi
if [ -n "$gpaths" ]; then
	net="(^|[^0-9.])(10\.$oct\.$oct\.$oct|172\.(1[6-9]|2[0-9]|3[01])\.$oct\.$oct|192\.168\.$oct\.$oct)([^0-9]|$)"
	netok="(^|[^0-9.])(192\.168\.1|192\.168\.42|172\.20\.10|172\.16\.0|172\.16\.255|10\.1\.1|10\.0\.$oct)\.$oct"
	home="(/home|/Users)/[A-Za-z0-9._-]+"
	homeok="(/home|/Users)/(user|username|you|yourname|me|name|example|alice|bob|liveuser)([^A-Za-z0-9._-]|$)"
	key="-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY-----"
	k=[A-Za-z0-9]
	doi="(^|[^A-Za-z0-9_])doi_$k$k$k$k$k$k$k$k$k$k$k$k"
	doiok="doi_example"
	hits=$(search "$gpaths" -E -e "$net" -e "$home" -e "$key" -e "$doi" |
		NET=$net NETOK=$netok HOME_=$home HOMEOK=$homeok KEY=$key DOI=$doi DOIOK=$doiok awk '
		{
			l = $0
			gsub(ENVIRON["NETOK"], "", l); gsub(ENVIRON["HOMEOK"], "", l); gsub(ENVIRON["DOIOK"], "", l)
			if (l ~ ENVIRON["NET"] || l ~ ENVIRON["HOME_"] || l ~ ENVIRON["KEY"] || l ~ ENVIRON["DOI"]) print
		}')
	if [ -n "$hits" ]; then
		printf '%s\n' "$hits"
		echo "scrub check: the lines above hold a private address, home path, private key or report key" >&2
		fail=1
	fi
fi

# 2. The maintainers' own names.
f=${OMABOOT_SCRUB_PATTERNS:-$HOME/.config/omaboot/scrub-patterns.txt}
if [ ! -r "$f" ]; then
	echo "scrub check: private patterns skipped (no $f)"
else
	hits=$(grep -v -E '^[[:space:]]*(#|$)' "$f" | search "$paths" -E -f -)
	if [ -n "$hits" ]; then
		printf '%s\n' "$hits"
		echo "scrub check: the lines above name private infrastructure" >&2
		fail=1
	fi
fi

[ $fail = 0 ] && echo "scrub check: clean"
exit $fail
