package forceruntime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/ostree"
)

// Runner runs a flatpak command and returns its standard output (see
// plan.ExecRunner).
type Runner interface {
	Output(ctx context.Context, argv ...string) ([]byte, error)
}

// Marks in the body of a reexported commit: which upstream commit it was
// made from, and the runtime it was given.
const (
	markUpstream = "rpak.upstream="
	markRuntime  = "rpak.runtime="
)

// Body is the commit body of a reexport.
func Body(upstream string, runtime Pref) string {
	return markUpstream + upstream + "\n" + markRuntime + runtime.String()
}

// ParseBody reads the marks of a reexported commit's body.
func ParseBody(body string) (upstream, runtime string) {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, markUpstream); ok {
			upstream = v
		}
		if v, ok := strings.CutPrefix(line, markRuntime); ok {
			runtime = v
		}
	}
	return upstream, runtime
}

// Exported returns the upstream commit and runtime of the commit ref has in
// the local repository; ok is false if it has none.
func Exported(installation, ref string) (upstream, runtime string, ok bool) {
	repo := RepoPath(installation)
	b, err := os.ReadFile(filepath.Join(repo, "refs", "heads", filepath.FromSlash(ref)))
	if err != nil {
		return "", "", false
	}
	sum := strings.TrimSpace(string(b))
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(ostree.ObjectPath(sum, "commit"))))
	if !ostree.ValidChecksum(sum) || err != nil {
		return "", "", false
	}
	c, err := ostree.ParseCommit(data)
	if err != nil {
		return "", "", false
	}
	upstream, runtime = ParseBody(c.Body)
	return upstream, runtime, upstream != ""
}

// Export makes the local repository's ref the upstream commit with the
// runtime of r: the commit is checked out from src, verified object by
// object against upstream (a checksum flatpak got from the remote's signed
// summary), its metadata rewritten, and the result committed with flatpak
// build-export, which creates the repository (archive-z2) the first time
// and makes xa.metadata and xa.extra-data-sources from the new metadata
// (the extra-data themselves are unchanged). The summary is not updated:
// call Publish once all refs are exported. ref is app/ID/ARCH/BRANCH.
func Export(ctx context.Context, flatpak Runner, installation string, src ostree.Source, ref, upstream string, r Resolution) error {
	parts := strings.Split(ref, "/")
	if len(parts) != 4 || parts[0] != "app" {
		return fmt.Errorf("%s is not an app ref", ref)
	}
	commit, err := ostree.ReadCommit(ctx, src, upstream)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(installation), 0o755); err != nil {
		return err
	}
	// What an interrupted export left; the caller holds rpak's lock, so no
	// other export is running.
	if old, err := filepath.Glob(filepath.Join(Dir(installation), "build-*")); err == nil {
		for _, dir := range old {
			os.RemoveAll(dir)
		}
	}
	tmp, err := os.MkdirTemp(Dir(installation), "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	build := filepath.Join(tmp, "app")
	if err := ostree.Checkout(ctx, src, commit, build); err != nil {
		return err
	}
	metaPath := filepath.Join(build, "metadata")
	text, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("the commit has no metadata: %w", err)
	}
	if xa, ok, _ := commit.MetadataString("xa.metadata"); ok && xa != string(text) {
		return fmt.Errorf("the metadata file of %s is not its xa.metadata", upstream)
	}
	orig, err := RuntimeOf(string(text))
	if err != nil {
		return err
	}
	if orig != r.Original {
		return fmt.Errorf("upstream's %s is built for %s, not %s", upstream[:12], orig, r.Original)
	}
	if r.Forced {
		out, err := RewriteMetadata(string(text), r.Original, r.Runtime)
		if err != nil {
			return err
		}
		if err := os.WriteFile(metaPath, []byte(out), 0o644); err != nil {
			return err
		}
	}
	// build-export wants export/, which build-finish makes.
	if err := os.MkdirAll(filepath.Join(build, "export"), 0o755); err != nil {
		return err
	}
	var extra []string
	// An end of life upstream declared stays declared (flatpak shows it
	// on update). build-export takes the id the rebase is to, the commit
	// has the whole ref (OSTREE_COMMIT_META_KEY_ENDOFLIFE{,_REBASE}).
	if eol, ok, _ := commit.MetadataString("ostree.endoflife"); ok {
		extra = append(extra, "--end-of-life="+eol)
	}
	if rebase, ok, _ := commit.MetadataString("ostree.endoflife-rebase"); ok {
		if p := strings.Split(rebase, "/"); len(p) == 4 {
			extra = append(extra, "--end-of-life-rebase="+p[1])
		}
	}
	_, err = flatpak.Output(ctx, exportArgv(installation, ref, build, upstream, subject(commit.Subject, r), r, extra)...)
	return err
}

// exportArgv is the build-export command that exports the checkout build
// of ref, made from the upstream commit, into the local repository.
func exportArgv(installation, ref, build, upstream, subject string, r Resolution, extra []string) []string {
	parts := strings.Split(ref, "/")
	argv := []string{"build-export", "--arch=" + parts[2], "--no-update-summary",
		"--subject=" + subject, "--body=" + Body(upstream, r.Runtime)}
	argv = append(argv, extra...)
	return append(argv, RepoPath(installation), build, parts[3])
}

