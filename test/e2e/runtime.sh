#!/bin/bash
# Runs inside the e2e image with the runtime volume mounted read-write as the
# system installation (/var/lib/flatpak), see run.sh. Installs the runtime
# the first time and checks it for updates after that; a failed update only
# warns, the test then runs with the commit that is there.
set -euo pipefail
. /e2e/env.sh

arch=$(flatpak --default-arch)
want=$RUNTIME/$arch/$RUNTIME_BRANCH

flatpak remote-add --system --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo
# Only the runtime: drop what an earlier version of the test installed.
for ref in $(flatpak list --system --columns=ref); do
	[ "$ref" = "$want" ] || flatpak uninstall --system -y --noninteractive "$ref"
done
if flatpak info --system "$want" >/dev/null 2>&1; then
	echo "runtime: checking $want for updates"
	flatpak update --system -y --noninteractive --no-related "$want" ||
		echo "WARNING: updating $want failed, testing with the installed commit"
else
	echo "runtime: installing $want into the volume"
	# --no-related: the GL/codecs extensions are ~2 GB and not needed here.
	flatpak install --system -y --noninteractive --no-related flathub "$want" >/dev/null
fi
echo "runtime: $want $(flatpak info --system --show-commit "$want")"
