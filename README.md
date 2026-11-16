# rpak

rpak is a wrapper around flatpak. It aims to accept every flatpak command
and option, and adds two things:

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
downloads and installs, and old deployments. The data of apps that are no
longer installed (`~/.var/app/ID`, overrides) is only listed; add
`--delete-data` to delete it.

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