// ExportPreview is the build-export command Export would run, for a dry
// run: without reading the commit, the checkout and the subject (and any
// end of life) are placeholders.
func ExportPreview(installation, ref, upstream string, r Resolution) []string {
	short := upstream
	if len(short) > 12 {
		short = short[:12]
	}
	return exportArgv(installation, ref, "<checkout of "+short+">", upstream, subject("<upstream's subject>", r), r, nil)
}

// PublishArgv is the command Publish runs.
func PublishArgv(installation string) []string {
	return []string{"build-update-repo", "--no-update-appstream", "--prune", "--prune-depth=0", RepoPath(installation)}
}

// RemoteAddArgv is the command EnsureRemote runs when the remote is missing.
func RemoteAddArgv(installation string) []string {
	return []string{"remote-add", "--user", "--no-gpg-verify", "--no-enumerate", "--if-not-exists",
		RemoteName, "file://" + RepoPath(installation)}
}

// RemoteDeleteArgv is the command RemoveLocalRepo runs if the remote is
// configured.
func RemoteDeleteArgv() []string { return []string{"remote-delete", "--user", RemoteName} }

func subject(upstream string, r Resolution) string {
	if r.Forced {
		return fmt.Sprintf("%s (rpak: runtime %s)", upstream, r.Runtime)
	}
	return upstream
}

// Publish updates the local repository's summary and drops every commit
// but the latest of each ref, and the objects only they used: flatpak
// build-update-repo --prune --prune-depth=0 (flatpak_repo_prune()).
func Publish(ctx context.Context, flatpak Runner, installation string) error {
	_, err := flatpak.Output(ctx, PublishArgv(installation)...)
	return err
}

// Unexport removes refs from the local repository; Publish then prunes
// their objects. Refs that are not there are no error.
func Unexport(installation string, refs []string) error {
	for _, ref := range refs {
		err := os.Remove(filepath.Join(RepoPath(installation), "refs", "heads", filepath.FromSlash(ref)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Remote is a remote of an installation, from its repo/config.
type Remote struct {
	Name, URL, Subset string
	Disabled          bool
}

// Remotes reads the remotes of an installation from <installation>/repo/
// config (the [remote "NAME"] groups flatpak remote-add writes).
func Remotes(installation string) ([]Remote, error) {
	text, err := os.ReadFile(filepath.Join(installation, "repo", "config"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Remote
	for _, g := range metadata.KeyFile(string(text)) {
		name, ok := strings.CutPrefix(g.Name, `remote "`)
		if !ok || !strings.HasSuffix(name, `"`) {
			continue
		}
		out = append(out, Remote{
			Name:     name[:len(name)-1],
			URL:      g.Values["url"],
			Subset:   g.Values["xa.subset"],
			Disabled: g.Values["xa.disable"] == "true",
		})
	}
	return out, nil
}

// FindRemote is the remote called name.
func FindRemote(installation, name string) (Remote, bool) {
	remotes, _ := Remotes(installation)
	for _, r := range remotes {
		if r.Name == name {
			return r, true
		}
	}
	return Remote{}, false
}

// EnsureRemote adds the local repository as the remote RemoteName, if it is
// not there: unsigned (it is made here, from verified commits), and not
// enumerated, so that it is not offered when the user installs by id; an
// explicit "flatpak install rpak-local REF" still finds the ref
// (flatpak_dir_list_all_remote_refs() does not filter, unlike
// flatpak_dir_list_remote_refs()). It reports whether it added it.
func EnsureRemote(ctx context.Context, flatpak Runner, installation string) (bool, error) {
	if _, ok := FindRemote(installation, RemoteName); ok {
		return false, nil
	}
	_, err := flatpak.Output(ctx, RemoteAddArgv(installation)...)
	return err == nil, err
}

// LocalUsers lists the refs installed in the installation whose origin is
// RemoteName.
func LocalUsers(installation string) []string {
	var out []string
	for _, kind := range []string{"app", "runtime"} {
		for _, ref := range Installed(installation, kind) {
			if d, ok, err := Active(installation, ref); err == nil && ok && d.Origin == RemoteName {
				out = append(out, ref)
			}
		}
	}
	return out
}

// LocalCleanup is what to do with the local remote and repository once
// records changed.
type LocalCleanup int

const (
	// KeepLocal: some record still reexports, or nothing is there.
	KeepLocal LocalCleanup = iota
	// RemoveLocal: no record reexports and nothing installed comes from
	// the remote: delete the remote and the repository.
	RemoveLocal
	// KeepLocalInUse: no record reexports, but installed refs still come
	// from the remote; deleting it would orphan them.
	KeepLocalInUse
)

// DecideLocalCleanup decides about the local remote and repository after
// records were removed: they go when the last reexport record went and no
// installed ref has the remote as its origin any more. exists reports
// whether the remote or the repository is there at all.
func DecideLocalCleanup(records []Record, users []string, exists bool) LocalCleanup {
	if !exists {
		return KeepLocal
	}
	for _, r := range records {
		if r.Method == MethodReexport {
			return KeepLocal
		}
	}
	if len(users) > 0 {
		return KeepLocalInUse
	}
	return RemoveLocal
}

// RemoveLocalRepo deletes the remote (if configured) and the repository.
func RemoveLocalRepo(ctx context.Context, flatpak Runner, installation string) error {
	if _, ok := FindRemote(installation, RemoteName); ok {
		if _, err := flatpak.Output(ctx, RemoteDeleteArgv()...); err != nil {
			return err
		}
	}
	return os.RemoveAll(RepoPath(installation))
}
