# rpak

rpak wraps `flatpak update` and `flatpak install`: it first downloads the
original installers that extra-data apps need, **several at a time**, and then
runs your system's flatpak with exactly the arguments you gave it. Afterwards
it frees the copy of those installers that flatpak keeps in its repository,
and `rpak clean` does the same for everything installed before.

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
rpak clean --dry-run                # what rpak clean would free (see below)
rpak list --app                     # any other command: rpak execs flatpak with it as is
```

Only `install` and `update` (and its alias `upgrade`, also after leading
`-v`/`--verbose`/`--ostree-verbose`) prefetch, and `clean` is rpak's own.
Every other command, and `--help` on any of them but `clean`, replaces rpak
with flatpak right away, so `rpak` can stand in for `flatpak` everywhere,
aliases included.

Example output (one line per event when stderr is not a terminal; on a
terminal, a block with a line per active download and a summary line is
redrawn in place, and finished files are listed above it):

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
flatpak. After flatpak, if it succeeded and there was space to free (see
[Cleaning up](#cleaning-up)), rpak prints one more line:

```
rpak: freed 29.0 MB of cached extra-data
```

| Command / variable | Effect |
|---|---|
| `rpak --version` | rpak's version, then `flatpak --version` |
| `rpak help`, `rpak --help` | rpak's usage (`flatpak --help` for flatpak's) |
| `RPAK_JOBS=N` | concurrent downloads, 1–16, default 4 (at most 4 per host) |
| `RPAK_DRY_RUN=1` | only print the prefetch plan and the flatpak command; no downloads, flatpak is not run, nothing is cleaned |
| `RPAK_FLATPAK=/path/to/flatpak` | flatpak executable to use (default: `flatpak` from `PATH`) |

Downloads honour `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`.

## Shell completion

`rpak completion bash|zsh|fish` prints a completion script:

```sh
# zsh: a file named _rpak in a directory in $fpath, e.g. with
# fpath=(~/.zfunc $fpath) before compinit in ~/.zshrc:
rpak completion zsh > ~/.zfunc/_rpak
# or in ~/.zshrc, after compinit:
source <(rpak completion zsh)

# bash, in ~/.bashrc:
source <(rpak completion bash)

