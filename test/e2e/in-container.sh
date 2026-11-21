#!/bin/bash
# Runs inside the e2e image, see run.sh. Exits non-zero if a check fails.
# /var/lib/flatpak is the runtime volume, read-only: a system installation
# with only the runtime, which the --user installs below depend on.
set -euo pipefail
. /e2e/env.sh

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "E2E FAILED: $*"; exit 1; }
pass() { echo "ok: $*"; }

# Small extra-data apps from FlatPark sharing one runtime: one installed
# with plain flatpak to test rpak clean on, two installed with rpak.
PLAIN=app.audionaut.Audionaut
APPS=(com.ferrispad.FerrisPad ai.storyteller.soundcraft)

step "the distribution's flatpak"
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
state() { find "${1:-$FLATPAK_USER_DIR}" -printf '%p %y %s %T@ %m\n' | sort; }
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
# Whether someone holds a lock on $1 that a write lock would conflict with,
# as flatpak tests files/.ref of a deployment.
ref_locked() {
	python3 - "$1" <<'PY'
import fcntl, os, struct, sys
fd = os.open(sys.argv[1], os.O_RDONLY)
r = fcntl.fcntl(fd, fcntl.F_GETLK, struct.pack("hhqqi4x", fcntl.F_WRLCK, 0, 0, 0, 0))
sys.exit(0 if struct.unpack("hhqqi4x", r)[0] != fcntl.F_UNLCK else 1)
PY
}

step "remotes"
# No flathub: the runtime has to come from the system installation.
flatpak remote-add --user flatpark https://dl.flatpark.org/flatpark.flatpakrepo
flatpak remotes --user

step "rpak doctor $PLAIN, before it is installed"
# doctor reads the remote's summary itself, into memory: flatpark's commit
# of the app, without flatpak caching the summary in the repository.
state > /tmp/state.before
code=0
rpak doctor "$PLAIN" > /tmp/doctor.log 2>&1 || code=$?
sed -n "/^== App $PLAIN/,/^Data and state/p" /tmp/doctor.log
[ "$code" -eq 0 ] || fail "rpak doctor $PLAIN exited $code"
grep -qx "  not installed in any installation" /tmp/doctor.log || fail "rpak doctor $PLAIN: not reported as not installed"
grep -Eq "^  extra-data .*  (HTTP [0-9]+.*|.* after [0-9.]+s) \(pinned [^)]+\)(  cached)?  \(remote flatpark \(User installation\), branch stable\)$" /tmp/doctor.log ||
	fail "rpak doctor $PLAIN: no extra-data check from the flatpark remote"
state > /tmp/state.after
diff /tmp/state.before /tmp/state.after || fail "rpak doctor $PLAIN changed files"
code=0
rpak doctor org.rpak.NoSuchApp > /tmp/doctor.log 2>&1 || code=$?
[ "$code" -eq 0 ] || fail "rpak doctor org.rpak.NoSuchApp exited $code"
sed -n "/^== App org.rpak.NoSuchApp/,\$p" /tmp/doctor.log | grep -qx "  not found in any remote" ||
	fail "rpak doctor org.rpak.NoSuchApp: $(sed -n "/^== App org.rpak.NoSuchApp/,\$p" /tmp/doctor.log)"
pass "rpak doctor finds the extra-data of an app that is not installed in the remote, changing nothing"

step "extra-data of the apps under test"
for app in "$PLAIN" "${APPS[@]}"; do
	echo "--- $app"
	flatpak remote-info --user --show-metadata flatpark "$app" | sed -n '/^\[Extra Data\]/,/^$/p;/^runtime=/p'
done

step "runtime: system installation (run.sh keeps it in a volume)"
flatpak list --system --columns=ref,active
flatpak info --system "$RUNTIME/$arch/$RUNTIME_BRANCH" >/dev/null || fail "no $RUNTIME in the system installation"
state /var/lib/flatpak > /tmp/system.before

step "clean: $PLAIN installed with plain flatpak"
flatpak install --user -y --noninteractive flatpark "$PLAIN" >/dev/null
flatpak info --user "$PLAIN" >/dev/null || fail "$PLAIN is not installed"
[ -z "$(flatpak list --user --runtime --columns=ref)" ] || fail "runtimes in the user installation: $(flatpak list --user --runtime --columns=ref)"
pass "$PLAIN installed --user with the runtime of the system installation"
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
grep -Eq "^ +[0-9.]+ (kB|MB|GB)  $PLAIN  (flatpark|deployed)$" /tmp/clean.log || fail "no line for $PLAIN"
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
# user data of an app that is gone (listed, kept) and of one installed
mkdir -p "$HOME/.var/app/org.rpak.Gone" && head -c 300000 /dev/urandom > "$HOME/.var/app/org.rpak.Gone/data"
mkdir -p "$FLATPAK_USER_DIR/overrides" && printf '[Context]\nshared=network;\n' > "$FLATPAK_USER_DIR/overrides/org.rpak.Gone"
mkdir -p "$HOME/.var/app/$PLAIN" && echo mine > "$HOME/.var/app/$PLAIN/rpak-test"
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
grep -Eq '^ +3[0-9.]+ kB  org\.rpak\.Gone  ~/\.var/app, overrides$' /tmp/clean.log || fail "data of org.rpak.Gone not listed"
! grep -q "  $PLAIN  ~/.var/app" /tmp/clean.log || fail "data of the installed $PLAIN listed"
for kept in "$HOME/.var/app/org.rpak.Gone/data" "$FLATPAK_USER_DIR/overrides/org.rpak.Gone" "$HOME/.var/app/$PLAIN/rpak-test"; do
	[ -e "$kept" ] || fail "$kept was removed"
done
pass "the data of the uninstalled app is listed and kept, that of the installed one is not listed"
rm -rf "$HOME/.var/app/org.rpak.Gone" "$FLATPAK_USER_DIR/overrides/org.rpak.Gone"
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
# rpak's own lines (with the plan table) plus flatpak's extra-data lines;
# the full log is in /out.
cp /tmp/install.log /out/install.log
grep -E '^rpak|^  ID  |^  [^ ].*  [0-9.]+ (bytes|[kMGT]B)( |$)|extra-data|extra data' /tmp/install.log

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

grep -Eq '^rpak: freed [0-9.]+ (kB|MB|GB) of cached extra-data$' /tmp/install.log || fail "no reclaim summary"
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
		printf '[Application]\nname=org.rpak.E2E\nruntime=%s/%s/%s\ncommand=apply_extra\n\n[Extra Data]\n' "$RUNTIME" "$arch" "$RUNTIME_BRANCH"
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
# v1 keeps running through the update: its deployment goes to .removed and
# stays there while in use (see "clean: an old deployment in use").
flatpak run --user --command=sleep org.rpak.E2E 600 >/tmp/running.log 2>&1 &
running_pid=$!
for _ in $(seq 100); do
	ref_locked "$FLATPAK_USER_DIR/app/org.rpak.E2E/current/active/files/.ref" && break
	sleep 0.1
done
ref_locked "$FLATPAK_USER_DIR/app/org.rpak.E2E/current/active/files/.ref" || { cat /tmp/running.log; fail "org.rpak.E2E v1 did not start"; }

