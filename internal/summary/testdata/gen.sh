#!/bin/sh
# Generates the summaries in this directory with flatpak's own tools: a
# repository with two small apps, one with extra-data on two branches,
# exported by flatpak build-export and indexed by build-update-repo, which
# writes summary.idx with its subsummaries (summary version 1, xa.data) and
# the compat summary for old clients (version 0, xa.cache). Run it from
# anywhere; it needs flatpak and replaces repo/.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

app() { # id branch, then [Extra Data] lines
	id=$1 branch=$2
	shift 2
	dir="$tmp/$id-$branch"
	mkdir -p "$dir/files/bin" "$dir/export"
	printf '#!/bin/sh\n' > "$dir/files/bin/run"
	{
		printf '[Application]\nname=%s\nruntime=org.freedesktop.Platform/x86_64/25.08\ncommand=run\n' "$id"
		if [ $# -gt 0 ]; then
			printf '\n[Extra Data]\n'
			printf '%s\n' "$@"
		fi
	} > "$dir/metadata"
	flatpak build-export --arch=x86_64 --no-update-summary "$tmp/repo" "$dir" "$branch" >/dev/null
}
app org.example.Extra stable name=app.deb checksum="$(printf '%064d' 1)" size=1000 uri=https://example.org/app-1.0.deb
app org.example.Extra beta name=app.deb checksum="$(printf '%064d' 2)" size=2000 uri=https://example.org/app-2.0.deb
app org.example.Plain stable

flatpak build-update-repo "$tmp/repo" >/dev/null
rm -rf "$here/repo"
mkdir -p "$here/repo/summaries"
cp "$tmp/repo/summary" "$tmp/repo/summary.idx" "$here/repo/"
cp "$tmp/repo/summaries/"*.gz "$here/repo/summaries/"
