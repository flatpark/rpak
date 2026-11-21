#!/bin/sh
# Generates app.commit, the commit object flatpak build-export makes for a
# tiny app (its metadata is xa.metadata), and app.commit.sum, its checksum,
# for the tests that read a deployed commit's metadata; extra.commit and
# extra.commit.sum the same for an app with extra-data. Needs flatpak; it
# only writes to a temporary repository, no installation is touched.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
commit() { # id name, then metadata lines after [Application]
	id=$1 name=$2
	shift 2
	dir="$tmp/$id"
	mkdir -p "$dir/files/bin" "$dir/export"
	printf '#!/bin/sh\n' > "$dir/files/bin/run"
	{
		printf '[Application]\nname=%s\nruntime=org.freedesktop.Platform/x86_64/25.08\nsdk=org.freedesktop.Sdk/x86_64/25.08\ncommand=run\n' "$id"
		printf '%s\n' "$@"
	} > "$dir/metadata"
	flatpak build-export --arch=x86_64 --no-update-summary "$tmp/repo" "$dir" stable >/dev/null
	sum=$(cat "$tmp/repo/refs/heads/app/$id/x86_64/stable")
	cp "$tmp/repo/objects/$(echo "$sum" | cut -c1-2)/$(echo "$sum" | cut -c3-).commit" "$here/$name.commit"
	echo "$sum" > "$here/$name.commit.sum"
}
commit org.rpak.Deploy app
# An app with extra-data, which rpak reexports.
commit org.rpak.DeployExtra extra '' '[Extra Data]' name=payload.txt \
	checksum=0000000000000000000000000000000000000000000000000000000000000001 size=10 uri=https://example.org/payload.txt
