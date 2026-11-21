package forceruntime

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flatpark/rpak/internal/gvariant"
	"github.com/flatpark/rpak/internal/ostree"
)

// Deployment is the active deployment of an installed app or runtime.
type Deployment struct {
	Ref string // app/ID/ARCH/BRANCH
	// Dir is the deployment directory itself, <ref dir>/<commit>, which
	// <ref dir>/active links to.
	Dir    string
	Origin string
	Commit string
}

// Active returns the active deployment of ref in an installation, if any
// (flatpak_dir_get_if_deployed(): <installation>/<ref>/active).
func Active(installation, ref string) (Deployment, bool, error) {
	active := filepath.Join(installation, filepath.FromSlash(ref), "active")
	dir, err := filepath.EvalSymlinks(active)
	if errors.Is(err, fs.ErrNotExist) {
		return Deployment{}, false, nil
	}
	if err != nil {
		return Deployment{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "deploy"))
	if err != nil {
		return Deployment{}, false, err
	}
	d, err := gvariant.ParseDeployData(data)
	if err != nil {
		return Deployment{}, false, fmt.Errorf("%s: %w", filepath.Join(dir, "deploy"), err)
	}
	return Deployment{Ref: ref, Dir: dir, Origin: d.Origin, Commit: d.Commit}, true, nil
}

// Installed lists the refs of a kind ("app" or "runtime") with an active
// deployment in an installation.
func Installed(installation, kind string) []string {
	matches, _ := filepath.Glob(filepath.Join(installation, kind, "*", "*", "*", "active"))
	var refs []string
	for _, m := range matches {
		if _, err := os.Stat(m); err != nil {
			continue
		}
		rel, err := filepath.Rel(installation, filepath.Dir(m))
		if err == nil {
			refs = append(refs, filepath.ToSlash(rel))
		}
	}
	return refs
}

// MetadataFile is the deployment's metadata, which flatpak run reads.
func (d Deployment) MetadataFile() (string, error) {
	b, err := os.ReadFile(filepath.Join(d.Dir, "metadata"))
	return string(b), err
}

// CommitMetadata is the xa.metadata of a commit in the installation's
// repository: the metadata the commit was built with, which deploying
// checks the checked-out metadata file against (validate_commit_metadata()).
func CommitMetadata(installation, commit string) (string, error) {
	if !ostree.ValidChecksum(commit) {
		return "", fmt.Errorf("%q is not a commit checksum", commit)
	}
	data, err := os.ReadFile(filepath.Join(installation, "repo", filepath.FromSlash(ostree.ObjectPath(commit, "commit"))))
	if err != nil {
		return "", err
	}
	c, err := ostree.ParseCommit(data)
	if err != nil {
		return "", err
	}
	text, ok, err := c.MetadataString("xa.metadata")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("commit %s has no xa.metadata", commit)
	}
	return text, nil
}

// Patch makes a deployment run on the runtime the metadata text names:
// the metadata file becomes text and the "runtime" of the deploy file
// that runtime, which is where flatpak looks when it works out which apps
// use a runtime (flatpak_deploy_data_get_runtime(), for uninstall's
// "needed by" check). The deploy file's other bytes stay as they are; a
// deploy file without "runtime" (deploy-version < 4) is left alone.
//
// The metadata file is a hard link to an object of the repository, so both
// files are replaced (WriteFileAtomic), never written through. It reports
// whether anything changed.
func Patch(d Deployment, text string) (bool, error) {
	runtime, err := RuntimeOf(text)
	if err != nil {
		return false, err
	}
	changed := false
	metaPath := filepath.Join(d.Dir, "metadata")
	fi, err := os.Stat(metaPath)
	if err != nil {
		return false, err
	}
	current, err := os.ReadFile(metaPath)
	if err != nil {
		return false, err
	}
	if string(current) != text {
		if err := WriteFileAtomic(metaPath, []byte(text), fi.Mode().Perm()); err != nil {
			return false, err
		}
		changed = true
	}

	deployPath := filepath.Join(d.Dir, "deploy")
	fi, err = os.Stat(deployPath)
	if err != nil {
		return changed, err
	}
	data, err := os.ReadFile(deployPath)
	if err != nil {
		return changed, err
	}
	dd, err := gvariant.ParseDeployData(data)
	if err != nil {
		return changed, fmt.Errorf("%s: %w", deployPath, err)
	}
	if old, ok := dd.String("runtime"); !ok || old == runtime.String() {
		return changed, nil
	}
	dd.SetString("runtime", runtime.String())
	out := dd.Serialize()
	if bytes.Equal(out, data) {
		return changed, nil
	}
	if err := WriteFileAtomic(deployPath, out, fi.Mode().Perm()); err != nil {
		return changed, err
	}
	return true, nil
}

// NeedsPatch reports whether Patch(d, text) would change anything: the
// metadata file is not text, or the deploy file names another runtime. It
// only reads, for dry runs.
func NeedsPatch(d Deployment, text string) (bool, error) {
	runtime, err := RuntimeOf(text)
	if err != nil {
		return false, err
	}
	current, err := os.ReadFile(filepath.Join(d.Dir, "metadata"))
	if err != nil {
		return false, err
	}
	if string(current) != text {
		return true, nil
	}
	deployPath := filepath.Join(d.Dir, "deploy")
	data, err := os.ReadFile(deployPath)
	if err != nil {
		return false, err
	}
	dd, err := gvariant.ParseDeployData(data)
	if err != nil {
		return false, fmt.Errorf("%s: %w", deployPath, err)
	}
	old, ok := dd.String("runtime")
	return ok && old != runtime.String(), nil
}

// RuntimeInstalled reports whether a runtime has an active deployment in
// one of the installations.
func RuntimeInstalled(installations []string, p Pref) bool {
	for _, inst := range installations {
		if _, err := os.Stat(filepath.Join(inst, "runtime", p.ID, p.Arch, p.Branch, "active")); err == nil {
			return true
		}
	}
	return false
}

// InstalledBranches lists the branches of a runtime installed in any of the
// installations.
func InstalledBranches(installations []string, id, arch string) []string {
	var out []string
	for _, inst := range installations {
		for _, ref := range Installed(inst, "runtime") {
			parts := strings.Split(ref, "/")
			if len(parts) == 4 && parts[1] == id && parts[2] == arch {
				out = append(out, parts[3])
			}
		}
	}
	return out
}
