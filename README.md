# rpak

rpak is a wrapper around flatpak. It aims to accept every flatpak command
and option, and adds two things (and an experimental third, see
[patch](#patch-experimental)):

- **Parallel downloads.** `install` and `update` download the installers
  of extra-data apps (most proprietary apps on FlatPark) several at a time;
  flatpak alone fetches them one after another.
- **Cleaning up.** flatpak keeps a copy of every such installer in its
  repository. rpak frees it after each install or update, and `rpak clean`
  frees it and other leftovers for everything installed before.

## Installation

```sh
curl -Lo ~/.local/bin/rpak "https://github.com/flatpark/rpak/releases/latest/download/rpak-linux-$(uname -m)"
chmod +x ~/.local/bin/rpak
```

Optionally add `alias flatpak=rpak` to your shell's startup file. To build
from source: `make build` (needs Go).

## Usage

Use `rpak` where you would use `flatpak`:

```sh
rpak install flatpark com.google.Chrome
rpak update
rpak clean
rpak list --app          # other commands run flatpak unchanged
```

### install and update

flatpak asks for confirmation as usual while rpak downloads in the
background. If you answer before the downloads are done, their progress
shows until they finish, then flatpak goes on:

```
Proceed with these changes to the user installation? [Y/n]:
    chrome.deb  ███████████▏      74%  107.2 / 144.8 MB    9.6 MB/s  00:04
    Total 0/1   ███████████▏      74%  107.2 / 144.8 MB    9.6 MB/s  00:04
```

Answering `n` cancels; what was downloaded is kept for the next run. With
`-y`, or when not on a terminal, rpak downloads first and then runs
flatpak. A failed download never stops the install: flatpak then downloads
that file itself.

### clean

```
rpak clean [--user | --system | --installation=NAME]... [--dry-run] [--delete-data] [-v]
```

Frees the installers cached in the repositories, leftovers of interrupted
downloads and installs, and old deployments (`.removed`). The data of apps
that are no longer installed (`~/.var/app/ID`, overrides) is only listed;
add `--delete-data` to delete it.

An old deployment that a running app still uses (the app was updated while
it ran) stays until the app quits, as with flatpak. rpak clean lists those
with the app and the pid of its sandbox (as `flatpak ps` shows it in
CHILD-PID), and does not count them as freed:

```
  Old deployments still in use
     757.0 MB  .removed/com.tencent.WeChat-2c45506193f71d4a…c4df35c8d067bcf47ee18  used by com.tencent.WeChat (pid 1234)
  Quit them and run rpak clean again
```

```
$ rpak clean
User installation  ~/.local/share/flatpak
  Installers cached in commits
     144.8 MB  com.google.Chrome  deployed
  Freed 144.8 MB

Freed 144.8 MB (1 item)
```

### doctor

```
rpak doctor [APP_ID]...
```

Collects what is needed to look into a problem: rpak, flatpak, the
distribution and kernel, the environment variables that matter, user
namespaces, portals, graphics cards and GL extensions, every installation
with its remotes, locks, free space, end-of-life refs and overrides, the
extra-data directory, and how the remotes answer. With app ids, also where
each app is installed, its runtime, permissions and overrides, and how the
servers of its extra-data files answer, with the size they give beside the
pinned one: for the installed commit and the one the remote has now, or
for an app that is not installed, for the remotes that have it.

doctor reports facts and measurements, and judges none of them: what they
mean is for the maintainers to read. It exits with 0 once it has collected
the report. It only reads: it changes no files, takes no locks and runs
only flatpak's query commands, none at all if the flatpak it finds is rpak
itself. It needs the network and takes a few seconds.

When you report a problem, paste the whole output into the issue. Home
directories and user names are replaced, passwords in proxy URLs hidden,
and the files shared with an app are only counted.

```
$ rpak doctor com.google.Chrome
...
== App com.google.Chrome
User installation
  ref: app/com.google.Chrome/x86_64/stable
  ...
  runtime: org.freedesktop.Platform/x86_64/26.08  installed in User installation
  ...
  extra-data chrome.deb  https://dl.google.com/...  HTTP 200, 147.5 MB (pinned 147.5 MB)  (installed and remote commit)
...
```

### patch (experimental)

```
rpak install --force-runtime[=TARGET] [--user] [OPTION]... [REMOTE] APP...
rpak update --force-runtime[=TARGET] [OPTION]... APP...
rpak patch --force-runtime[=TARGET] [-y] APP...
rpak install|update|patch --extra-data=[NAME=]URL... [--force-runtime[=TARGET]] [OPTION]... [REMOTE] APP
rpak patch [--list]
rpak patch --reset[=PATCH] [-y] APP...
```

**Experimental.** `--force-runtime` makes apps run on a newer branch of
their runtime than the one they were built for, without waiting for
upstream: an app on Flathub still built for
`org.freedesktop.Platform//25.08` runs on `26.08`. The old runtime is then
not downloaded, and `flatpak uninstall --unused` can remove it; before a
major runtime upgrade you can try out how your apps do on the new one. The
apps have not been tested on that runtime and may break; rpak checks
nothing about compatibility. Every time it forces an app it says so:

```
rpak: experimental: com.example.App was built for org.freedesktop.Platform/x86_64/25.08, now runs on org.freedesktop.Platform/x86_64/26.08; it may break. Undo: rpak patch --reset com.example.App
```

- `rpak install --force-runtime [REMOTE] APP...` installs apps forced;
  without `REMOTE`, from the one remote of the user installation that has
  them (in more than one: say which);
- `rpak patch --force-runtime APP...` forces apps that are installed, at the
  version they are;
- `rpak update --force-runtime APP...` updates apps and forces them.

`TARGET` is `latest` (the default: the newest branch of the same form,
`YY.MM`, `N` or KDE's `5.15-YY.MM`, found in the remotes and the installed
runtimes; KDE 5.15 never moves to 6), a branch (`26.08`), or a whole
runtime `ID/ARCH/BRANCH`, which may name another runtime (your call). An
app is only ever moved up: if upstream is already on the target or past
it, nothing is forced. Giving `--force-runtime=TARGET` again for a patched
app changes its target.

**Experimental too.** `--extra-data=[NAME=]URL` makes an app download its
extra-data file `NAME` from `URL` instead of where its packagers point:
a vendor's newer release the manifest has not caught up with, a link that
went dead, an older release to go back to, a beta. rpak downloads the file
(once: the install that follows reuses it), takes its checksum and size,
and installs the app from a commit of upstream's with just that source
changed; the app's installer script, permissions and files stay
upstream's. The file is your choice, nobody else checked it:

```
rpak: Using source code.deb (https://vscode.download.prss.microsoft.com/.../code_1.147.0-1794903003_amd64.deb → https://vscode.download.prss.microsoft.com/.../code_1.148.0-1795000000_amd64.deb)
rpak: experimental: com.visualstudio.code downloads its extra-data code.deb from https://... (110.2 MB, sha256 ...) instead of https://...; that file is your choice, its packagers did not pick it.
rpak: Undo: rpak patch --reset=extra-data com.visualstudio.code
```

- One app at a time; `--extra-data` may be given again for another of its
  files, and with `--force-runtime` (one export does both).
- Without `NAME`, rpak takes the app's only extra-data file, or the one
  whose URL is clearly the closest to `URL`: on the same host, sharing the
  most leading directories (on github.com, the same owner/repo) and no
  other as many. A vendor's new release usually sits next to its old one,
  support files (FlatPark's prebuilt libraries, Debian packages, a JRE) do
  not. Anything less clear is an error listing the files, `NAME` and URL,
  so that you say which; rpak never guesses from version numbers.
- The patch lasts while upstream keeps the file it replaced: when upstream
  publishes a commit with the same file (it changed something else),
  `rpak update` applies the patch to it; when upstream changes that file
  itself (it caught up, or moved on), the patch is dropped and the app
  comes from its remote again: `rpak: com.example.App: upstream updated
  NAME, extra-data patch dropped`. To stay on a file, `flatpak mask` the
  app: neither flatpak nor rpak updates it then.
- `flatpak info` still shows upstream's version (it comes from the app's
  metainfo); `rpak patch` and `rpak doctor` show the URL used and
  upstream's.
- A failed download changes and records nothing.

rpak remembers what it patched (`rpak patch` lists it, from
`<user installation>/rpak/patches.json`: the runtime change, `(pinned)`
when a target was given, and the replaced extra-data URLs; `rpak doctor`
shows the rest: method, upstream remote and commit, checksums, and how
each deployment stands now); later `rpak update` and
`rpak install` keep it, with or without APP arguments. `rpak patch --reset
APP` undoes an app's patches, `--reset=force-runtime` or
`--reset=extra-data` just one kind: rpak forgets them and reinstalls the
app from its remote as it was built, or, if the other kind is left,
reexports it with that one alone. Only the user installation is
supported.

How it works: flatpak runs an app on the runtime its deployed `metadata`
names. For most apps rpak installs them with `--no-deps` and rewrites the
deployed `metadata` (and the runtime in the deployment's `deploy` file)
after flatpak deployed them (method *patch*); runtimes the apps then need
are installed afterwards. Apps with extra-data run their installer script
in the runtime while being deployed, so rpak downloads upstream's commit
itself, checks every object against the checksum the remote's signed
summary gives, changes the runtime in its metadata and exports it into a
local repository, `<user installation>/rpak/local-repo`, which flatpak
installs the app from (method *reexport*, remote `rpak-local`, added when
first needed and removed with the last such app). `--extra-data` always
goes that way: what flatpak downloads is in the commit (`[Extra Data]` of
its metadata, from which `flatpak build-export` makes
`xa.extra-data-sources`). The data in `~/.var/app` stays where it is.

Things to know:

- `flatpak update` without rpak deploys a patched app as upstream built it:
  it pulls the old runtime and runs on it, as before. The next `rpak
  update` forces it again. A reexported app gets no updates from flatpak
  alone (its remote is rpak's); `rpak update` checks upstream and exports
  anew. `rpak doctor` shows how each patched app stands.
- An app uninstalled with flatpak is forgotten by the next rpak command.
- When `rpak update` updates both installations and an app of the user
  installation is patched, flatpak runs twice, once per installation, so
  that `--no-deps` stays in the user installation: you may be asked twice.
- Extensions of the app that follow the runtime's version (codecs, GL)
  are named in the new version, but only the runtime is installed for you.
- Reexporting needs upstream to serve loose objects over http(s) or
  `file:` (Flathub and FlatPark do); a repository that only offers static
  deltas is refused, and `rpak patch` needs upstream to still have the
  installed commit. The Locale and Debug extensions of a reexported app
  are not reexported and so not installed.

### Environment

| Variable | |
|---|---|
| `RPAK_JOBS=N` | concurrent downloads, 1–16, default 4 |
| `RPAK_DRY_RUN=1` | show the plan and the flatpak command, run nothing |
| `RPAK_FLATPAK=PATH` | flatpak to run (default: `flatpak` from `PATH`) |

## Shell completion

```sh
source <(rpak completion zsh)     # ~/.zshrc, after compinit
source <(rpak completion bash)    # ~/.bashrc
rpak completion fish > ~/.config/fish/completions/rpak.fish
```

rpak's own commands are completed by rpak, everything else by flatpak.

## Limitations

- GUI stores and automatic updates call flatpak directly, so they get
  neither parallel downloads nor cleanup; `rpak clean` frees what they
  leave.
- Only apps named on the command line, or pending updates, are downloaded
  ahead; dependencies the transaction adds are left to flatpak.
- After rpak frees a commit's cached installers, `--no-pull` cannot deploy
  that commit again.

## Development

```sh
make lint test build   # ./rpak
make e2e               # end-to-end test in podman (cached, about a minute)
```

## License

MIT, see [LICENSE](LICENSE).