step "rpak update --user -y -v"
rpak update --user -y -v > /tmp/update.log 2>&1 || {
	cat /tmp/update.log
	fail "rpak update failed"
}
cp /tmp/update.log /out/update.log
grep -E '^rpak|^  ID  |^  [^ ].*  [0-9.]+ (bytes|[kMGT]B)( |$)|extra-data|extra data' /tmp/update.log

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
grep -Eq '^rpak: freed [0-9.]+ (kB|MB|GB) of cached extra-data$' /tmp/update.log || fail "no reclaim summary"
grep '^rpak: freed' /tmp/update.log
# The local repository is not signed: only xa.extra-data was in there.
keys "$new" > /tmp/keys
[ ! -s /tmp/keys ] || fail "keys left: $(cat /tmp/keys)"
echo "org.rpak.E2E: commitmeta $(stat -c %s "$(commitmeta org.rpak.E2E)") bytes"
flatpak run --user --command=true org.rpak.E2E || fail "org.rpak.E2E does not run"
pass "rpak update stripped xa.extra-data of the new commit"

step "clean: an old deployment in use"
old_deploy=$FLATPAK_USER_DIR/.removed/org.rpak.E2E-$old
ls "$FLATPAK_USER_DIR/.removed"
[ -d "$old_deploy" ] || fail "v1 is not in .removed while it runs"
# flatpak ps may list more than one instance of the app; the pid is that of
# the one holding the lock, which is one of them.
flatpak ps --columns=application,child-pid | awk '$1 == "org.rpak.E2E" { print $2 }' > /tmp/child-pids
echo "flatpak ps: org.rpak.E2E child-pids $(tr '\n' ' ' < /tmp/child-pids)"
in_use="^ +[0-9.]+ (kB|MB)  \.removed/org\.rpak\.E2E-$old  used by org\.rpak\.E2E \(pid ([0-9]+)\)\$"
for argv in "--dry-run" ""; do
	rpak clean --user $argv | tee /tmp/clean.log
	grep -qx "  Old deployments still in use" /tmp/clean.log || fail "rpak clean $argv: no section for deployments in use"
	child=$(grep -E "$in_use" /tmp/clean.log | sed -E 's/.*\(pid ([0-9]+)\)$/\1/')
	[ -n "$child" ] || fail "rpak clean $argv: no line for v1 used by org.rpak.E2E with a pid"
	grep -qx "$child" /tmp/child-pids || fail "rpak clean $argv: pid $child is not a CHILD-PID of org.rpak.E2E in flatpak ps"
	grep -qx "  Quit them and run rpak clean again" /tmp/clean.log || fail "rpak clean $argv: no hint"
	grep -Eq "still in use\$" /tmp/clean.log || fail "rpak clean $argv: the total does not mention it"
	[ -d "$old_deploy" ] || fail "rpak clean $argv removed the deployment in use"
done
pass "rpak clean lists v1 as used by org.rpak.E2E (pid $child) and keeps it"
flatpak kill org.rpak.E2E 2>/dev/null || kill "$running_pid"
wait "$running_pid" 2>/dev/null || true
for _ in $(seq 100); do
	ref_locked "$old_deploy/files/.ref" || break
	sleep 0.1
done
rpak clean --user | tee /tmp/clean.log
[ ! -e "$old_deploy" ] || fail "v1 kept after it quit"
! grep -q "still in use" /tmp/clean.log || fail "still listed as in use after it quit"
pass "once v1 quit, rpak clean removed it"

step "rpak doctor"
# A redaction point: a global override naming a path in $HOME, which
# doctor prints as ~/... (the user is root here, whose name is left as is).
mkdir -p "$FLATPAK_USER_DIR/overrides" "$HOME/.var/app/${APPS[0]}"
printf '[Context]\nfilesystems=%s/rpak-doctor-secret;\n' "$HOME" > "$FLATPAK_USER_DIR/overrides/global"
state > /tmp/state.before
SECONDS=0
for argv in "" "${APPS[0]}"; do
	code=0
	# shellcheck disable=SC2086 # no argument for the first run
	rpak doctor $argv > /tmp/doctor.log 2>&1 || code=$?
	cat /tmp/doctor.log
	[ "$code" -eq 0 ] || fail "rpak doctor $argv exited $code"
	for title in "rpak and flatpak" Environment System Installations "Extra-data directory" Network; do
		grep -qx "== $title" /tmp/doctor.log || fail "rpak doctor $argv: no section $title"
	done
	# Facts only: no marks, no summary of problems.
	! grep -Eq "^(✓|✗|!) |^== Problems$|^All installations$|→" /tmp/doctor.log ||
		fail "rpak doctor $argv: a judgement: $(grep -E "^(✓|✗|!) |^== Problems$|^All installations$|→" /tmp/doctor.log | head -3)"
	grep -qF "~/rpak-doctor-secret" /tmp/doctor.log || fail "rpak doctor $argv: the global override is not shown redacted"
	! grep -qF "$HOME/" /tmp/doctor.log || fail "rpak doctor $argv: $HOME/ in the output: $(grep -F "$HOME/" /tmp/doctor.log | head -3)"
done
pass "rpak doctor: all sections, facts only, exit 0, $HOME shown as ~ (${SECONDS}s for both runs)"
# The cards of the host's /sys, or none in the container.
grep -Eq "^  GPU (card[0-9]+: .*, (driver [^ ]+|no driver)( \(boot\))?|: none found in /sys/class/drm)$" /tmp/doctor.log ||
	fail "rpak doctor: no GPU line"
pass "rpak doctor lists the graphics cards: $(grep -E '^  GPU' /tmp/doctor.log | sed 's/^  //' | paste -sd ';')"
grep -qx "== App ${APPS[0]}" /tmp/doctor.log || fail "no section for ${APPS[0]}"
# Both sizes, the same: the file is the pinned one.
grep -E '^  extra-data .*  HTTP 200, (.+) \(pinned \1\)(  cached)?  \(installed and remote commit\)$' /tmp/doctor.log ||
	fail "no extra-data check of the remote's commit of ${APPS[0]}: $(grep -E '^  extra-data' /tmp/doctor.log)"
grep -qF "~/.var/app/${APPS[0]}: " /tmp/doctor.log || fail "no data directory line for ${APPS[0]}"
pass "rpak doctor ${APPS[0]}: the app's section with the extra-data check of the remote's commit"
# Every remote with its options, and how its summary answers.
grep -q "^  remote e2e  file:///tmp/repo  (priority 1, no-gpg-verify)$" /tmp/doctor.log || fail "rpak doctor: no line for the e2e remote"
grep -q "^  remote flathub  https://dl.flathub.org/repo/  (priority 1)$" /tmp/doctor.log || fail "rpak doctor: no line for the flathub remote"
grep -Eq "^  https://dl.flatpark.org/summary  (HTTP [0-9]+ in|.* after) [0-9.]+s  flatpark \(User installation\)$" /tmp/doctor.log ||
	fail "rpak doctor: no network line for flatpark's summary"
grep -Eq "^  repo/.lock: not locked$" /tmp/doctor.log || fail "rpak doctor: no lock line"
pass "rpak doctor: remotes, locks and the network as facts"
state > /tmp/state.after
diff /tmp/state.before /tmp/state.after || fail "rpak doctor changed files"
pass "rpak doctor changed nothing"
rm -f "$FLATPAK_USER_DIR/overrides/global"

