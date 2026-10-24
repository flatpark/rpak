#!/bin/bash
# Runs inside debian:trixie, see run.sh. Exits non-zero if a check fails.
set -euo pipefail

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "E2E FAILED: $*"; exit 1; }
pass() { echo "ok: $*"; }

# Small extra-data apps from FlatPark sharing one runtime: one installed
# with plain flatpak to test rpak clean on, two installed with rpak.
PLAIN=app.audionaut.Audionaut
APPS=(com.ferrispad.FerrisPad ai.storyteller.soundcraft)

step "install the distribution's flatpak"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# ostree and python3 only for the checks: ostree show, and holding locks.
apt-get install -y -qq --no-install-recommends flatpak ca-certificates ostree python3 >/dev/null
dpkg-query -W -f='debian package: ${Package} ${Version}\n' flatpak ostree
flatpak --version
rpak --version

# A throwaway user installation; rpak and flatpak both derive the
# extra-data directory from it.
export FLATPAK_USER_DIR=/tmp/flatpak-user
mkdir -p "$FLATPAK_USER_DIR"
extra="$FLATPAK_USER_DIR/extra-data"
repo="$FLATPAK_USER_DIR/repo"
arch=$(flatpak --default-arch)

commitmeta() { local c; c=$(flatpak info --user --show-commit "$1"); echo "$repo/objects/${c:0:2}/${c:2}.commitmeta"; }
keys() { ostree --repo="$repo" show --list-detached-metadata-keys "$1"; }
# Lists every file below the installation with size, mtime and mode.
state() { find "$FLATPAK_USER_DIR" -printf '%p %y %s %T@ %m\n' | sort; }
# Holds a shared OFD lock on $1, as a flatpak pull does, until killed.
hold_lock() {
	rm -f /tmp/lock-ready
	python3 - "$1" <<'PY' &
import fcntl, os, struct, sys, time
fd = os.open(sys.argv[1], os.O_RDWR | os.O_CREAT, 0o600)
# struct flock: l_type, l_whence, l_start, l_len, l_pid
fcntl.fcntl(fd, fcntl.F_OFD_SETLK, struct.pack("hhqqi4x", fcntl.F_RDLCK, 0, 0, 0, 0))
open("/tmp/lock-ready", "w").close()
time.sleep(120)
PY
	lock_pid=$!
	while [ ! -e /tmp/lock-ready ]; do sleep 0.1; done
}

step "remotes"
flatpak remote-add --user flathub https://dl.flathub.org/repo/flathub.flatpakrepo
flatpak remote-add --user flatpark https://dl.flatpark.org/flatpark.flatpakrepo
flatpak remotes --user

step "extra-data of the apps under test"
for app in "$PLAIN" "${APPS[@]}"; do
	echo "--- $app"
	flatpak remote-info --user --show-metadata flatpark "$app" | sed -n '/^\[Extra Data\]/,/^$/p;/^runtime=/p'
done

step "runtime (plain flatpak, so that the logs below are about the apps)"
# --no-related: the GL/codecs extensions are ~2 GB and not needed here.
flatpak install --user -y --noninteractive --no-related flathub "org.freedesktop.Platform/$arch/26.08" >/dev/null
flatpak list --user --runtime --columns=ref

step "clean: $PLAIN installed with plain flatpak"
flatpak install --user -y --noninteractive flatpark "$PLAIN" >/dev/null
commit=$(flatpak info --user --show-commit "$PLAIN")
meta=$(commitmeta "$PLAIN")
before=$(stat -c %s "$meta")
keys "$commit" | tee /tmp/keys.before
grep -qx xa.extra-data /tmp/keys.before || fail "plain flatpak kept no xa.extra-data"
grep -qx ostree.gpgsigs /tmp/keys.before || fail "the commit of $PLAIN is not signed"
for k in $(grep -vx xa.extra-data /tmp/keys.before); do
	ostree --repo="$repo" show --print-detached-metadata-key="$k" "$commit" > "/tmp/key.$k.before"
done
ostree --repo="$repo" show --gpg-verify-remote=flatpark "$commit" > /tmp/gpg.before 2>&1 || {
	cat /tmp/gpg.before
	fail "the signature does not verify before rpak clean"
}
grep -E 'Good signature|Signature made' /tmp/gpg.before
echo "commitmeta: $before bytes"

