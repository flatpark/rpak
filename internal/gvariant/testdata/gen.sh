#!/bin/bash
# Regenerates the fixtures in this directory with GLib (python3-gi) in a
# debian:trixie container. The "real" case comes from a commit pulled by the
# distribution's flatpak: app.audionaut.Audionaut from FlatPark, without
# deploying it or its runtime (about 11 MB download). Needs podman.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
podman run --rm --privileged -v "$here:/out" -e HOST_IDS="$(id -u):$(id -g)" -e http_proxy -e https_proxy -e HTTP_PROXY -e HTTPS_PROXY \
	docker.io/library/debian:trixie bash -euo pipefail -c '
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends flatpak ca-certificates python3-gi gir1.2-glib-2.0 ostree >/dev/null
export FLATPAK_USER_DIR=/tmp/user
flatpak remote-add --user flatpark https://dl.flatpark.org/flatpark.flatpakrepo
flatpak install --user -y --noninteractive --no-deploy --no-deps flatpark app.audionaut.Audionaut >/dev/null
commit=$(flatpak remote-info --user --show-commit flatpark app.audionaut.Audionaut)
meta=$FLATPAK_USER_DIR/repo/objects/${commit:0:2}/${commit:2}.commitmeta
ls -l "$meta"
ostree --repo=$FLATPAK_USER_DIR/repo show --list-detached-metadata-keys "$commit"
python3 /out/gen.py /out "$meta"
chown "$HOST_IDS" /out/*.gvariant
'
