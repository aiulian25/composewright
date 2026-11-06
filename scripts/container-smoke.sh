#!/usr/bin/env bash
# Only the named outer daemon touches the caller's Docker host. Everything
# tested, including deliberately unrelated sentinel resources, lives inside it.
set -euo pipefail
cd "$(dirname "$0")/.."
image="${1:?usage: scripts/container-smoke.sh IMAGE [ordinary|rootless|proxy]}"
topology="${2:-ordinary}"
case "$topology" in ordinary|rootless|proxy) ;; *) echo 'unknown topology' >&2; exit 2;; esac
logs="${COMPOSEWRIGHT_TEST_LOGS:-$(mktemp -d /tmp/composewright-packaged-XXXXXX)}"
mkdir -p "$logs"
logs="$(realpath "$logs")"
echo "Packaged test logs: $logs"
name="composewright-check-$(date +%s)-$$"
work="$(mktemp -d)"
inner() { docker exec "$name" docker "$@"; }
cleanup() {
	status=$?
	trap - EXIT INT TERM
	set +e
	docker logs "$name" >"$logs/daemon.log" 2>&1
	if docker inspect "$name" >/dev/null 2>&1; then
		# Inventory survives removal of the entire disposable daemon.
		inner logs socket-proxy >"$logs/proxy.log" 2>&1
		inner ps -a >"$logs/final-containers.log" 2>&1
		inner volume ls >"$logs/final-volumes.log" 2>&1
		inner network ls >"$logs/final-networks.log" 2>&1
		docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}' "$name" >"$logs/outer-volumes.list" || status=1
		docker rm -fv "$name" >>"$logs/cleanup.log" 2>&1 || status=1
	fi
	if docker inspect "$name" >/dev/null 2>&1; then status=1; fi
	if [ -f "$logs/outer-volumes.list" ]; then
		while IFS= read -r owned; do
			[ -z "$owned" ] && continue
			if docker volume inspect "$owned" >/dev/null 2>&1; then
				docker volume rm "$owned" >>"$logs/cleanup.log" 2>&1 || status=1
			fi
			if docker volume inspect "$owned" >/dev/null 2>&1; then status=1; fi
		done <"$logs/outer-volumes.list"
	fi
	# No outer networks or named volumes are created. All image-declared
	# volume paths are tmpfs mounts; rm -v also removes anonymous volumes.
	for kind in 'volume ls' 'network ls' 'ps -a'; do
		read -r -a args <<<"$kind"
		remaining="$(docker "${args[@]}" --filter "label=io.composewright.test=$name" -q)" || status=1
		if [ -n "$remaining" ]; then echo "resources survived: $remaining" >>"$logs/cleanup.log"; status=1; fi
	done
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# A rootless daemon has its own user namespace; no host socket, bind-mounted
# data, ports, networks or production daemon resources enter this environment.
daemon=docker.io/library/docker:29.9.0-dind
host=unix:///var/run/docker.sock
if [ "$topology" = rootless ]; then
	daemon=docker.io/library/docker:29.9.0-dind-rootless
	host=unix:///run/user/1000/docker.sock
fi
docker run --rm -d --privileged --name "$name" --label "io.composewright.test=$name" \
	--mount type=tmpfs,destination=/var/lib/docker \
	--mount type=tmpfs,destination=/home/rootless/.local/share/docker,tmpfs-mode=1777 \
	-e DOCKER_HOST="$host" -e DOCKER_TLS_CERTDIR= "$daemon" --storage-driver=vfs >"$logs/daemon.id"
ready=false
for _ in $(seq 120); do
	if inner info >"$logs/info.log" 2>&1; then ready=true; break; fi
	sleep 1