step "rpak clean --user --dry-run"
state > /tmp/state.before
rpak clean --user --dry-run | tee /tmp/clean.log
state > /tmp/state.after
diff /tmp/state.before /tmp/state.after || fail "rpak clean --dry-run changed files"
pass "the dry run changed nothing"

step "rpak clean --user"
rpak clean --user | tee /tmp/clean.log
grep -Eq "MB  (deploy/|flatpark:)app/$PLAIN/$arch/stable$" /tmp/clean.log || fail "no line for $PLAIN"
after=$(stat -c %s "$meta")
echo "commitmeta: $before -> $after bytes"
[ "$after" -lt $((before / 1000)) ] || fail "commitmeta did not shrink"
pass "commitmeta shrank from $before to $after bytes"
keys "$commit" > /tmp/keys.after
grep -vx xa.extra-data /tmp/keys.before | diff - /tmp/keys.after || fail "keys after: $(cat /tmp/keys.after)"
pass "xa.extra-data is gone, the other keys are there: $(tr '\n' ' ' < /tmp/keys.after)"
for k in $(cat /tmp/keys.after); do
	ostree --repo="$repo" show --print-detached-metadata-key="$k" "$commit" | cmp - "/tmp/key.$k.before" ||
		fail "$k changed"
	pass "$k is unchanged ($(wc -c < "/tmp/key.$k.before") bytes of ostree show output)"
done
ostree --repo="$repo" show --gpg-verify-remote=flatpark "$commit" > /tmp/gpg.after 2>&1 || {
	cat /tmp/gpg.after
	fail "the signature does not verify after rpak clean"
}
diff /tmp/gpg.before /tmp/gpg.after || fail "ostree show --gpg-verify-remote output changed"
pass "the signature still verifies against the flatpark remote's key"
flatpak repair --user > /tmp/repair.log 2>&1 || { cat /tmp/repair.log; fail "flatpak repair failed"; }
tail -3 /tmp/repair.log
pass "flatpak repair --user"
[ "$(flatpak info --user --show-commit "$PLAIN")" = "$commit" ] || fail "flatpak info"
[ "$(stat -c %s "$meta")" = "$after" ] || fail "repair rewrote the commitmeta"
pass "flatpak info shows the same commit, repair left the commitmeta alone"
flatpak run --user --command=true "$PLAIN" || fail "flatpak run failed"
flatpak run --user --command=sh "$PLAIN" -c 'ls /app/extra | head -3'
pass "$PLAIN runs"

step "clean: leftovers"
hex() { printf '%064x' "$1"; }
branch="$FLATPAK_USER_DIR/app/$PLAIN/$arch/stable"
tmpdir="$branch/.$(hex 1)-AbC123"
mkdir -p "$tmpdir/files" && head -c 100000 /dev/urandom > "$tmpdir/files/leftover"
partial="$repo/state/$(hex 2).commitpartial"
mkdir -p "$repo/state" && touch "$partial"
mkdir -p "$repo/refs/remotes/deleted-remote/app/org.rpak.Gone/$arch"
mkdir -p "$FLATPAK_USER_DIR/app/org.rpak.Gone/$arch/stable"
marked="$extra/$(hex 3)"
mkdir -p "$marked" && touch "$marked/.rpak-prefetch" && head -c 50000 /dev/urandom > "$marked/app.deb.part"
mkdir -p "$extra/$(hex 4)" && echo mine > "$extra/$(hex 4)/app.deb"
mkdir -p "$extra/$(hex 5)" && touch "$extra/$(hex 5)/.flatpak-prefetch" && echo fork > "$extra/$(hex 5)/app.deb.part"
notmp="$branch/.not-a-checkout"
mkdir -p "$notmp"
rpak clean --user | tee /tmp/clean.log
for gone in "$tmpdir" "$partial" "$repo/refs/remotes/deleted-remote" "$FLATPAK_USER_DIR/app/org.rpak.Gone" "$marked"; do
	[ ! -e "$gone" ] || fail "$gone is still there"