step "rpak complete: doctor"
rpak complete "rpak doctor " 12 "" | tee /tmp/complete.log
grep -qx "${APPS[1]} " /tmp/complete.log || fail "rpak doctor <TAB> does not offer ${APPS[1]}"
line="rpak doctor com.ferris"
[ "$(rpak complete "$line" "${#line}" com.ferris)" = "com.ferrispad.FerrisPad " ] || fail "rpak doctor com.ferris<TAB>"
pass "rpak doctor completes the installed apps"

step "force-runtime: an upstream repository served over http"
# Two apps made here, without an SDK, built for a branch of the runtime
# below $RUNTIME_BRANCH that is nowhere in the container: forcing them is
# the only way they can run. The repository and the extra-data come from
# python's http server, so rpak fetches the objects over http.
old=$(awk -F. -v b="$RUNTIME_BRANCH" 'BEGIN { split(b, v, "."); printf "%d.%s", v[1] - 1, v[2] }')
! flatpak info "$RUNTIME/$arch/$old" >/dev/null 2>&1 || fail "$RUNTIME//$old is installed in the container"
www=/tmp/fr-www
up=$www/repo
port=8765
url=http://127.0.0.1:$port
export no_proxy=127.0.0.1,localhost NO_PROXY=127.0.0.1,localhost
mkdir -p "$www"
python3 -m http.server --bind 127.0.0.1 --directory "$www" "$port" >/tmp/http.log 2>&1 &
http_pid=$!
for _ in $(seq 50); do
	python3 -c 'import sys, urllib.request; urllib.request.urlopen(sys.argv[1])' "$url/" 2>/dev/null && break
	sleep 0.1
done
fr_app() { # id version command, then extra metadata lines
	local id=$1 version=$2 command=$3 dir=/tmp/fr-$1-$2
	shift 3
	rm -rf "$dir"
	mkdir -p "$dir/files/bin" "$dir/export"
	{
		printf '[Application]\nname=%s\nruntime=%s/%s/%s\nsdk=org.freedesktop.Sdk/%s/%s\ncommand=%s\n' \
			"$id" "$RUNTIME" "$arch" "$old" "$arch" "$old" "$command"
		printf '%s\n' "$@"
	} > "$dir/metadata"
	echo "$dir"
}
publish_plain() { # version
	local dir
	dir=$(fr_app org.rpak.test.Plain "$1" plain)
	printf '#!/bin/sh\necho "rpak plain %s"\n' "$1" > "$dir/files/bin/plain"
	chmod +x "$dir/files/bin/plain"
	flatpak build-export --arch="$arch" "$up" "$dir" stable >/dev/null
}
publish_extra() { # version
	local dir payload=$www/payload-$1.txt
	echo "payload $1" > "$payload"
	dir=$(fr_app org.rpak.test.Extra "$1" extra '' '[Extra Data]' name=payload.txt \
		"checksum=$(sha256sum "$payload" | cut -d' ' -f1)" "size=$(stat -c %s "$payload")" "uri=$url/payload-$1.txt")
	# Run by the runtime's /bin/sh while deploying: proves which runtime.
	printf '#!/bin/sh\nset -e\ntr a-z A-Z < payload.txt > marker\nrm payload.txt\n. /usr/lib/os-release && echo "$VERSION_ID" > runtime-version\n' > "$dir/files/bin/apply_extra"
	printf '#!/bin/sh\ncat /app/extra/marker /app/extra/runtime-version\n' > "$dir/files/bin/extra"
	chmod +x "$dir/files/bin/apply_extra" "$dir/files/bin/extra"
	flatpak build-export --arch="$arch" "$up" "$dir" stable >/dev/null
}
publish_plain 1
publish_extra 1
flatpak remote-add --user --no-gpg-verify upstream "$url/repo"
flatpak remote-info --user upstream org.rpak.test.Extra | grep -E 'Runtime|Commit'
pass "upstream serves org.rpak.test.Plain and org.rpak.test.Extra, built for $RUNTIME//$old"

step "force-runtime: plain flatpak cannot deploy the extra-data app without its runtime"
code=0
flatpak install --user -y --noninteractive --no-deps upstream org.rpak.test.Extra > /tmp/fr-plain.log 2>&1 || code=$?
tail -3 /tmp/fr-plain.log
[ "$code" -ne 0 ] || fail "flatpak installed org.rpak.test.Extra without $RUNTIME//$old"
! flatpak info --user org.rpak.test.Extra >/dev/null 2>&1 || fail "org.rpak.test.Extra is installed"
pass "apply_extra needs the runtime of the metadata: flatpak install --no-deps failed (exit $code)"

step "rpak install -y --force-runtime=$RUNTIME_BRANCH upstream org.rpak.test.Plain org.rpak.test.Extra"
code=0
rpak install -y --force-runtime="$RUNTIME_BRANCH" upstream org.rpak.test.Plain org.rpak.test.Extra > /tmp/fr-install.log 2>&1 || code=$?
cp /tmp/fr-install.log /out/force-install.log
grep -E '^rpak' /tmp/fr-install.log
[ "$code" -eq 0 ] || { cat /tmp/fr-install.log; fail "rpak install --force-runtime exited $code"; }

step "checks: force-runtime install"
forced=$RUNTIME/$arch/$RUNTIME_BRANCH
for app in org.rpak.test.Plain org.rpak.test.Extra; do
	rt=$(flatpak info --user "$app" | sed -n 's/^ *Runtime: //p')
	[ "$rt" = "$forced" ] || fail "$app: flatpak info says runtime $rt"
	grep -qxF "rpak: experimental: $app was built for $RUNTIME/$arch/$old, now runs on $forced; it may break. Undo: rpak patch --reset $app" /tmp/fr-install.log ||
		fail "no experimental warning for $app"
done
pass "flatpak info: both run on $forced; the experimental warning was printed for both"
[ -z "$(flatpak list --user --runtime --columns=ref)" ] || fail "runtimes pulled into the user installation: $(flatpak list --user --runtime --columns=ref)"
! ls "$repo/refs/remotes/"*/runtime 2>/dev/null | grep -q . || fail "runtime refs pulled: $(ls -R "$repo/refs/remotes/"*/runtime)"
pass "no runtime was pulled into the user installation"
[ "$(flatpak run --user org.rpak.test.Plain)" = "rpak plain 1" ] || fail "org.rpak.test.Plain: $(flatpak run --user org.rpak.test.Plain 2>&1)"
pass "org.rpak.test.Plain runs: rpak plain 1"
extradir=$FLATPAK_USER_DIR/app/org.rpak.test.Extra/$arch/stable/active/files/extra
[ "$(cat "$extradir/marker")" = "PAYLOAD 1" ] || fail "marker: $(cat "$extradir/marker" 2>&1)"
[ "$(cat "$extradir/runtime-version")" = "$RUNTIME_BRANCH" ] || fail "apply_extra ran in runtime $(cat "$extradir/runtime-version" 2>&1)"
[ "$(flatpak run --user org.rpak.test.Extra | sed -n 1p)" = "PAYLOAD 1" ] || fail "org.rpak.test.Extra does not run"
pass "apply_extra of org.rpak.test.Extra ran in the $RUNTIME_BRANCH runtime; the app runs"
[ "$(flatpak info --user --show-origin org.rpak.test.Extra)" = rpak-local ] || fail "origin of org.rpak.test.Extra: $(flatpak info --user --show-origin org.rpak.test.Extra)"
[ "$(flatpak info --user --show-origin org.rpak.test.Plain)" = upstream ] || fail "origin of org.rpak.test.Plain"
flatpak remotes --user --columns=name,url,options | grep '^rpak-local' || fail "no remote rpak-local"
pass "org.rpak.test.Extra comes from the remote rpak-local (exists before any reset), org.rpak.test.Plain from upstream"
plainmeta=$FLATPAK_USER_DIR/app/org.rpak.test.Plain/$arch/stable/active/metadata
[ "$(stat -c %h "$plainmeta")" = 1 ] || fail "the deployment's metadata still has $(stat -c %h "$plainmeta") links"
pcommit=$(flatpak info --user --show-commit org.rpak.test.Plain)
ostree --repo="$repo" cat "$pcommit" /metadata | grep -qx "runtime=$RUNTIME/$arch/$old" || fail "the repository's metadata changed"
ostree --repo="$repo" fsck > /tmp/fsck.log 2>&1 || { cat /tmp/fsck.log; fail "ostree fsck"; }
pass "the patched metadata is a file of its own; the repository object still says $old and ostree fsck passes"
grep -qx "runtime=$forced" "$plainmeta" || fail "deployment metadata"
grep -qx "sdk=org.freedesktop.Sdk/$arch/$RUNTIME_BRANCH" "$plainmeta" || fail "sdk not moved along"
pass "the deployment's metadata names runtime and sdk $RUNTIME_BRANCH"

step "force-runtime: rpak update -y after a new upstream commit"
publish_plain 2
publish_extra 2
code=0
rpak update -y > /tmp/fr-update.log 2>&1 || code=$?
cp /tmp/fr-update.log /out/force-update.log
grep -E '^rpak' /tmp/fr-update.log
[ "$code" -eq 0 ] || { cat /tmp/fr-update.log; fail "rpak update exited $code"; }
[ "$(flatpak run --user org.rpak.test.Plain)" = "rpak plain 2" ] || fail "org.rpak.test.Plain not updated: $(flatpak run --user org.rpak.test.Plain 2>&1)"
[ "$(cat "$extradir/marker")" = "PAYLOAD 2" ] || fail "org.rpak.test.Extra not updated: $(cat "$extradir/marker")"
for app in org.rpak.test.Plain org.rpak.test.Extra; do
	rt=$(flatpak info --user "$app" | sed -n 's/^ *Runtime: //p')
	[ "$rt" = "$forced" ] || fail "$app runs on $rt after the update"
done
[ -z "$(flatpak list --user --runtime --columns=ref)" ] || fail "runtimes pulled by the update"
pass "both updated to their new commits and still run on $RUNTIME_BRANCH"

step "rpak patch (the list)"
rpak patch | tee /tmp/fr-list.log
for app in org.rpak.test.Plain org.rpak.test.Extra; do
	grep -E "^$app/$arch/stable +force-runtime +$RUNTIME/$arch/$old +$forced +(patch|reexport) +upstream +$RUNTIME_BRANCH\$" /tmp/fr-list.log >/dev/null || fail "list: no line for $app"
done
grep -q "^org.rpak.test.Plain/.* patch " /tmp/fr-list.log && grep -q "^org.rpak.test.Extra/.* reexport " /tmp/fr-list.log || fail "list: methods"
[ -f "$FLATPAK_USER_DIR/rpak/patches.json" ] || fail "no patches.json"
pass "rpak patch lists both, methods patch and reexport"

step "force-runtime: rpak doctor"
code=0
rpak doctor org.rpak.test.Plain org.rpak.test.Extra > /tmp/fr-doctor.log 2>&1 || code=$?
sed -n '/^== Patches/,/^== /p' /tmp/fr-doctor.log
[ "$code" -eq 0 ] || fail "rpak doctor exited $code"
grep -qx "== Patches" /tmp/fr-doctor.log || fail "no Patches section"
grep -q "org.rpak.test.Plain.*force-runtime (method patch).*deployment runs on $forced" /tmp/fr-doctor.log || fail "doctor: patch state of org.rpak.test.Plain"
grep -q "installed from rpak-local: app/org.rpak.test.Extra/$arch/stable" /tmp/fr-doctor.log || fail "doctor: apps from rpak-local"
grep -E "^  forced runtime: " /tmp/fr-doctor.log || fail "doctor: no forced runtime line in the app sections"
grep -E "^  extra-data payload.txt .*\(upstream remote upstream" /tmp/fr-doctor.log || fail "doctor: the extra-data check of org.rpak.test.Extra is not against upstream"
pass "rpak doctor: the Patches section, the apps marked as forced, extra-data checked upstream"

step "force-runtime: flatpak update outside rpak, flatpak repair, uninstall --unused"
publish_plain 3
# flatpak deploys upstream's metadata again; without --no-deps it would
# pull $RUNTIME//$old, which this container does not have anywhere.
flatpak update --user -y --noninteractive --no-deps org.rpak.test.Plain >/dev/null
rt=$(flatpak info --user org.rpak.test.Plain | sed -n 's/^ *Runtime: //p')
[ "$rt" = "$RUNTIME/$arch/$old" ] || fail "after flatpak update: runtime $rt"
rpak doctor org.rpak.test.Plain > /tmp/fr-doctor.log 2>&1
grep "org.rpak.test.Plain" /tmp/fr-doctor.log | grep -q "not forced now" || { sed -n '/^== Patches/,/^== /p' /tmp/fr-doctor.log; fail "doctor does not see the restored metadata"; }
pass "flatpak update put back upstream's runtime; rpak doctor reports it"
rpak update -y > /tmp/fr-update.log 2>&1 || { cat /tmp/fr-update.log; fail "rpak update"; }
grep '^rpak' /tmp/fr-update.log
[ "$(flatpak run --user org.rpak.test.Plain)" = "rpak plain 3" ] || fail "org.rpak.test.Plain after rpak update"
rt=$(flatpak info --user org.rpak.test.Plain | sed -n 's/^ *Runtime: //p')
[ "$rt" = "$forced" ] || fail "rpak update did not force it again: $rt"
pass "the next rpak update forced it again"
flatpak repair --user > /tmp/repair.log 2>&1 || { cat /tmp/repair.log; fail "flatpak repair failed"; }
tail -2 /tmp/repair.log
flatpak uninstall --user -y --noninteractive --unused > /tmp/unused.log 2>&1 || { cat /tmp/unused.log; fail "uninstall --unused"; }
for app in org.rpak.test.Plain org.rpak.test.Extra; do
	flatpak info --user "$app" >/dev/null || fail "$app is gone"
	rt=$(flatpak info --user "$app" | sed -n 's/^ *Runtime: //p')
	[ "$rt" = "$forced" ] || fail "$app runs on $rt after repair"
done
[ "$(flatpak run --user org.rpak.test.Plain)" = "rpak plain 3" ] || fail "org.rpak.test.Plain does not run after repair"
[ "$(flatpak run --user org.rpak.test.Extra | sed -n 1p)" = "PAYLOAD 2" ] || fail "org.rpak.test.Extra does not run after repair"
pass "flatpak repair --user and uninstall --unused leave both forced and running"

step "force-runtime: an app uninstalled with flatpak is forgotten"
flatpak uninstall --user -y --noninteractive org.rpak.test.Plain >/dev/null 2>&1
rpak patch | tee /tmp/fr-list.log
! grep -q org.rpak.test.Plain /tmp/fr-list.log || fail "the record of the uninstalled app is still listed"
grep -q org.rpak.test.Extra /tmp/fr-list.log || fail "org.rpak.test.Extra is not listed"
pass "the record of org.rpak.test.Plain is gone, org.rpak.test.Extra's kept"

step "rpak patch --force-runtime on an app installed by flatpak, then rpak update"
# Without the extra-data, flatpak can deploy it without its runtime.
flatpak install --user -y --noninteractive --no-deps upstream org.rpak.test.Plain >/dev/null 2>&1
rt=$(flatpak info --user org.rpak.test.Plain | sed -n 's/^ *Runtime: //p')
[ "$rt" = "$RUNTIME/$arch/$old" ] || fail "installed by flatpak: runtime $rt"
publish_plain 4
state > /tmp/state.before
RPAK_DRY_RUN=1 rpak patch -y --force-runtime="$RUNTIME_BRANCH" org.rpak.test.Plain > /tmp/fr-dry.log 2>&1 || { cat /tmp/fr-dry.log; fail "rpak patch --force-runtime, dry run"; }
cat /tmp/fr-dry.log
state > /tmp/state.after
diff /tmp/state.before /tmp/state.after || fail "the dry run of rpak patch changed files"
grep -q "^rpak: dry run, would rewrite metadata and deploy of org.rpak.test.Plain (.*): runtime $RUNTIME/$arch/$old -> $forced\$" /tmp/fr-dry.log || fail "dry run: no rewrite line"
grep -q "^rpak: dry run, would record org.rpak.test.Plain in .*/patches.json: force-runtime, target $RUNTIME_BRANCH, method patch, $RUNTIME/$arch/$old -> $forced\$" /tmp/fr-dry.log || fail "dry run: no record line"
! grep -q experimental /tmp/fr-dry.log || fail "the dry run announced"
pass "RPAK_DRY_RUN=1 rpak patch --force-runtime says what it would record and rewrite, and changes nothing"
rpak patch -y --force-runtime="$RUNTIME_BRANCH" org.rpak.test.Plain > /tmp/fr-patch.log 2>&1 || { cat /tmp/fr-patch.log; fail "rpak patch --force-runtime"; }
grep '^rpak' /tmp/fr-patch.log
rt=$(flatpak info --user org.rpak.test.Plain | sed -n 's/^ *Runtime: //p')
[ "$rt" = "$forced" ] || fail "rpak patch --force-runtime: runtime $rt"
[ "$(flatpak run --user org.rpak.test.Plain)" = "rpak plain 3" ] || fail "rpak patch changed the version: $(flatpak run --user org.rpak.test.Plain 2>&1)"
pass "rpak patch --force-runtime=$RUNTIME_BRANCH forced the installed org.rpak.test.Plain, still version 3"
rpak patch -y --force-runtime="$RUNTIME_BRANCH" org.rpak.test.Plain > /tmp/fr-patch.log 2>&1 || { cat /tmp/fr-patch.log; fail "rpak patch --force-runtime again"; }
! grep -q experimental /tmp/fr-patch.log || fail "the same patch again announced: $(cat /tmp/fr-patch.log)"
pass "the same rpak patch again prints no experimental line"
rpak update -y --force-runtime="$RUNTIME_BRANCH" org.rpak.test.Plain > /tmp/fr-update.log 2>&1 || { cat /tmp/fr-update.log; fail "rpak update --force-runtime"; }
rt=$(flatpak info --user org.rpak.test.Plain | sed -n 's/^ *Runtime: //p')
[ "$rt" = "$forced" ] || fail "rpak update --force-runtime: runtime $rt"
[ "$(flatpak run --user org.rpak.test.Plain)" = "rpak plain 4" ] || fail "not updated: $(flatpak run --user org.rpak.test.Plain 2>&1)"
pass "rpak update --force-runtime updated it to version 4, still forced"

step "force-runtime: the remote rpak-local and its repository deleted by hand"
flatpak remote-delete --user --force rpak-local >/dev/null
rm -rf "$FLATPAK_USER_DIR/rpak/local-repo"
publish_extra 3
rpak update -y > /tmp/fr-update.log 2>&1 || { cat /tmp/fr-update.log; fail "rpak update without rpak-local"; }
grep '^rpak' /tmp/fr-update.log
flatpak remotes --user --columns=name | grep -qx rpak-local || fail "rpak-local was not added again"
[ "$(cat "$extradir/marker")" = "PAYLOAD 3" ] || fail "org.rpak.test.Extra not updated: $(cat "$extradir/marker")"
rt=$(flatpak info --user org.rpak.test.Extra | sed -n 's/^ *Runtime: //p')
[ "$rt" = "$forced" ] || fail "org.rpak.test.Extra runs on $rt"
pass "rpak update made the repository and the remote again, and updated org.rpak.test.Extra"

step "rpak patch --force-runtime (latest) on an extra-data app installed by flatpak"
# A runtime of busybox alone, org.rpak.test.Platform, branches 1 and 2,
# in upstream: enough for an apply_extra script, so that flatpak itself
# can install an extra-data app first.
publish_runtime() { # branch
	local dir=/tmp/fr-runtime-$1
	rm -rf "$dir"
	# build-export --runtime commits usr/ (it becomes the files of the
	# runtime, /usr in the sandbox), but wants files/ to exist.
	mkdir -p "$dir/usr/bin" "$dir/usr/lib" "$dir/files"
	cp /bin/busybox "$dir/usr/bin/"
	for applet in sh tr rm cat; do ln -s busybox "$dir/usr/bin/$applet"; done
	echo "VERSION_ID=$1" > "$dir/usr/lib/os-release"
	printf '[Runtime]\nname=org.rpak.test.Platform\n' > "$dir/metadata"
	flatpak build-export --runtime --arch="$arch" "$up" "$dir" "$1" >/dev/null
}
publish_extra2() { # version
	local dir payload=$www/payload2-$1.txt
	echo "second $1" > "$payload"
	dir=/tmp/fr-extra2-$1
	rm -rf "$dir"
	mkdir -p "$dir/files/bin" "$dir/export"
	printf '[Application]\nname=org.rpak.test.Extra2\nruntime=org.rpak.test.Platform/%s/1\ncommand=apply_extra\n\n[Extra Data]\nname=payload.txt\nchecksum=%s\nsize=%s\nuri=%s\n' \
		"$arch" "$(sha256sum "$payload" | cut -d' ' -f1)" "$(stat -c %s "$payload")" "$url/payload2-$1.txt" > "$dir/metadata"
	printf '#!/bin/sh\nset -e\ntr a-z A-Z < payload.txt > marker\nrm payload.txt\n. /usr/lib/os-release && echo "$VERSION_ID" > runtime-version\n' > "$dir/files/bin/apply_extra"
	chmod +x "$dir/files/bin/apply_extra"
	flatpak build-export --arch="$arch" "$up" "$dir" stable >/dev/null
}
publish_runtime 1
publish_runtime 2
publish_extra2 1
flatpak install --user -y --noninteractive upstream org.rpak.test.Extra2 >/dev/null 2>&1 || fail "flatpak install org.rpak.test.Extra2"
extra2=$FLATPAK_USER_DIR/app/org.rpak.test.Extra2/$arch/stable/active/files/extra
[ "$(cat "$extra2/runtime-version")" = 1 ] || fail "apply_extra of org.rpak.test.Extra2 ran in $(cat "$extra2/runtime-version")"
publish_extra2 2
rpak patch -y --force-runtime org.rpak.test.Extra2 > /tmp/fr-patch.log 2>&1 || { cat /tmp/fr-patch.log; fail "rpak patch --force-runtime org.rpak.test.Extra2"; }
grep '^rpak' /tmp/fr-patch.log
grep -qxF "rpak: experimental: org.rpak.test.Extra2 was built for org.rpak.test.Platform/$arch/1, now runs on org.rpak.test.Platform/$arch/2; it may break. Undo: rpak patch --reset org.rpak.test.Extra2" /tmp/fr-patch.log ||
	fail "no experimental warning"
[ "$(flatpak info --user --show-origin org.rpak.test.Extra2)" = rpak-local ] || fail "origin $(flatpak info --user --show-origin org.rpak.test.Extra2)"
[ "$(cat "$extra2/runtime-version")" = 2 ] || fail "apply_extra ran in $(cat "$extra2/runtime-version")"
[ "$(cat "$extra2/marker")" = "SECOND 1" ] || fail "rpak patch changed the version: $(cat "$extra2/marker")"
pass "latest found branch 2 in upstream's summary; org.rpak.test.Extra2 moved to rpak-local, apply_extra ran in branch 2, still version 1"

step "rpak patch --reset: the last reexported app"
# org.rpak.test.Extra cannot be reset here (its runtime is nowhere), so it
# goes the way users may go: uninstalled with flatpak, forgotten by rpak.
flatpak uninstall --user -y --noninteractive org.rpak.test.Extra >/dev/null 2>&1
rpak patch > /tmp/fr-list.log
! grep -q '^org.rpak.test.Extra/' /tmp/fr-list.log || fail "org.rpak.test.Extra is still listed"
flatpak remotes --user --columns=name | grep -qx rpak-local || fail "rpak-local went while org.rpak.test.Extra2 comes from it"
state > /tmp/state.before
RPAK_DRY_RUN=1 rpak patch --reset -y org.rpak.test.Extra2 > /tmp/fr-dry.log 2>&1 || { cat /tmp/fr-dry.log; fail "rpak patch --reset, dry run"; }
cat /tmp/fr-dry.log
state > /tmp/state.after
diff /tmp/state.before /tmp/state.after || fail "the dry run of rpak patch --reset changed files"
grep -q "^rpak: dry run, would run: .*flatpak install --user --reinstall -y upstream app/org.rpak.test.Extra2/$arch/stable\$" /tmp/fr-dry.log || fail "dry run: no reinstall"
grep -q "^rpak: dry run, would forget the force-runtime patch of org.rpak.test.Extra2 " /tmp/fr-dry.log || fail "dry run: no forget line"
grep -q "^rpak: dry run, would run: .*flatpak remote-delete --user rpak-local\$" /tmp/fr-dry.log || fail "dry run: no remote-delete"
flatpak remotes --user --columns=name | grep -qx rpak-local || fail "the dry run removed rpak-local"
pass "RPAK_DRY_RUN=1 rpak patch --reset shows the reinstall, the forgotten patch and the removal of rpak-local, and changes nothing"
rpak patch --reset -y org.rpak.test.Extra2 > /tmp/fr-reset.log 2>&1 || { cat /tmp/fr-reset.log; fail "rpak patch --reset"; }
grep '^rpak' /tmp/fr-reset.log
[ "$(flatpak info --user --show-origin org.rpak.test.Extra2)" = upstream ] || fail "origin after reset: $(flatpak info --user --show-origin org.rpak.test.Extra2)"
[ "$(cat "$extra2/runtime-version")" = 1 ] || fail "after reset apply_extra ran in $(cat "$extra2/runtime-version")"
[ "$(cat "$extra2/marker")" = "SECOND 2" ] || fail "after reset: $(cat "$extra2/marker")"
! flatpak remotes --user --columns=name | grep -qx rpak-local || fail "rpak-local is still there"
[ ! -e "$FLATPAK_USER_DIR/rpak/local-repo" ] || fail "local-repo is still there"
rpak patch | tee /tmp/fr-list.log
! grep -q 'org.rpak.test.Extra2' /tmp/fr-list.log || fail "org.rpak.test.Extra2 is still listed"
pass "reset reinstalled org.rpak.test.Extra2 from upstream on branch 1; rpak-local and local-repo are gone"

step "extra-data: org.rpak.test.Extra moves to org.rpak.test.Platform//1, installed by flatpak"
# Upstream rebuilds org.rpak.test.Extra on the busybox runtime, which
# flatpak can install, so that resetting goes back to upstream for real.
# Its extra-data are files of $www; apply_extra upper-cases payload.txt
# into marker and notes the runtime it ran in.
publish_xd() { # version, then NAME FILE...: the extra-data, files of $www
	local version=$1 dir=/tmp/xd-$1 n=0 s
	shift
	rm -rf "$dir"
	mkdir -p "$dir/files/bin" "$dir/export"
	{
		# sdk=: without it flatpak info aborts (it passes its error to
		# g_key_file_get_string() for the sdk and returns success).
		printf '[Application]\nname=org.rpak.test.Extra\nruntime=org.rpak.test.Platform/%s/1\nsdk=org.rpak.test.Platform/%s/1\ncommand=extra\n\n[Extra Data]\n' "$arch" "$arch"
		while [ $# -gt 0 ]; do
			s=
			[ "$n" -eq 0 ] || s=$n
			printf 'name%s=%s\nchecksum%s=%s\nsize%s=%s\nuri%s=%s/%s\n' "$s" "$1" "$s" "$(sha256sum "$www/$2" | cut -d' ' -f1)" \
				"$s" "$(stat -c %s "$www/$2")" "$s" "$url" "$2"
			n=$((n + 1))
			shift 2
		done
	} > "$dir/metadata"
	printf '#!/bin/sh\nset -e\ntr a-z A-Z < payload.txt > marker\nrm payload.txt\n. /usr/lib/os-release && echo "$VERSION_ID" > runtime-version\n' > "$dir/files/bin/apply_extra"
	printf '#!/bin/sh\ncat /app/extra/marker\n' > "$dir/files/bin/extra"
	echo "$version" > "$dir/files/version"
	chmod +x "$dir/files/bin/apply_extra" "$dir/files/bin/extra"
	flatpak build-export --arch="$arch" "$up" "$dir" stable >/dev/null
}
xd=$FLATPAK_USER_DIR/app/org.rpak.test.Extra/$arch/stable/active/files
patches=$FLATPAK_USER_DIR/rpak/patches.json
# The extra-data record of org.rpak.test.Extra: FIELD of it, or of its
# first source (url, sha256, base_url); "none" without one.
xrec() {
	python3 - "$patches" "app/org.rpak.test.Extra/$arch/stable" "$1" <<'PY'
import json, sys
try:
    r = json.load(open(sys.argv[1]))["apps"][sys.argv[2]]["extra-data"]
except (OSError, KeyError):
    print("none")
    sys.exit()
s = r["sources"][0]
print({"url": s["url"], "sha256": s["sha256"], "base_url": s["base"]["url"]}.get(sys.argv[3]) or r[sys.argv[3]])
PY
}
gets() { grep -c "\"GET /$1 " /tmp/http.log || true; }
origin() { flatpak info --user --show-origin org.rpak.test.Extra; }
upcommit() { flatpak remote-info --user -c upstream org.rpak.test.Extra; }
echo "payload 10" > "$www/xd-10.txt"
publish_xd 10 payload.txt xd-10.txt
flatpak install --user -y --noninteractive upstream org.rpak.test.Extra >/dev/null 2>&1 || fail "flatpak install org.rpak.test.Extra"
[ "$(cat "$xd/extra/marker")" = "PAYLOAD 10" ] && [ "$(origin)" = upstream ] || fail "installed: $(cat "$xd/extra/marker"), from $(origin)"
pass "org.rpak.test.Extra 10 installed by flatpak from upstream, payload.txt from $url/xd-10.txt"

step "extra-data: RPAK_DRY_RUN=1 rpak patch --extra-data=URL"
echo "fresh 1" > "$www/new-1.txt"
state > /tmp/state.before
RPAK_DRY_RUN=1 rpak patch -y --extra-data="$url/new-1.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "dry run"; }
cat /tmp/xd.log
state > /tmp/state.after
diff /tmp/state.before /tmp/state.after || fail "the dry run changed files"
[ "$(gets new-1.txt)" = 0 ] || fail "the dry run downloaded new-1.txt"
grep -qxF "rpak: dry run, would download $url/new-1.txt as payload.txt of org.rpak.test.Extra, instead of $url/xd-10.txt" /tmp/xd.log || fail "dry run: no download line"
grep -q "^rpak: dry run, would run: .*flatpak build-export .*rpak.extra-data=payload.txt:<sha256 of payload.txt>" /tmp/xd.log || fail "dry run: no export"
grep -q "^rpak: dry run, would run: .*flatpak install -y --user --reinstall rpak-local app/org.rpak.test.Extra/$arch/stable\$" /tmp/xd.log || fail "dry run: no install"
grep -qxF "rpak: dry run, would record org.rpak.test.Extra in $patches: extra-data payload.txt, $url/new-1.txt instead of $url/xd-10.txt" /tmp/xd.log || fail "dry run: no record line"
! grep -q experimental /tmp/xd.log || fail "the dry run announced"
pass "the dry run shows the download, the export, the install and the record, and downloads and changes nothing"

step "extra-data a: rpak patch --extra-data=URL"
rpak patch -y --extra-data="$url/new-1.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak patch --extra-data"; }
grep '^rpak' /tmp/xd.log
grep -qxF "rpak: Using source payload.txt ($url/xd-10.txt → $url/new-1.txt)" /tmp/xd.log || fail "the chosen source is not said"
grep -q "^rpak: experimental: org.rpak.test.Extra downloads its extra-data payload.txt from $url/new-1.txt (.*) instead of $url/xd-10.txt; " /tmp/xd.log || fail "no experimental line"
[ "$(grep -A1 '^rpak: experimental: org.rpak.test.Extra downloads' /tmp/xd.log | tail -1)" = "rpak: Undo: rpak patch --reset=extra-data org.rpak.test.Extra" ] || fail "the warning does not end with Undo"
[ "$(cat "$xd/extra/marker")" = "FRESH 1" ] || fail "apply_extra had $(cat "$xd/extra/marker")"
[ "$(cat "$xd/version")" = 10 ] || fail "the version changed: $(cat "$xd/version")"
[ "$(origin)" = rpak-local ] || fail "origin $(origin)"
[ "$(xrec url)" = "$url/new-1.txt" ] && [ "$(xrec base_url)" = "$url/xd-10.txt" ] && [ "$(xrec sha256)" = "$(sha256sum "$www/new-1.txt" | cut -d' ' -f1)" ] ||
	fail "patches.json: $(cat "$patches")"
[ "$(xrec upstream_commit)" = "$(upcommit)" ] || fail "upstream_commit $(xrec upstream_commit)"
pass "apply_extra got the new file, still version 10; origin rpak-local; patches.json has url, sha256, upstream's url and commit"
[ "$(gets new-1.txt)" = 1 ] || fail "new-1.txt was asked for $(gets new-1.txt) times"
grep -q "^rpak: [0-9:.]* reusing payload.txt from an earlier run\$" /tmp/xd.log || fail "the install did not reuse rpak's download"
leftover=$(find "$extra" -mindepth 1 2>/dev/null | head -5)
[ -z "$leftover" ] || fail "left in $extra: $leftover"
pass "downloaded once (one GET in the server's log): the install from rpak-local reused the file, nothing is left in $extra"

step "extra-data b: with --force-runtime, then --reset=extra-data, then --reset"
rpak patch -y --force-runtime org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak patch --force-runtime"; }
grep '^rpak' /tmp/xd.log
[ "$(cat "$xd/extra/runtime-version")" = 2 ] && [ "$(cat "$xd/extra/marker")" = "FRESH 1" ] || fail "runtime $(cat "$xd/extra/runtime-version"), $(cat "$xd/extra/marker")"
# Plain --reset would undo the replaced payload.txt too.
grep -qxF "rpak: experimental: org.rpak.test.Extra was built for org.rpak.test.Platform/$arch/1, now runs on org.rpak.test.Platform/$arch/2; it may break. Undo: rpak patch --reset=force-runtime org.rpak.test.Extra" /tmp/xd.log ||
	fail "the experimental line does not undo force-runtime alone"
rpak patch | tee /tmp/xd-list.log
grep -Eq "^org.rpak.test.Extra/$arch/stable +force-runtime +org.rpak.test.Platform/$arch/1 +org.rpak.test.Platform/$arch/2 +reexport +upstream +latest\$" /tmp/xd-list.log || fail "list: no force-runtime line"
grep -Eq "^org.rpak.test.Extra/$arch/stable +extra-data +payload.txt +$url/new-1.txt +$url/xd-10.txt +upstream\$" /tmp/xd-list.log || fail "list: no extra-data line"
pass "both patches at once: apply_extra ran in branch 2 on the new file; rpak patch lists both"
rpak patch -y --reset=extra-data org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "--reset=extra-data"; }
grep '^rpak' /tmp/xd.log
! grep -q "^rpak: installing runtime" /tmp/xd.log || fail "--reset=extra-data installed runtimes"
[ "$(cat "$xd/extra/marker")" = "PAYLOAD 10" ] && [ "$(cat "$xd/extra/runtime-version")" = 2 ] && [ "$(origin)" = rpak-local ] ||
	fail "after --reset=extra-data: $(cat "$xd/extra/marker"), runtime $(cat "$xd/extra/runtime-version"), from $(origin)"
[ "$(xrec url)" = none ] || fail "the extra-data record is left"
rpak patch | grep -q "^org.rpak.test.Extra/.* force-runtime " || fail "the force-runtime patch went too"
pass "--reset=extra-data: upstream's file again, still on branch 2 from rpak-local"
rpak patch -y --reset org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "--reset"; }
grep '^rpak' /tmp/xd.log
[ "$(origin)" = upstream ] && [ "$(cat "$xd/extra/runtime-version")" = 1 ] && [ "$(cat "$xd/extra/marker")" = "PAYLOAD 10" ] ||
	fail "after --reset: from $(origin), runtime $(cat "$xd/extra/runtime-version")"
