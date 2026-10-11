#!/usr/bin/env bash
# Runs the container image the way the README does, as its non-root user,
# read-only and without capabilities, with a named data volume and the
# Docker socket: the UI starts on an empty volume, then init, doctor, gen,
# show and export work inside it.
set -euo pipefail
cd "$(dirname "$0")/.."
image="${1:?usage: scripts/container-smoke.sh IMAGE}"
volume="composewright-smoke-$$"
server="composewright-smoke-$$"
source="$(mktemp -d)"
cleanup() {
	docker rm -f "$server" >/dev/null 2>&1 || true
	docker volume rm "$volume" >/dev/null 2>&1 || true
	rm -rf "$source"
}
trap cleanup EXIT
socket_group="$(stat -c '%g' /var/run/docker.sock)"
hardening=(--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges)

user="$(docker image inspect --format '{{.Config.User}}' "$image")"
if [ "$user" != "65532:65532" ]; then
	echo "container: the image runs as '$user', not 65532:65532" >&2
	exit 1
fi
docker run --rm "$image" version

# The image's own command initialises an empty volume, then serves.
docker run -d --name "$server" "${hardening[@]}" -p 127.0.0.1::8484 -v "$volume:/data" "$image" >/dev/null
address="$(docker port "$server" 8484/tcp | head -n 1)"
for _ in $(seq 100); do
	if curl -s -o /dev/null "http://$address/login"; then break; fi
	sleep 0.2
done
curl -sS --fail -o /dev/null "http://$address/setup"
docker rm -f "$server" >/dev/null
echo "container: serve --init started the UI on an empty volume"

# The container's user reads what is mounted into it, so the corpus copy is
# readable by everyone whatever the checkout's own modes are.
cp -r testdata/corpus/miniflux/files/. "$source"
chmod -R a+rX "$source"
run() {
	docker run --rm "${hardening[@]}" -v "$volume:/data" -v /var/run/docker.sock:/var/run/docker.sock --group-add "$socket_group" \
		-v "$source:/source:ro" "$image" "$@"
}
run init >/dev/null
doctor="$(run doctor --json)"
for check in "data and key" "docker compose" "docker daemon"; do
	status="$(jq -r --arg name "$check" '.checks[] | select(.name == $name) | .status' <<<"$doctor")"
	if [ "$status" != "ok" ]; then
		echo "container: doctor's $check check is '$status': $doctor" >&2
		exit 1
	fi
done
run gen --offline --no-llm --no-validate --upstream https://github.com/miniflux/v2 /source >/dev/null
run show miniflux --file compose >/dev/null
run export miniflux --to /data/exports/miniflux >/dev/null
echo "container: init, doctor, gen, show and export work as uid 65532"