done
pass "removed: interrupted deploy, orphan commitpartial, empty refs, empty app dir, rpak prefetch dir"
for kept in "$extra/$(hex 4)/app.deb" "$extra/$(hex 5)/.flatpak-prefetch" "$extra/$(hex 5)/app.deb.part" "$notmp" \
	"$branch/active/files" "$repo/refs/remotes/flatpark/app/$PLAIN/$arch/stable"; do
	[ -e "$kept" ] || fail "$kept was removed"
done
pass "kept: unmarked extra-data dir, flatpak's .flatpak-prefetch dir, other dot dirs, the deployment, live refs"
rm -rf "$extra" "$notmp"
flatpak run --user --command=true "$PLAIN" || fail "flatpak run failed after cleaning leftovers"

step "clean: locks"
for lockfile in "$repo/.lock" "$FLATPAK_USER_DIR/repo-lock"; do
	mkdir -p "$repo/refs/remotes/deleted-remote/x" # a leftover for rpak clean to want
	hold_lock "$lockfile"
	state > /tmp/state.before
	code=0
	rpak clean --user > /tmp/clean.log 2>&1 || code=$?
	cat /tmp/clean.log
	state > /tmp/state.after
	[ "$code" -ne 0 ] || fail "rpak clean succeeded while $lockfile was locked"
	grep -q "busy with another transaction" /tmp/clean.log || fail "no busy message"
	diff /tmp/state.before /tmp/state.after || fail "rpak clean changed files while $lockfile was locked"
	# --no-pull: flatpak has nothing to do, so the exit code is 0 and
	# flatpak itself does not touch the repository.
	code=0
	rpak update --user -y --no-pull > /tmp/update-locked.log 2>&1 || code=$?
	grep '^rpak' /tmp/update-locked.log
	[ "$code" -eq 0 ] || fail "rpak update exited $code while $lockfile was locked"
	grep -q "busy, not freeing cached extra-data" /tmp/update-locked.log || fail "no busy warning from rpak update"
	kill "$lock_pid"; wait "$lock_pid" 2>/dev/null || true
	pass "with $lockfile locked: rpak clean failed as busy and changed nothing, rpak update kept flatpak's exit code 0"
done
rpak clean --user | tail -1
rm -rf "$extra"

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

grep -q '^rpak: freed [0-9.]* MB of cached extra-data$' /tmp/install.log || fail "no reclaim summary"
grep '^rpak: freed' /tmp/install.log
for app in "${APPS[@]}"; do
	c=$(flatpak info --user --show-commit "$app")
	keys "$c" > /tmp/keys
	! grep -qx xa.extra-data /tmp/keys || fail "$app still has xa.extra-data"
	grep -qx ostree.gpgsigs /tmp/keys || fail "$app lost its signatures"
	ostree --repo="$repo" show --gpg-verify-remote=flatpark "$c" >/dev/null 2>&1 || fail "$app: signature"
	echo "$app: commitmeta $(stat -c %s "$(commitmeta "$app")") bytes, keys: $(tr '\n' ' ' < /tmp/keys)"
	flatpak run --user --command=true "$app" || fail "$app does not run"
done
pass "rpak install stripped xa.extra-data of both apps; signatures verify, the apps run"

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
grep -q '^rpak: freed [0-9.]* MB of cached extra-data$' /tmp/update.log || fail "no reclaim summary"
grep '^rpak: freed' /tmp/update.log
# The local repository is not signed: only xa.extra-data was in there.
keys "$new" > /tmp/keys
[ ! -s /tmp/keys ] || fail "keys left: $(cat /tmp/keys)"
echo "org.rpak.E2E: commitmeta $(stat -c %s "$(commitmeta org.rpak.E2E)") bytes"
flatpak run --user --command=true org.rpak.E2E || fail "org.rpak.E2E does not run"
pass "rpak update stripped xa.extra-data of the new commit"

step "final checks"
flatpak repair --user > /tmp/repair.log 2>&1 || { cat /tmp/repair.log; fail "flatpak repair failed"; }
pass "flatpak repair --user"
rpak clean --user --dry-run | tail -1
leftover=$(find "$repo/objects" -name '*.commitmeta' -size +100k)
[ -z "$leftover" ] || fail "big commitmeta left: $leftover"
pass "no commitmeta over 100 KiB left in the repository"

step "E2E PASSED"