! flatpak remotes --user --columns=name | grep -qx rpak-local || fail "rpak-local is still there"
[ ! -e "$FLATPAK_USER_DIR/rpak/local-repo" ] || fail "local-repo is still there"
! rpak patch | grep -q org.rpak.test.Extra || fail "still listed"
pass "--reset: back to upstream on branch 1; rpak-local and local-repo are gone"

step "extra-data c: upstream moves on"
echo "fresh 2" > "$www/new-2.txt"
rpak patch -y --extra-data="$url/new-2.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak patch --extra-data"; }
publish_xd 11 payload.txt xd-10.txt
rpak update -y > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak update"; }
grep '^rpak' /tmp/xd.log
[ "$(cat "$xd/version")" = 11 ] && [ "$(cat "$xd/extra/marker")" = "FRESH 2" ] && [ "$(origin)" = rpak-local ] ||
	fail "after the update: version $(cat "$xd/version"), $(cat "$xd/extra/marker"), from $(origin)"
[ "$(xrec upstream_commit)" = "$(upcommit)" ] || fail "upstream_commit $(xrec upstream_commit), upstream has $(upcommit)"
pass "upstream 11 with the same payload.txt: updated to 11, the patch kept, the record on upstream's new commit"
echo "payload 12" > "$www/xd-12.txt"
publish_xd 12 payload.txt xd-12.txt
rpak update -y > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak update"; }
grep '^rpak' /tmp/xd.log
grep -qxF "rpak: org.rpak.test.Extra: upstream updated payload.txt, extra-data patch dropped" /tmp/xd.log || fail "the drop is not said"
[ "$(origin)" = upstream ] && [ "$(cat "$xd/version")" = 12 ] && [ "$(cat "$xd/extra/marker")" = "PAYLOAD 12" ] ||
	fail "after the update: from $(origin), version $(cat "$xd/version"), $(cat "$xd/extra/marker")"
