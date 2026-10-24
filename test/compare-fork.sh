#!/bin/bash
# Runs FlatPark's flatpak fork (flatpak clean) and rpak clean on the same
# synthetic installation in a temporary directory and shows both outputs
# side by side. Needs the fork as $FORK_FLATPAK (default: flatpak) and
# ostree. Nothing outside the temporary directory is touched: HOME and all
# FLATPAK_* locations point into it.
#
# Usage: test/compare-fork.sh [--dry-run]
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
rpak=$here/../rpak
fork=${FORK_FLATPAK:-flatpak}
"$fork" clean --help >/dev/null 2>&1 || { echo "$fork has no clean command" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# make DIR: one installation with each kind of leftover, apps and data
make() {
	local u=$1 home=$2
	mkdir -p "$u" && ostree init --repo="$u/repo" --mode=bare-user-only
	mkdir -p "$u/repo/refs/remotes/deleted-remote/app/org.example.Gone/x86_64"
	mkdir -p "$u/repo/refs/heads/deploy/app/org.example.Gone"
	mkdir -p "$u/repo/state" && touch "$u/repo/state/$(printf '%064x' 1).commitpartial"
	mkdir -p "$u/app/org.example.Gone/x86_64/stable/.$(printf '%064x' 2)-AbC123/files"
	head -c 300000 /dev/zero > "$u/app/org.example.Gone/x86_64/stable/.$(printf '%064x' 2)-AbC123/files/big"
	mkdir -p "$u/app/org.example.Empty/x86_64/stable"
	# an installed app, with data
	mkdir -p "$u/app/org.example.Here/x86_64/stable/$(printf '%064x' 3)/files"
	ln -s "$(printf '%064x' 3)" "$u/app/org.example.Here/x86_64/stable/active"
	ln -s x86_64/stable "$u/app/org.example.Here/current"
	mkdir -p "$home/.var/app/org.example.Here" && echo here > "$home/.var/app/org.example.Here/f"
	# apps that are gone, with data and/or overrides, and names that are not apps
	mkdir -p "$home/.var/app/org.example.Gone/cache" && head -c 200000 /dev/zero > "$home/.var/app/org.example.Gone/cache/x"
	mkdir -p "$u/overrides" && printf '[Context]\nshared=network;\n' > "$u/overrides/org.example.Gone"
	printf '[Context]\n' > "$u/overrides/org.example.OnlyOverride"
	mkdir -p "$home/.var/app/not-an-id" "$home/.var/app/org.1digit.App"
}

run() { # name, command...
	local name=$1
	shift
	rm -rf "$work/$name" && mkdir -p "$work/$name/home" "$work/$name/system" "$work/$name/conf"
	make "$work/$name/user" "$work/$name/home"
	(
		cd "$work/$name"
		HOME=$work/$name/home FLATPAK_USER_DIR=$work/$name/user FLATPAK_SYSTEM_DIR=$work/$name/system \
			FLATPAK_CONFIG_DIR=$work/$name/conf "$@" 2>&1 | sed "s|$work/$name|<tmp>|g"
	) > "$work/$name.out"
	find "$work/$name/user" "$work/$name/home" -mindepth 1 | sed "s|$work/$name|<tmp>|" | grep -v '/repo/objects\|/repo/tmp\|/repo/config\|/repo/extensions' | sort > "$work/$name.left"
}

run fork "$fork" clean --user "$@"
run rpak "$rpak" clean --user "$@"
echo "=== flatpak clean --user $*          (fork)"
cat "$work/fork.out"
echo "=== rpak clean --user $*"
cat "$work/rpak.out"
echo "=== diff of the outputs"
diff "$work/fork.out" "$work/rpak.out" && echo "(identical)"
echo "=== diff of what is left afterwards"
diff "$work/fork.left" "$work/rpak.left" && echo "(identical)"
