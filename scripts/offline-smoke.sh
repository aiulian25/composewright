#!/usr/bin/env bash
# Runs init, gen and the web UI on a corpus snapshot inside a network
# namespace with no network at all (unshare -rn): a local directory must need
# no egress, and every page of the UI must work offline.
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "${1:-}" != "--inside" ]; then
	exec unshare -rn "$(pwd)/scripts/offline-smoke.sh" --inside "${1:-bin/composewright}"
fi
binary="$2"
workdir="$(mktemp -d)"
server=""
cleanup() {
	if [ -n "$server" ]; then
		kill "$server" 2>/dev/null || true
		wait "$server" 2>/dev/null || true
	fi
	rm -rf "$workdir"
}
trap cleanup EXIT
run() { "$binary" --config "$workdir/config.yaml" --data-dir "$workdir/data" "$@"; }

run init >/dev/null
run gen --offline --no-llm --no-validate --upstream https://github.com/miniflux/v2 testdata/corpus/miniflux/files >/dev/null
run show miniflux --file compose >/dev/null
echo "offline: init, gen and show work"

# A fresh namespace has its loopback interface down.
ip link set lo up
address=127.0.0.1:18499
base="http://$address"
# Started directly, not through run(): $! must be the server, or cleanup
# kills a wrapper shell and leaves the server running.
"$binary" --config "$workdir/config.yaml" --data-dir "$workdir/data" serve --listen "$address" >"$workdir/serve.out" 2>&1 &
server=$!
for _ in $(seq 100); do
	if curl -s -o /dev/null "$base/login"; then break; fi
	sleep 0.1
done
# Right after start the first account is made from the page, no token.
password="$(head -c 24 /dev/urandom | base64)"
curl -sS --fail -o /dev/null -D "$workdir/headers" --data-urlencode "username=admin" \
	--data-urlencode "password=$password" --data-urlencode "confirm=$password" "$base/setup"
cookie="$(sed -n 's/^[Ss]et-[Cc]ookie: \(cw-session=[^;]*\).*/\1/p' "$workdir/headers")"
if [ -z "$cookie" ]; then
	echo "offline: setup did not start a session" >&2
	exit 1
fi
page() { curl -sS --fail -H "Cookie: $cookie" "$@"; }

for path in / /projects /new /settings; do page "$base$path" >/dev/null; done
csrf="$(page "$base/new" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -n 1)"
job="$(page -o /dev/null -w '%{redirect_url}' --data-urlencode "csrf=$csrf" --data-urlencode "source=$(pwd)/testdata/corpus/miniflux/files" \
	--data-urlencode "name=offline" --data-urlencode "engine=rules" "$base/new")"
# The event stream ends when the job does.
events="$(page --max-time 60 "$job/events?from=0")"
if [[ "$events" != *'"kind":"done"'* || "$events" == *'"failed":true'* ]]; then
	echo "offline: the generation did not finish cleanly: $events" >&2
	exit 1
fi
assets="$(page "$base/projects/offline/review" | grep -o '/static/[^"?]*' | sort -u)"
for path in /projects/offline /projects/offline/analyse /projects/offline/review /projects/offline/files /projects/offline/delete $assets; do
	page "$base$path" >/dev/null
done
for tab in $(page "$base/projects/offline/files" | grep -o '/projects/offline/files?[^"]*' | sed 's/&amp;/\&/g' | sort -u); do
	page "$base$tab" >/dev/null
done
page -o "$workdir/bundle.zip" --data-urlencode "csrf=$csrf" "$base/projects/offline/download"
if [ "$(head -c 2 "$workdir/bundle.zip")" != "PK" ]; then
	echo "offline: the download is not a zip archive" >&2
	exit 1
fi
echo "offline: the web UI works without any network (setup, every page, a generation, the files and the download)"