[ "$(xrec url)" = none ] || fail "the record is left"
! flatpak remotes --user --columns=name | grep -qx rpak-local || fail "rpak-local is still there"
pass "upstream 12 changed payload.txt: the patch dropped, org.rpak.test.Extra 12 from upstream, rpak-local gone"

step "extra-data d: two sources, a URL that fits both"
echo "other 13" > "$www/xd-other.txt"
publish_xd 13 payload.txt xd-12.txt other.txt xd-other.txt
flatpak update --user -y --noninteractive org.rpak.test.Extra >/dev/null 2>&1 || fail "flatpak update"
echo "fresh 3" > "$www/new-3.txt"
code=0
rpak patch -y --extra-data="$url/new-3.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || code=$?
cat /tmp/xd.log
[ "$code" = 2 ] || fail "exit $code"
grep -q "cannot tell which extra-data $url/new-3.txt replaces; give the source as NAME=URL, one of:\$" /tmp/xd.log || fail "no explanation"
grep -qxF "  payload.txt  $url/xd-12.txt" /tmp/xd.log && grep -qxF "  other.txt  $url/xd-other.txt" /tmp/xd.log || fail "the sources are not listed"
[ "$(gets new-3.txt)" = 0 ] && [ "$(origin)" = upstream ] || fail "downloaded or changed"
pass "exit 2, both sources listed with their URLs, nothing downloaded"
rpak patch -y --extra-data="payload.txt=$url/new-3.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "NAME=URL"; }
! grep -q "Using source" /tmp/xd.log || fail "a given NAME was chosen"
[ "$(cat "$xd/extra/marker")" = "FRESH 3" ] && [ "$(cat "$xd/extra/other.txt")" = "other 13" ] || fail "after NAME=URL: $(cat "$xd/extra/marker")"
pass "payload.txt=URL replaced payload.txt, other.txt is upstream's"
rpak patch -y --reset org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "--reset"; }