done
[ "$ready" = true ] || { echo 'disposable daemon did not start' >&2; exit 1; }
docker image save "$image" | docker exec -i "$name" docker load >"$logs/load.log"
# Compile native, static tests, then run them with the candidate's exact
# Docker CLI and Compose plugin. /tmp in this client is NOT the daemon's /tmp.
CGO_ENABLED=0 go test -c -o "$work/sandbox.test" ./internal/sandbox
docker cp "$work/sandbox.test" "$name:/tmp/cw-sandbox.test"
cp -r testdata/corpus/miniflux/files "$work/source"
chmod -R a+rX "$work/source"
docker cp "$work/source" "$name:/tmp/cw-source"
socket="${host#unix://}"
gid="$(docker exec "$name" stat -c %g "$socket")"
connection=(-v "$socket:/var/run/docker.sock" -e DOCKER_HOST=unix:///var/run/docker.sock)
if [ "$topology" = proxy ]; then
	# Read the real deployment's proxy image, command allowlists and hardening.
	# Only its host-side bind is replaced by the disposable daemon's socket.
	DOCKER_GID="$gid" docker compose -f deploy/composewright.hardened.yaml config --format json \
		>"$work/deploy.json" 2>"$logs/compose-config.log"
	proxy="$(jq -r '.services["socket-proxy"].image' "$work/deploy.json")"
	mapfile -t command < <(jq -r '.services["socket-proxy"].command[]' "$work/deploy.json")
	inner network create --internal cw-proxy >/dev/null
	inner run -d --name socket-proxy --network cw-proxy --read-only --cap-drop ALL \
		--security-opt no-new-privileges --user "65534:$gid" \
		-v "$socket:/var/run/docker.sock:ro" "$proxy" "${command[@]}" >/dev/null
	connection=(--network cw-proxy --network-alias composewright -e DOCKER_HOST=tcp://socket-proxy:2375)
fi
hardening=(--read-only --tmpfs /tmp --cap-drop ALL --cap-add CHOWN --cap-add SETGID --cap-add SETUID --security-opt no-new-privileges)
run() {
	inner run --rm "${hardening[@]}" "${connection[@]}" -v cw-check-data:/data -v /tmp/cw-source:/source:ro "$image" "$@"
}
# Sentinels must survive every test; never use host-wide prune/label sweeps.
inner volume create sentinel-volume >/dev/null
inner network create sentinel-network >/dev/null
inner run -d --name sentinel-container --network sentinel-network -v sentinel-volume:/data \
	docker.io/library/alpine:3.20 sleep 3600 >/dev/null
inner inspect sentinel-container >"$logs/sentinel.before.json"
# Startup on both volume and daemon-side bind data directories exercises the
# entrypoint's ownership handoff, then a second container reopens the data.
helper="$(sed -n 's/^FROM \(docker\.io\/library\/docker:[^ ]*\) AS docker$/\1/p' Dockerfile)"
serve() {
	inner run -d --name ui "${hardening[@]}" "${connection[@]}" -v "$1:/data" "$image" >/dev/null
	ready=false
	for _ in $(seq 30); do
		if inner run --rm --network container:ui --entrypoint wget "$helper" -qO /dev/null http://127.0.0.1:8484/setup; then ready=true; break; fi
		sleep 1
	done
	inner logs ui >"$logs/ui-${2}.log" 2>&1
	[ "$ready" = true ]
	inner run --rm --pid container:ui --cap-drop ALL --entrypoint cat "$helper" /proc/1/status >"$logs/ui-${2}-process.log"
	grep -Eq '^Uid:[[:space:]]+65532[[:space:]]' "$logs/ui-${2}-process.log"
	grep -Eq '^CapEff:[[:space:]]+0+$' "$logs/ui-${2}-process.log"
	inner rm -f ui >/dev/null
}
serve cw-check-data volume
serve /tmp/owned-data bind
[ "$(inner run --rm --entrypoint stat -v /tmp/owned-data:/data "$helper" -c '%u:%g %a' /data)" = '65532:65532 700' ]
run version >"$logs/version.log"
run init >"$logs/init.log"
run doctor --json >"$logs/doctor.json"
for check in 'data and key' 'docker compose' 'docker daemon'; do
	jq -e --arg name "$check" '.checks[] | select(.name == $name) | .status == "ok"' "$logs/doctor.json" >/dev/null
done
# Import upstream files, persist, reopen in separate containers, and export.
# This offline authoring check is supplemented by REAL validation below.
run gen --offline --no-llm --no-validate --upstream https://github.com/miniflux/v2 /source >"$logs/gen.log"
run show miniflux --file compose >"$logs/compose.before.yaml"
run export miniflux --to /data/exports/miniflux >"$logs/export.log"
run --json validate miniflux --timeout 2m >"$logs/cli-validation.json"
jq -e '.verdict.result == "PASS"' "$logs/cli-validation.json" >/dev/null
serve cw-check-data reopened
run show miniflux --file compose >"$logs/compose.after.yaml"
cmp "$logs/compose.before.yaml" "$logs/compose.after.yaml"
# The test runner runs with the same unprivileged UID and shipped binaries.
# Rootless DinD has no delegated cgroups: its focused suite asserts file
# transfer/cancellation/cleanup; the full suite retains its metrics assertions
# on ordinary and proxy daemons.
tests='^TestDocker'
if [ "$topology" = rootless ]; then tests='^TestDockerPackaged(Files|Cancellation)$'; fi
inner run --rm --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
	--user 65532:65532 --group-add "$gid" "${connection[@]}" \
	-e COMPOSEWRIGHT_DOCKER_TESTS=1 -e COMPOSEWRIGHT_PACKAGED_TESTS=1 \
	-v /tmp/cw-sandbox.test:/sandbox.test:ro --entrypoint /sandbox.test "$image" \
	-test.v -test.timeout=15m -test.run="$tests" \
	2>&1 | tee "$logs/validation.log"
# The packaged suites must remove ALL their containers, networks and volumes.
for kind in 'ps -a' 'volume ls' 'network ls'; do
	read -r -a args <<<"$kind"
	[ -z "$(inner "${args[@]}" --filter label=io.composewright.sandbox -q)" ]
done
inner inspect sentinel-container >"$logs/sentinel.after.json"
# State timestamps can change, but ID, mounts and attached networks cannot.
jq -S '.[0] | {Id,Mounts,networks:.NetworkSettings.Networks}' "$logs/sentinel.before.json" >"$work/before"
jq -S '.[0] | {Id,Mounts,networks:.NetworkSettings.Networks}' "$logs/sentinel.after.json" >"$work/after"
cmp "$work/before" "$work/after"
inner volume inspect sentinel-volume >/dev/null
inner network inspect sentinel-network >/dev/null
inner rm -f sentinel-container >/dev/null
inner volume rm sentinel-volume cw-check-data >/dev/null
inner network rm sentinel-network >/dev/null
if [ "$topology" = proxy ]; then
	inner logs socket-proxy >"$logs/proxy.log" 2>&1
	inner rm -f socket-proxy >/dev/null
	inner network rm cw-proxy >/dev/null
fi
[ -z "$(inner ps -aq)" ]
[ -z "$(inner volume ls -q)" ]
[ -z "$(inner network ls --filter type=custom -q)" ]
echo "Packaged $topology suite passed; removing disposable daemon."
