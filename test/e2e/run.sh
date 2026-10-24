#!/bin/bash
# End-to-end test of rpak against the distribution's (unpatched) flatpak in a
# container (podman). Needs network access.
#
# The image (Containerfile) is built once and tagged with a hash of the
# Containerfile and the base image. The runtime of the test apps lives in the
# system installation of a podman volume: downloaded the first time (a few
# hundred MB), then only checked for updates. Everything the test installs
# goes --user into a fresh directory in the container and is discarded, and
# the extra-data are downloaded again every run.
#
# Usage: make e2e                (or: make build && test/e2e/run.sh)
#        E2E_FRESH=1 make e2e    build the image and the runtime from scratch,
#                                in a throwaway image and volume
#        make e2e-clean          (run.sh --clean) remove the cached image and volume
# RPAK_E2E_IMAGE picks another base image. The log ends up in test/e2e/out/e2e.log.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
base=${RPAK_E2E_IMAGE:-docker.io/library/debian:trixie}
name=localhost/rpak-e2e
volume=rpak-e2e-runtime
# Building the image and writing the volume take the lock exclusively, a test
# run holds it shared: runs can test side by side, but the runtime does not
# change under a running test.
lock=${XDG_RUNTIME_DIR:-/tmp}/$volume.lock

lock() { # -x or -s, on fd 9
	flock -n "$1" 9 && return
	echo "waiting for another e2e run ($lock)"
	flock "$1" 9
}

if [ "${1:-}" = --clean ]; then
	exec 9>"$lock"
	lock -x
	! podman volume exists "$volume" || podman volume rm "$volume"
	images=$(podman images --filter "reference=$name" --format '{{.Repository}}:{{.Tag}}')
	[ -z "$images" ] || podman rmi $images
	exit
fi

if [ ! -x "$repo/rpak" ]; then
	echo "build rpak first (make build)" >&2
	exit 1
fi
mkdir -p "$here/out"
exec > >(tee "$here/out/e2e.log") 2>&1

if [ -n "${E2E_FRESH:-}" ]; then
	image=$name:fresh-$$
	volume=$volume-fresh-$$
	cleanup() {
		! podman volume exists "$volume" || podman volume rm "$volume" >/dev/null || true
		podman rmi --ignore "$image" >/dev/null || true
	}
	trap cleanup EXIT
	build=(--no-cache --pull=always)
else
	image=$name:$(cat "$here/Containerfile" - <<<"$base" | sha256sum | cut -c1-12)
	build=()
	exec 9>"$lock"
	lock -x
fi

if podman image exists "$image"; then
	echo "image: $image (cached)"
else
	echo "image: building $image from $base"
	podman build "${build[@]}" --build-arg BASE="$base" -t "$image" -f "$here/Containerfile" "$here" 9>&-
fi

# --privileged: flatpak runs apply_extra and triggers under bubblewrap, which
# needs to create namespaces inside the container.
run() {
	podman run --rm --privileged \
		-v "$repo/rpak:/usr/local/bin/rpak:ro" \
		-v "$here:/e2e:ro" \
		-v "$here/out:/out" \
		-e http_proxy -e https_proxy -e no_proxy -e HTTP_PROXY -e HTTPS_PROXY -e NO_PROXY \
		"$@" 9>&-
}

SECONDS=0
run -v "$volume:/var/lib/flatpak" "$image" /e2e/runtime.sh
echo "runtime: ready after ${SECONDS}s"
[ -n "${E2E_FRESH:-}" ] || lock -s

# Read-only: the test installs --user only and must not touch the runtime.
SECONDS=0
code=0
run -v "$volume:/var/lib/flatpak:ro" "$image" /e2e/in-container.sh || code=$?
echo "test: ${SECONDS}s"
exit "$code"