step "extra-data e: a URL that is not there"
commit=$(flatpak info --user --show-commit org.rpak.test.Extra)
code=0
rpak patch -y --extra-data="payload.txt=$url/missing.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || code=$?
grep '^rpak' /tmp/xd.log
[ "$code" = 1 ] || fail "exit $code"
grep -q "404" /tmp/xd.log || fail "no 404"
[ "$(xrec url)" = none ] && [ "$(origin)" = upstream ] && [ "$(flatpak info --user --show-commit org.rpak.test.Extra)" = "$commit" ] || fail "recorded or changed"
leftover=$(find "$extra" -mindepth 1 2>/dev/null | head -5)
[ -z "$leftover" ] || fail "left in $extra: $leftover"
pass "404: exit 1, no record, the deployment and $extra as they were"

step "extra-data f: --force-runtime and --extra-data at once, then flatpak outside rpak"
echo "fresh 4" > "$www/new-4.txt"
rpak patch -y --force-runtime --extra-data="payload.txt=$url/new-4.txt" org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "both"; }
grep '^rpak' /tmp/xd.log
grep -q "^rpak: experimental: org.rpak.test.Extra was built for org.rpak.test.Platform/$arch/1, now runs on org.rpak.test.Platform/$arch/2" /tmp/xd.log &&
	grep -q "^rpak: experimental: org.rpak.test.Extra downloads its extra-data payload.txt from $url/new-4.txt " /tmp/xd.log || fail "not both announced"
