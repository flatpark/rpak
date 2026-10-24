# rpak

rpak wraps `flatpak update` and `flatpak install`: it first downloads the
original installers that extra-data apps need, **several at a time**, and then
runs your system's flatpak with exactly the arguments you gave it.

## Why

Many proprietary apps on FlatPark are extra-data apps: the repository only
holds the packaging, and flatpak downloads the vendor's installer (tens of MB
to over a GB) when the app is installed or updated. Stock flatpak downloads
these **one after another over a single connection**, which is slow when
several such apps update at once. rpak does not replace or patch flatpak. It
is a single static binary that lives next to your distribution's flatpak and
only places the files, before flatpak runs, in the local directory flatpak
checks first.

## Installation

Download the binary for your architecture and put it in your `PATH`:

```sh
curl -Lo ~/.local/bin/rpak https://github.com/flatpark/rpak/releases/latest/download/rpak-linux-amd64   # aarch64: rpak-linux-arm64
chmod +x ~/.local/bin/rpak
```

To build from source (needs Go; the result is a static `CGO_ENABLED=0` binary):

```sh
make build        # ./rpak
make dist         # dist/rpak-linux-amd64, dist/rpak-linux-arm64
```

## Usage

Type `rpak` instead of `flatpak`. The arguments go to flatpak unchanged:

```sh
rpak update                         # flatpak update, after prefetching what the pending updates need
rpak update --user -y com.google.Chrome
rpak install flatpark com.google.Chrome us.zoom.Zoom
```

Example output (one line per event when stderr is not a terminal; on a
terminal it is a single progress line that updates in place):

```
rpak: prefetching 3 extra-data file(s), 39.8 MiB:
  app/app.audionaut.Audionaut/x86_64/stable  audionaut.deb  10.7 MiB  github.com
  app/com.ferrispad.FerrisPad/x86_64/stable  FerrisPad-linux  13.3 MiB  github.com
  app/ai.storyteller.soundcraft/x86_64/stable  soundcraft.tar.gz  15.7 MiB  github.com
rpak: 13:48:55.881 start soundcraft.tar.gz (github.com)
rpak: 13:48:55.881 start audionaut.deb (github.com)
rpak: 13:49:05.607 done audionaut.deb, 10.7 MiB in 9.7s
rpak: 13:49:05.607 start FerrisPad-linux (github.com)
rpak: 13:49:06.194 done soundcraft.tar.gz, 15.7 MiB in 10.3s
rpak: 13:49:10.060 done FerrisPad-linux, 13.3 MiB in 4.5s
rpak: prefetch done: 3 ready (3 downloaded, 0 reused), 0 skipped/failed, 14.2s
... flatpak's own output follows
```

If there is nothing to prefetch, rpak prints nothing and goes straight to
flatpak.

| Command / variable | Effect |
|---|---|
| `rpak --version`, `rpak help` | version, usage |
| `RPAK_JOBS=N` | concurrent downloads, 1–16, default 4 (at most 2 per host) |
| `RPAK_DRY_RUN=1` | only print the prefetch plan and the flatpak command; no downloads, flatpak is not run |
| `RPAK_FLATPAK=/path/to/flatpak` | flatpak executable to use (default: `flatpak` from `PATH`) |

Downloads honour `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`.

## How it works

Before downloading an extra-data file, stock flatpak checks whether it is
already available locally. This is long-standing upstream code, in
`flatpak_dir_pull_extra_data_to_bytes()` in `common/flatpak-dir.c`. The path
it checks is:

```
<user base dir>/extra-data/<sha256>/<name>
```

`<user base dir>` is `$FLATPAK_USER_DIR`, otherwise `$XDG_DATA_HOME/flatpak`,
otherwise `~/.local/share/flatpak`. It is the directory of **the user running
flatpak** even when installing into the system installation. If the file
exists, flatpak reads all of it and checks its size and sha256. If either is
wrong, the install fails; flatpak does not fall back to downloading. If the
file does not exist, flatpak downloads it as usual.

What rpak does:

1. Parses the arguments (with flatpak 1.18's option tables) to find the refs,
   remotes and installations involved. For `update` it uses
   `flatpak remote-ls --updates`. For `install` it recognises REMOTE the way
   flatpak does. Without a REMOTE, it only prefetches when exactly one remote
   has the ref.
2. Reads each ref's `[Extra Data]` (name, sha256, size, URL) with
   `flatpak remote-info --show-metadata`.
3. Downloads concurrently to `extra-data/<sha256>/<name>.part`. The file is
   renamed to `<name>` only after its size and sha256 match, so a file under
   the real name is never incomplete or unverified. The directory gets a
   `.rpak-prefetch` marker. rpak never writes to or deletes a directory or
   file without that marker (one the user or another tool put there). A
   `.part` left by an interrupted run is resumed with a Range request.
4. Runs `flatpak update|install ARGS...` unchanged. flatpak inherits the
   terminal, and SIGINT/SIGTERM/SIGHUP are forwarded to it. rpak exits with
   flatpak's exit code, or 128+signal if flatpak was killed. When flatpak
   uses a local file, `-v` shows `Loading extra-data from local file ...`.
5. If flatpak succeeds, rpak removes the marked directories it placed. If
   flatpak fails, or you answer "n" at its prompt, they are kept and the next
   attempt reuses them: they are verified again but not downloaded again. At
   startup rpak removes marked directories older than 3 days.

**Prefetching is best effort.** Nothing that goes wrong in it stops or
changes the flatpak run: an unparsable command line, a failing `remote-info`,
network errors, checksum mismatches, a full disk. Each problem prints one
warning and skips that ref or file. flatpak gets byte-for-byte the arguments
you typed, so the worst case is the same as running flatpak directly. A wrong
path or name is just a miss, and flatpak downloads the file itself.

Ctrl-C while prefetching stops all downloads and keeps the `.part` files.
rpak then exits with 130 (143 for SIGTERM) without running flatpak.

## Limitations

- Only commands run through rpak prefetch anything. GUI stores (GNOME
  Software, KDE Discover) and background automatic updates call flatpak
  directly.
- rpak prefetches only the refs named on the command line, or listed as
  pending updates by `update`. Refs that the transaction pulls in on its own
  (dependencies, related refs such as a newly added extra-data extension) are
  downloaded serially by flatpak as usual.
- Nothing is prefetched with `--no-pull`, `--commit`, `--bundle`, `--from`,
  `--image`, `--sideload-repo` or `--appstream`, or when installing from a
  file or URL.
- flatpak reads the whole local file into memory. It does the same with the
  files it downloads itself.
- Without installation options, `update` looks at the user installation and
  the default system installation only. Other system installations are only
  covered when named with `--installation=`.
- Files are not locked while flatpak reads them. If two rpak runs install the
  same app at the same time, the worst case is that one misses and flatpak
  downloads that file itself.
