#!/bin/bash
# End-to-end test of rpak against the distribution's (unpatched) flatpak in a
# debian:trixie container. Needs podman and network access; downloads a
# few hundred MB (the freedesktop runtime).
#
# Usage: make e2e    (or: make build && test/e2e/run.sh)
# The log ends up in test/e2e/out/e2e.log.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
image=${RPAK_E2E_IMAGE:-docker.io/library/debian:trixie}

if [ ! -x "$repo/rpak" ]; then
	echo "build rpak first (make build)" >&2
	exit 1
fi
mkdir -p "$here/out"

# --privileged: flatpak runs apply_extra under bubblewrap, which needs to
# create namespaces inside the container.
podman run --rm --privileged \
	-v "$repo/rpak:/usr/local/bin/rpak:ro" \
	-v "$here:/e2e:ro" \
	-v "$here/out:/out" \
	-e http_proxy -e https_proxy -e no_proxy -e HTTP_PROXY -e HTTPS_PROXY -e NO_PROXY \
	"$image" /e2e/in-container.sh 2>&1 | tee "$here/out/e2e.log"
exit "${PIPESTATUS[0]}"