[ "$(cat "$xd/extra/marker")" = "FRESH 4" ] && [ "$(cat "$xd/extra/runtime-version")" = 2 ] && [ "$(origin)" = rpak-local ] || fail "not both applied"
pass "one rpak patch made both patches"
publish_xd 14 payload.txt xd-12.txt other.txt xd-other.txt
flatpak update --user -y --noninteractive org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "flatpak update"; }
flatpak repair --user > /tmp/repair.log 2>&1 || { cat /tmp/repair.log; fail "flatpak repair"; }
[ "$(cat "$xd/version")" = 13 ] && [ "$(cat "$xd/extra/marker")" = "FRESH 4" ] && [ "$(origin)" = rpak-local ] ||
	fail "after flatpak update and repair: version $(cat "$xd/version"), $(cat "$xd/extra/marker"), from $(origin)"
[ "$(flatpak run --user org.rpak.test.Extra)" = "FRESH 4" ] || fail "does not run"
pass "flatpak update finds nothing new in rpak-local, flatpak repair leaves it; the app runs"
rpak doctor org.rpak.test.Extra > /tmp/xd-doctor.log 2>&1 || { cat /tmp/xd-doctor.log; fail "rpak doctor"; }
sed -n '/^== Patches/,/^== /p;/^== App org.rpak.test.Extra/,/^Data and state/p' /tmp/xd-doctor.log
grep -q "^  org.rpak.test.Extra/$arch/stable  extra-data: payload.txt from $url/new-4.txt (.*) instead of upstream's $url/xd-12.txt  (upstream remote upstream, upstream commit [0-9a-f]*); deployment: has it\$" /tmp/xd-doctor.log ||
	fail "doctor: no Patches line"
