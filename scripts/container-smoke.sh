#!/usr/bin/env bash
# Runs the container image the way the README does: it starts as root only to
# make its data folder its own and join the Docker socket's group, then runs
# as uid 65532, read-only and with no capabilities. The UI starts on an empty
# volume and on a host folder Docker creates as root, then init, doctor, gen,
# show and export work inside it, all without --group-add.
set -euo pipefail
cd "$(dirname "$0")/.."
image="${1:?usage: scripts/container-smoke.sh IMAGE}"
volume="composewright-smoke-$$"
server="composewright-smoke-$$"
source="$(mktemp -d)"
folders="$(mktemp -d)"
# The data folder ends up uid 65532's, so only a container can remove it.
helper="$(sed -n 's/^FROM \(docker\.io\/library\/docker:[^ ]*\) AS docker$/\1/p' Dockerfile)"
cleanup() {
	docker rm -f "$server" >/dev/null 2>&1 || true
	docker volume rm "$volume" >/dev/null 2>&1 || true
	docker run --rm --entrypoint rm -v "$folders:/folders" "$helper" -rf /folders/data >/dev/null 2>&1 || true
	rm -rf "$source" "$folders"
}
trap cleanup EXIT
hardening=(--read-only --tmpfs /tmp --cap-drop ALL --cap-add CHOWN --cap-add SETGID --cap-add SETUID --security-opt no-new-privileges)

docker run --rm "$image" version

# serve starts the UI on the given volume or folder and checks the server
# process: uid 65532, with no capabilities left.
serve() {
	docker run -d --name "$server" "${hardening[@]}" -p 127.0.0.1::8484 -v "$1:/data" -v /var/run/docker.sock:/var/run/docker.sock "$image" >/dev/null
	address="$(docker port "$server" 8484/tcp | head -n 1)"
	for _ in $(seq 100); do
		if curl -s -o /dev/null "http://$address/login"; then break; fi
		sleep 0.2
	done
	curl -sS --fail -o /dev/null "http://$address/setup"
	process_status="/proc/$(docker inspect --format '{{.State.Pid}}' "$server")/status"
	if ! grep -Eq '^Uid:\s+65532\s' "$process_status" || ! grep -Eq '^CapEff:\s+0+$' "$process_status"; then
		echo "container: the server does not run as uid 65532 without capabilities:" >&2
		grep -E '^(Uid|CapEff):' "$process_status" >&2
		exit 1
	fi
	docker rm -f "$server" >/dev/null
}
serve "$volume"
echo "container: serve --init started the UI on an empty volume, as uid 65532 with no capabilities"
serve "$folders/data"
owner="$(stat -c '%u:%g %a' "$folders/data")"
if [ "$owner" != "65532:65532 700" ]; then
	echo "container: the data folder Docker created is $owner, not 65532:65532 700" >&2
	exit 1
fi
echo "container: a data folder Docker created as root became the app's, mode 700"

# The container's user reads what is mounted into it, so the corpus copy is
# readable by everyone whatever the checkout's own modes are.
cp -r testdata/corpus/miniflux/files/. "$source"
chmod -R a+rX "$source"
run() {
	docker run --rm "${hardening[@]}" -v "$volume:/data" -v /var/run/docker.sock:/var/run/docker.sock \
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
echo "container: init, doctor, gen, show and export work as uid 65532, without --group-add"
