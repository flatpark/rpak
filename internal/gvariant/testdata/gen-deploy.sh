#!/bin/bash
# Regenerates the *.deploy fixtures with GLib (python3-gi) in a
# debian:trixie container. The "real" case is the deploy file the
# distribution's flatpak writes when it deploys a tiny app built here (no
# runtime needed: --no-deps, and the app is never run). Needs podman.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
podman run --rm --privileged -v "$here:/out" -e HOST_IDS="$(id -u):$(id -g)" -e http_proxy -e https_proxy -e HTTP_PROXY -e HTTPS_PROXY \
	docker.io/library/debian:trixie bash -euo pipefail -c '
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends flatpak python3-gi gir1.2-glib-2.0 >/dev/null
export FLATPAK_USER_DIR=/tmp/user
mkdir -p /tmp/app/files/bin /tmp/app/export
printf "#!/bin/sh\n" > /tmp/app/files/bin/run
printf "[Application]\nname=org.rpak.Deploy\nruntime=org.freedesktop.Platform/x86_64/25.08\nsdk=org.freedesktop.Sdk/x86_64/25.08\ncommand=run\n" > /tmp/app/metadata
flatpak build-export --arch=x86_64 /tmp/repo /tmp/app stable >/dev/null
flatpak remote-add --user --no-gpg-verify local /tmp/repo
flatpak install --user -y --noninteractive --no-deps --arch=x86_64 local org.rpak.Deploy >/dev/null
python3 /out/gen-deploy.py /out "$FLATPAK_USER_DIR/app/org.rpak.Deploy/x86_64/stable/active/deploy"
chown "$HOST_IDS" /out/*.deploy
'