# fish:
rpak completion fish > ~/.config/fish/completions/rpak.fish
```

The scripts are flatpak's, calling `rpak complete` instead of
`flatpak complete` (same protocol). rpak completes what it implements
itself: the command name, with `clean` added to flatpak's commands, and
the options of `rpak clean`, which are not those of `flatpak clean` in
FlatPark's fork (no `--delete-data`). For every other command, `install`
and `update` included, rpak asks your flatpak and passes its answer on, so
refs, remotes and options are completed exactly as for flatpak.

With `alias flatpak=rpak`, zsh expands the alias before completing, so
rpak's completion is all it needs. In bash, also run
`complete -o nospace -F __rpak flatpak` after loading rpak's script.

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

## Cleaning up

When stock flatpak pulls an extra-data app, it puts the downloaded
installers, whole, into the detached metadata of the commit
(`repo/objects/XX/CHECKSUM.commitmeta`, key `xa.extra-data`). Once the app is
deployed they are never read again (only `flatpak install --no-pull` of the
same commit would), yet they stay there, one more copy with every update of
the app. That is easily gigabytes. The same file also holds the commit's
signatures (`ostree.gpgsigs`), so it cannot simply be deleted.

**After `rpak install` and `rpak update`.** When flatpak exits with 0, rpak
removes `xa.extra-data` from every commit in the repositories of the
installations the command used (`--user`, `--system`, `--installation=NAME`,
or by default the user and the default system installation), and prints a
total. It never waits: if another flatpak is busy with a repository, rpak
skips it with a warning. A system installation that rpak cannot write to (you
are not root) is only looked at: if there is something to free, rpak says
`run "sudo rpak clean --system" to free X`. Nothing here changes rpak's exit
code, which is always flatpak's. `RPAK_DRY_RUN=1` skips it, and so does
`--no-deploy`: what that pulled is deployed later with `--no-pull`, from the
installers in the repository.

**`rpak clean`** does that for all installations, and removes leftovers:

```
rpak clean [--user | --system | --installation=NAME]... [--dry-run] [-v]
```

- `xa.extra-data` in the detached metadata of every commit, deployed or not.
  Only that key is removed; every other entry (signatures and all) is copied
  byte for byte.
- rpak's own prefetch directories in `<user base dir>/extra-data`: those with
  a `.rpak-prefetch` marker, including `.part` files of interrupted downloads
  and files kept for a retry. Directories without the marker are never
  touched, flatpak's own `.flatpak-prefetch` ones included. Skipped while
  another rpak is prefetching.
- Leftovers: directories under `repo/refs/{heads,remotes,mirrors}` that hold
  only empty directories; `repo/state/CHECKSUM.commitpartial` markers of
  commits that no longer exist; checkouts of interrupted deploys
  (`{app,runtime}/ID/ARCH/BRANCH/.CHECKSUM-XXXXXX`); `ID/ARCH/BRANCH`
  directories that an install interrupted before its first deploy left
  empty (nothing inside a deployment is ever looked at); and old
  deployments in `.removed` that no running app holds a lock on, which is
  what flatpak itself does after most transactions.
- The data of apps that are no longer installed in any installation
  (`~/.var/app/ID` and per-user overrides) is **listed, never deleted**: it
  is your data. rpak has no `--delete-data`.

Without options it looks at the default system installation, the user
installation and every installation configured in
`/etc/flatpak/installations.d`, with a heading for each. Each removed item
gets a line, and a total ends the output:

```
$ rpak clean
System installation (default):
2410.3 MB  flathub:app/com.example.Big/x86_64/stable
System installation (default): would free 2.5 GB; run "sudo rpak clean --system"
User installation:
10.7 MB  deploy/app/app.audionaut.Audionaut/x86_64/stable
0.0 MB  /home/me/.local/share/flatpak/repo/refs: 4 empty directories
Data of apps that are no longer installed:
120.4 MB  com.example.Old  ~/.var/app  overrides
Kept 126.2 MB; rpak does not delete app data
Freed: 2 objects, 0.01 GB
```

A system installation you cannot write to is only inspected, like above; one
you name explicitly (`--system`) is an error then. `--dry-run` only reports
and changes nothing at all, not even lock files. `-v` also lists what is kept
and why. `rpak clean` exits with 1 if an installation could not be cleaned
(busy, not writable, an I/O error), 2 for a usage error.

How it works, and why it is safe:

- The detached metadata is a GVariant `a{sv}`. rpak reads its framing
  (following GLib's `gvariant-serialiser.c`), drops the `xa.extra-data`
  entry and writes the others back unchanged with new framing. The result is
  byte for byte what GLib would serialise for the same dictionary without
  that key; the test suite checks this against fixtures that GLib made,
  including a real commitmeta. A file that is not in GVariant normal form,
  or anything else rpak is not sure about, is left alone with a warning. The
  new file is written next to the old one, synced, given the old one's
  permissions and renamed over it. The installers are memory-mapped, not
  read.
- While changing a repository, rpak holds the same two locks as `flatpak
  clean` of FlatPark's flatpak fork, without waiting for either: ostree's
  repository lock `repo/.lock`, and flatpak's `repo-lock`, both exclusive.
  They are OFD locks (`fcntl(F_OFD_SETLK)`) like the ones libglnx and ostree
  take, so they really exclude a running flatpak; `flock()` would not see
  them. Pulls and deploys hold both shared.
- The rules for leftovers, and the output, are those of `flatpak clean` in
  [FlatPark's flatpak fork](https://github.com/flatpark/flatpak), which does
  the same in C; items even come in the same order. rpak works with the
  stock flatpak of your distribution. `test/compare-fork.sh` runs both on
  the same throwaway installation and diffs their output.

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
- Installs and updates that do not go through rpak keep adding installers to
  the repository until the next `rpak install`, `rpak update` or
  `rpak clean`.
- A commit stripped of `xa.extra-data` cannot be deployed again without
  downloading: `flatpak install --no-pull` or `flatpak update --no-pull` of
  that same commit fails, a normal install or update downloads the
  installers again.
- `rpak clean` does not remove `-origin` remotes of uninstalled bundles, and
  only lists the data of uninstalled apps (`flatpak clean --delete-data` of
  the fork deletes it and resets the app's portal permissions).