grep -qxF "  extra-data replaced: payload.txt from $url/new-4.txt instead of upstream's $url/xd-12.txt  (rpak patch extra-data, upstream remote upstream)" /tmp/xd-doctor.log || fail "doctor: no app line"
grep -Eq "^  extra-data payload.txt  $url/new-4.txt  HTTP 200, .*  \(installed commit, extra-data replaced by rpak patch extra-data\)\$" /tmp/xd-doctor.log || fail "doctor: the URL used is not checked"
grep -Eq "^  extra-data payload.txt  $url/xd-12.txt  .*\(upstream remote upstream commit, newer than the one reexported: .*; rpak replaced payload.txt\)\$" /tmp/xd-doctor.log || fail "doctor: upstream's URL is not shown"
! grep -Eq "differs from its commit|has no record|not replaced now" /tmp/xd-doctor.log || fail "doctor: $(grep -E 'differs from its commit|has no record|not replaced now' /tmp/xd-doctor.log)"
pass "rpak doctor: the URL used and upstream's, each checked; nothing taken for a problem"
flatpak mask --user org.rpak.test.Extra
recorded=$(xrec upstream_commit)
rpak update -y > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak update, masked"; }
[ "$(cat "$xd/version")" = 13 ] && [ "$(xrec upstream_commit)" = "$recorded" ] || fail "masked, but updated or recorded: version $(cat "$xd/version")"
pass "masked: rpak update neither reexports nor records it, flatpak does not update it"
flatpak mask --user --remove org.rpak.test.Extra
rpak update -y > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "rpak update"; }
grep '^rpak' /tmp/xd.log
[ "$(cat "$xd/version")" = 14 ] && [ "$(cat "$xd/extra/marker")" = "FRESH 4" ] && [ "$(cat "$xd/extra/runtime-version")" = 2 ] &&
	[ "$(origin)" = rpak-local ] && [ "$(xrec upstream_commit)" = "$(upcommit)" ] || fail "rpak update did not bring 14 with both patches"
pass "unmasked, rpak update brings upstream 14 with both patches"
rpak patch -y --reset org.rpak.test.Extra > /tmp/xd.log 2>&1 || { cat /tmp/xd.log; fail "--reset"; }
[ "$(origin)" = upstream ] && [ "$(cat "$xd/extra/marker")" = "PAYLOAD 12" ] || fail "after the last reset"
! flatpak remotes --user --columns=name | grep -qx rpak-local || fail "rpak-local is still there"
pass "reset: org.rpak.test.Extra 14 from upstream as built, rpak-local gone"

kill "$http_pid" 2>/dev/null || true
unset no_proxy NO_PROXY

step "final checks"
flatpak repair --user > /tmp/repair.log 2>&1 || { cat /tmp/repair.log; fail "flatpak repair failed"; }
pass "flatpak repair --user"
rpak clean --user --dry-run | tail -1
leftover=$(find "$repo/objects" -name '*.commitmeta' -size +100k)
[ -z "$leftover" ] || fail "big commitmeta left: $leftover"
pass "no commitmeta over 100 KiB left in the repository"
state /var/lib/flatpak | diff /tmp/system.before - || fail "the system installation changed"
pass "the system installation with the runtime is unchanged"

step "E2E PASSED"
