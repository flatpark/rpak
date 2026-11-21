#!/bin/sh
# Generates repo/, an ostree repository in archive mode committed by ostree
# itself, for the checksum and checkout tests: regular files (executable,
# empty, compressible, binary), a symlink, nested directories and, where
# the file system takes user xattrs, a file with two of them given out of
# order. COMMIT holds the commit checksum. Needs ostree; run it from
# anywhere, it replaces repo/ and COMMIT.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d "$here/.gen-XXXXXX")
trap 'rm -rf "$tmp"' EXIT

tree=$tmp/tree
mkdir -p "$tree/files/bin" "$tree/files/share/doc" "$tree/export"
printf '[Application]\nname=org.example.Fixture\nruntime=org.freedesktop.Platform/x86_64/25.08\ncommand=hello\n' > "$tree/metadata"
printf '#!/bin/sh\necho hello\n' > "$tree/files/bin/hello"
chmod 755 "$tree/files/bin/hello"
: > "$tree/files/share/doc/empty"
yes 'compressible line' | head -2000 > "$tree/files/share/doc/big.txt"
head -c 4096 /dev/urandom > "$tree/files/share/doc/random.bin"
ln -s ../share/doc/big.txt "$tree/files/bin/link"
chmod 750 "$tree/files/share"
printf 'x' > "$tree/files/xattrs"
python3 -c 'import os, sys
try:
    os.setxattr(sys.argv[1], "user.zz", b"1")
    os.setxattr(sys.argv[1], "user.aa", b"2")
except OSError as e:
    print("no xattrs:", e)' "$tree/files/xattrs"

ostree init --repo="$tmp/repo" --mode=archive-z2
ostree commit --repo="$tmp/repo" --branch=app/org.example.Fixture/x86_64/stable \
	--subject="fixture" --body="rpak.upstream=x" "$tree" > "$tmp/COMMIT"
ostree fsck --repo="$tmp/repo" >/dev/null
rm -rf "$here/repo"
mkdir -p "$here/repo"
cp -r "$tmp/repo/objects" "$here/repo/"
cp "$tmp/COMMIT" "$here/COMMIT"
