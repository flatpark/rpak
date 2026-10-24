#!/bin/bash
# Runs inside debian:trixie, see run.sh. Exits non-zero if a check fails.
set -euo pipefail

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "E2E FAILED: $*"; exit 1; }
pass() { echo "ok: $*"; }

# Three small extra-data apps from FlatPark sharing one runtime.
APPS=(app.audionaut.Audionaut com.ferrispad.FerrisPad ai.storyteller.soundcraft)

step "install the distribution's flatpak"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends flatpak ca-certificates >/dev/null
dpkg-query -W -f='debian package: ${Package} ${Version}\n' flatpak
flatpak --version
rpak --version

# A throwaway user installation; rpak and flatpak both derive the
# extra-data directory from it.
export FLATPAK_USER_DIR=/tmp/flatpak-user
mkdir -p "$FLATPAK_USER_DIR"
extra="$FLATPAK_USER_DIR/extra-data"
arch=$(flatpak --default-arch)

step "remotes"
flatpak remote-add --user flathub https://dl.flathub.org/repo/flathub.flatpakrepo
flatpak remote-add --user flatpark https://dl.flatpark.org/flatpark.flatpakrepo
flatpak remotes --user

step "extra-data of the apps under test"
for app in "${APPS[@]}"; do
	echo "--- $app"
	flatpak remote-info --user --show-metadata flatpark "$app" | sed -n '/^\[Extra Data\]/,/^$/p;/^runtime=/p'
done

step "runtime (plain flatpak, so that the logs below are about the apps)"
# --no-related: the GL/codecs extensions are ~2 GB and not needed here.
flatpak install --user -y --noninteractive --no-related flathub "org.freedesktop.Platform/$arch/26.08" >/dev/null
flatpak list --user --runtime --columns=ref

step "rpak install --user -y -v flatpark ${APPS[*]}"
rpak install --user -y -v flatpark "${APPS[@]}" > /tmp/install.log 2>&1 || {
	cat /tmp/install.log
	fail "rpak install failed"
}
# rpak's own lines plus flatpak's extra-data lines; the full log is in /out.
cp /tmp/install.log /out/install.log
grep -E '^rpak|^  app/|extra-data|extra data' /tmp/install.log

step "checks: install"
for app in "${APPS[@]}"; do
	flatpak info --user "$app" >/dev/null || fail "$app is not installed"
done
pass "all ${#APPS[@]} apps installed"

loaded=$(grep -c 'Loading extra-data from local file' /tmp/install.log || true)
[ "$loaded" -eq "${#APPS[@]}" ] || fail "flatpak loaded $loaded local extra-data files, want ${#APPS[@]}"
pass "flatpak loaded all ${#APPS[@]} extra-data files from the local directory"

# Downloads overlap if one starts before another one is done.
overlap=$(awk '
	/^rpak: [0-9:.]+ start / { active++; if (active > max) max = active }
	/^rpak: [0-9:.]+ done /  { active-- }
	END { print max + 0 }' /tmp/install.log)
[ "$overlap" -ge 2 ] || fail "downloads did not overlap (max $overlap at once)"
pass "up to $overlap downloads ran at the same time"

leftover=$(find "$extra" -mindepth 1 2>/dev/null | head -5)
[ -z "$leftover" ] || fail "left in $extra: $leftover"
pass "nothing left in $extra after the successful install"

step "update: build a local extra-data app with two versions"
# Not every FlatPark app keeps old commits around to downgrade to, so the
# update test publishes its own app. The extra-data are real release files
# (from FlatPark's metadata); apply_extra only checksums them.
make_app() { # dir, then [Extra Data] lines
	local dir=$1
	shift
	# export/ is what build-export takes as the sign of build-finish.
	mkdir -p "$dir/files/bin" "$dir/export"
	printf '#!/bin/sh\nsha256sum * > SHA256SUMS\n' > "$dir/files/bin/apply_extra"
	chmod +x "$dir/files/bin/apply_extra"
	{
		printf '[Application]\nname=org.rpak.E2E\nruntime=org.freedesktop.Platform/%s/26.08\ncommand=apply_extra\n\n[Extra Data]\n' "$arch"
		printf '%s\n' "$@"
	} > "$dir/metadata"
}
make_app /tmp/v1 \
	name=authme.deb \
	checksum=8ae36b8ced36fbf470ecef9acd0e458fdee0c1ea13c3ab00708092743eb31018 \
	size=4659086 \
	uri=https://github.com/Levminer/authme/releases/download/7.1.1/authme-7.1.1-linux-x64.deb
make_app /tmp/v2 \
	name=mqtt-viewer.deb \
	checksum=6d02089f0e96cc9852e3140c0995e3c190cf32a304cd712e897947d64c1a4bef \
	size=10317978 \
	uri=https://github.com/mqtt-viewer/mqtt-viewer/releases/download/v1.2.0/MQTT_Viewer_v1.2.0_linux_amd64.deb \
	name1=aptakube.deb \
	checksum1=a18b4f9a8986745cbfd2be5eebff8a1046ce976aba69825dedfed8c0eb81056f \
	size1=12653958 \
	uri1=https://github.com/aptakube/aptakube/releases/download/1.21.6/aptakube_1.21.6_amd64.deb

flatpak build-export /tmp/repo /tmp/v1 stable >/dev/null
flatpak remote-add --user --no-gpg-verify e2e /tmp/repo
flatpak install --user -y --noninteractive e2e org.rpak.E2E >/dev/null
old=$(flatpak info --user --show-commit org.rpak.E2E)
echo "installed v1 (plain flatpak): $old"
flatpak build-export /tmp/repo /tmp/v2 stable >/dev/null
flatpak remote-ls --user --updates --columns=ref,origin

step "rpak update --user -y -v"
rpak update --user -y -v > /tmp/update.log 2>&1 || {
	cat /tmp/update.log
	fail "rpak update failed"
}
cp /tmp/update.log /out/update.log
grep -E '^rpak|^  app/|extra-data|extra data' /tmp/update.log

step "checks: update"
new=$(flatpak info --user --show-commit org.rpak.E2E)
[ "$new" != "$old" ] || fail "org.rpak.E2E was not updated"
pass "org.rpak.E2E updated: $old -> $new"
loaded=$(grep -c 'Loading extra-data from local file' /tmp/update.log || true)
[ "$loaded" -eq 2 ] || fail "flatpak loaded $loaded local extra-data files during the update, want 2"
pass "flatpak loaded both extra-data files of the update from the local directory"
cat "$FLATPAK_USER_DIR/app/org.rpak.E2E/current/active/files/extra/SHA256SUMS"
leftover=$(find "$extra" -mindepth 1 2>/dev/null | head -5)
[ -z "$leftover" ] || fail "left in $extra: $leftover"
pass "nothing left in $extra after the successful update"

step "E2E PASSED"
