// Package clean removes what stock flatpak leaves behind in an
// installation, following `flatpak clean` of FlatPark's flatpak fork
// (app/flatpak-builtins-clean.c and common/flatpak-extra-data-private.c on
// the flatpark/1.18.4 branch):
//
//   - the copies of extra-data installers that flatpak keeps in the
//     detached metadata of every commit it pulled (StripExtraData),
//   - leftovers in the repository and of interrupted deploys
//     (RepoLeftovers, InterruptedDeploys, Removed),
//   - rpak's own prefetch directories (PrefetchDirs),
//   - the data of apps that are no longer installed, which is the user's
//     and only deleted on request (UninstalledAppData, RemoveAppData).
//
// Callers lock the installation first (Lock) unless DryRun is set.
package clean

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/flatpark/rpak/internal/gvariant"
	"github.com/flatpark/rpak/internal/lock"
	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// ExtraDataKey is the detached-metadata key flatpak stores the installers
// in, as a(ayay): (name, contents) pairs.
const ExtraDataKey = "xa.extra-data"

// Cleaner runs the clean-ups and counts what they free.
type Cleaner struct {
	// DryRun only reports.
	DryRun bool
	// Item is called for every thing removed (or that would be), with
	// the bytes freed; may be nil.
	Item func(size int64, what string)
	// Verbose explains what is left alone and why; may be nil.
	Verbose func(format string, a ...any)
	// Warn reports a problem; the clean-up goes on with the next thing.
	Warn func(format string, a ...any)

	Objects int
	Freed   int64
	// Failed counts problems that left something that should have been
	// removed in place.
	Failed int
}

func (c *Cleaner) report(size int64, what string) {
	c.Objects++
	c.Freed += size
	if c.Item != nil {
		c.Item(size, what)
	}
}

func (c *Cleaner) verbose(format string, a ...any) {
	if c.Verbose != nil {
		c.Verbose(format, a...)
	}
}

func (c *Cleaner) warn(format string, a ...any) {
	if c.Warn != nil {
		c.Warn(format, a...)
	}
}

func (c *Cleaner) fail(format string, a ...any) {
	c.Failed++
	c.warn(format, a...)
}

// Lock takes the locks flatpak clean holds while cleaning an installation
// (clean_repo()): ostree's repository lock exclusively, then flatpak's
// repo-lock exclusively; neither waits. lock.ErrBusy means that a flatpak
// (or ostree) process is working on the installation.
func Lock(installation string) (release func(), err error) {
	repoLock, err := lock.OstreeRepo(filepath.Join(installation, "repo"))
	if err != nil {
		return nil, err
	}
	deployLock, err := lock.Glnx(filepath.Join(installation, "repo-lock"))
	if err != nil {
		repoLock.Release()
		return nil, err
	}
	return func() {
		deployLock.Release()
		repoLock.Release()
	}, nil
}

// Writable reports whether the repository can be changed by this process,
// as clean_repo() checks it: access (repo, W_OK).
func Writable(installation string) bool {
	return unix.Access(filepath.Join(installation, "repo"), unix.W_OK) == nil
}

// HasRepo reports whether the installation has a repository; flatpak only
// opens one that exists (flatpak_dir_maybe_ensure_repo()).
func HasRepo(installation string) bool {
	info, err := os.Stat(filepath.Join(installation, "repo"))
	return err == nil && info.IsDir()
}

// --- a. extra-data in detached commit metadata

// StripExtraData removes xa.extra-data from the detached metadata of every
// commit in the repository, deployed or not, as clean_commit_metadata()
// and flatpak_repo_remove_extra_data() do. Everything else in the file is
// kept byte for byte; see package gvariant. Files that are not in normal
// form are skipped with a warning.
func (c *Cleaner) StripExtraData(repo string) {
	checksums, err := commitsWithMetadata(repo)
	if err != nil {
		c.fail("%v", err)
		return
	}
	refs := commitRefs(repo)
	for _, checksum := range checksums {
		freed, removed, err := c.stripCommit(repo, checksum)
		if err != nil {
			continue
		}
		if !removed {
			continue
		}
		what := refs[checksum]
		if what == "" {
			what = checksum[:12]
		}
		c.report(freed, what)
	}
}

// commitsWithMetadata lists the commits that have detached metadata,
// sorted: objects/XX/YYYY.commitmeta next to objects/XX/YYYY.commit.
// A commitmeta without its commit is not a commit to flatpak and is left
// alone.
func commitsWithMetadata(repo string) ([]string, error) {
	objects := filepath.Join(repo, "objects")
	dirs, err := os.ReadDir(objects)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(objects, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			rest, ok := strings.CutSuffix(f.Name(), ".commitmeta")
			if !ok || !f.Type().IsRegular() {
				continue
			}
			checksum := d.Name() + rest
			if !metadata.ValidChecksum(checksum) {
				continue
			}
			if info, err := os.Lstat(objectPath(repo, checksum, "commit")); err != nil || !info.Mode().IsRegular() {
				continue
			}
			out = append(out, checksum)
		}
	}
	sort.Strings(out)
	return out, nil
}

func objectPath(repo, checksum, kind string) string {
	return filepath.Join(repo, "objects", checksum[:2], checksum[2:]+"."+kind)
}

// stripCommit rewrites one commitmeta without xa.extra-data and returns
// the bytes freed. Problems are warned about here.
func (c *Cleaner) stripCommit(repo, checksum string) (int64, bool, error) {
	path := objectPath(repo, checksum, "commitmeta")
	f, err := os.Open(path)
	if err != nil {
		c.fail("%v", err)
		return 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		c.fail("%v", err)
		return 0, false, err
	}
	if info.Size() == 0 { // an empty a{sv}
		return 0, false, nil
	}
	// Mapped, not read: the installers in there can be gigabytes, and
	// only the framing at the end of each entry is ever looked at.
	data, err := unix.Mmap(int(f.Fd()), 0, int(info.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		c.fail("%s: %v", path, err)
		return 0, false, err
	}
	defer unix.Munmap(data)
	out, removed, err := gvariant.RemoveKey(data, ExtraDataKey)
	if err != nil {
		c.warn("%s: not changed: %v", path, err)
		return 0, false, err
	}
	if !removed {
		c.verbose("%s: no %s", path, ExtraDataKey)
		return 0, false, nil
	}
	freed := info.Size() - int64(len(out))
	if c.DryRun {
		return freed, true, nil
	}
	if err := replaceFile(path, info, out); err != nil {
		c.fail("%s: %v", path, err)
		return 0, false, err
	}
	return freed, true, nil
}

// replaceFile replaces path with data: a temporary file in the same
// directory, written, synced, given the old file's permission bits (and,
// for root, owner), renamed over the old one, and the directory synced.
func replaceFile(path string, old fs.FileInfo, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".rpak-tmp-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(old.Mode().Perm() | old.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	if st, ok := old.Sys().(*unix.Stat_t); ok && os.Geteuid() == 0 {
		if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// commitRefs maps commits to the ref to show for them, as
// add_ref_to_commit_map() builds it from ostree_repo_list_refs(): local
// refs as "name", remote ones as "remote:name"; app/ and runtime/ refs win
// over others. Refs are visited in sorted order, so that ties are broken
// the same way every time (flatpak's hash table order is arbitrary).
func commitRefs(repo string) map[string]string {
	refs := listRefs(repo)
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	preferred := func(ref string) bool {
		return strings.HasPrefix(ref, "app/") || strings.HasPrefix(ref, "runtime/")
	}
	out := make(map[string]string)
	for _, name := range names {
		checksum := refs[name]
		old, ok := out[checksum]
		if !ok || (!preferred(old) && preferred(name)) {
			out[checksum] = name
		}
	}
	return out
}

// listRefs reads refs/heads and refs/remotes/REMOTE the way ostree's
// enumerate_refs_recurse() and add_ref_to_set() do: every regular file
// or symlink whose path components are valid ref fragments, holding a
// checksum. refs/mirrors are not listed by ostree_repo_list_refs().
func listRefs(repo string) map[string]string {
	refs := make(map[string]string)
	walkRefs(filepath.Join(repo, "refs", "heads"), "", refs)
	remotes, _ := os.ReadDir(filepath.Join(repo, "refs", "remotes"))
	for _, r := range remotes {
		if r.IsDir() {
			walkRefs(filepath.Join(repo, "refs", "remotes", r.Name()), r.Name()+":", refs)
		}
	}
	return refs
}

func walkRefs(dir, prefix string, refs map[string]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !refFragment(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			walkRefs(path, prefix+e.Name()+"/", refs)
		case e.Type().IsRegular() || e.Type()&fs.ModeSymlink != 0:
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			refs[prefix+e.Name()] = strings.TrimRight(string(b), " \t\n\r\f\v")
		}
	}
}

// refFragment is _ostree_validate_ref_fragment(): [-._\w\d]+.
func refFragment(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '-' || c == '.' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// --- b. rpak's prefetch directories

// PrefetchDirs removes every directory directly in <user base
// dir>/extra-data that carries rpak's marker: files kept for a retry,
// .part files of interrupted downloads, temporary directories. Anything
// without the marker, flatpak's own prefetch directories included, is left
// alone.
func (c *Cleaner) PrefetchDirs(userBase string) {
	root := filepath.Join(userBase, "extra-data")
	entries, err := readDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		c.fail("%v", err)
		return
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if !e.IsDir() {
			c.verbose("keeping %s: not a directory", path)
			continue
		}
		marker, err := os.Lstat(filepath.Join(path, prefetch.MarkerName))
		if err != nil || !marker.Mode().IsRegular() {
			c.verbose("keeping %s: no %s marker", path, prefetch.MarkerName)
			continue
		}
		size := diskUsage(path)
		if !c.DryRun {
			if err := os.RemoveAll(path); err != nil {
				c.fail("%v", err)
				continue
			}
		}
		c.report(size, path)
	}
}

// --- c. leftovers

// RepoLeftovers is clean_repo_leftovers() without the -origin remotes:
// directories under refs/{heads,remotes,mirrors} that hold nothing but
// empty directories, and state/CHECKSUM.commitpartial markers of commits
// that are gone.
func (c *Cleaner) RepoLeftovers(repo string) {
	removed := 0
	for _, kind := range []string{"heads", "remotes", "mirrors"} {
		c.emptyDirs(filepath.Join(repo, "refs", kind), false, -1, &removed)
	}
	if removed > 0 {
		noun := "directories"
		if removed == 1 {
			noun = "directory"
		}
		c.report(0, fmt.Sprintf("%s: %d empty %s", filepath.Join(repo, "refs"), removed, noun))
	}

	states, _ := readDir(filepath.Join(repo, "state"))
	for _, e := range states {
		checksum, ok := strings.CutSuffix(e.Name(), ".commitpartial")
		if !ok || !metadata.ValidChecksum(checksum) {
			continue
		}
		_, err := os.Lstat(objectPath(repo, checksum, "commit"))
		if !errors.Is(err, fs.ErrNotExist) {
			// The commit is there (or cannot be checked): a pull of it
			// is unfinished, or it is not ours to judge.
			continue
		}
		path := filepath.Join(repo, "state", e.Name())
		if !c.DryRun {
			if err := os.Remove(path); err != nil {
				c.fail("%v", err)
				continue
			}
		}
		c.report(0, path)
	}
}

// InterruptedDeploys is clean_interrupted_deploys(): checkouts that
// flatpak_dir_deploy() did not get to rename into place,
// {app,runtime}/ID/ARCH/BRANCH/.CHECKSUM-XXXXXX, and ID directories that
// an install interrupted before its first deployment left with nothing
// but empty ARCH/BRANCH directories. Nothing below BRANCH is looked into:
// empty directories in a deployment are mount points.
func (c *Cleaner) InterruptedDeploys(installation string) {
	for _, kind := range []string{"app", "runtime"} {
		ids, _ := readDir(filepath.Join(installation, kind))
		for _, id := range ids {
			idDir := filepath.Join(installation, kind, id.Name())
			arches, _ := readDirNoFollow(idDir)
			for _, arch := range arches {
				archDir := filepath.Join(idDir, arch.Name())
				branches, _ := readDirNoFollow(archDir)
				for _, branch := range branches {
					branchDir := filepath.Join(archDir, branch.Name())
					entries, _ := readDirNoFollow(branchDir)
					for _, e := range entries {
						if !e.IsDir() || !isDeployTmpdir(e.Name()) {
							continue
						}
						path := filepath.Join(branchDir, e.Name())
						size := diskUsage(path)
						if !c.DryRun {
							if err := os.RemoveAll(path); err != nil {
								c.fail("%v", err)
								continue
							}
						}
						c.report(size, path)
					}
				}
			}
			removed := 0
			if c.emptyDirs(idDir, true, 2, &removed) {
				c.report(0, idDir)
			}
		}
	}
}

// readDirNoFollow lists dir unless it is a symlink, like
// g_file_enumerate_children with G_FILE_QUERY_INFO_NOFOLLOW_SYMLINKS.
func readDirNoFollow(dir string) ([]fs.DirEntry, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}
	return readDir(dir)
}

// readDir lists dir in the order g_file_enumerate_children() returns its
// entries, so that items come out in the order flatpak clean prints them:
// GLib's local enumerator reads up to 1000 entries at a time and sorts
// each batch by inode number (next_file_helper() in
// gio/glocalfileenumerator.c).
func readDir(dir string) ([]fs.DirEntry, error) {
	entries, err := readDirents(dir)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(entries); start += 1000 {
		chunk := entries[start:min(start+1000, len(entries))]
		sort.SliceStable(chunk, func(i, j int) bool { return chunk[i].ino < chunk[j].ino })
	}
	out := make([]fs.DirEntry, len(entries))
	for i := range entries {
		out[i] = &entries[i]
	}
	return out, nil
}

// isDeployTmpdir is is_deploy_tmpdir(): flatpak_dir_deploy() checks out
// into ".<checksum>-XXXXXX" and renames that into place when done.
func isDeployTmpdir(name string) bool {
	if len(name) < 1+64+7 || name[0] != '.' {
		return false
	}
	for i := 1; i <= 64; i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return name[65] == '-'
}

// emptyDirs is clean_empty_dirs(): it removes the directories below dir
// that hold nothing but other empty directories, and dir itself if
// removeSelf. Only maxDepth levels below dir are descended into (-1: no
// limit); a directory at the limit only counts as empty if it has no
// entries at all, except deploy checkouts that a dry run is about to
// remove. It returns whether dir is (or would be) empty.
func (c *Cleaner) emptyDirs(dir string, removeSelf bool, maxDepth int, removed *int) bool {
	entries, err := readDirNoFollow(dir)
	if err != nil {
		return false
	}
	empty := true
	for _, e := range entries {
		switch {
		case !e.IsDir():
			empty = false
		case maxDepth == 0:
			if !(c.DryRun && isDeployTmpdir(e.Name())) {
				empty = false
			}
		default:
			next := maxDepth - 1
			if maxDepth < 0 {
				next = -1
			}
			if !c.emptyDirs(filepath.Join(dir, e.Name()), true, next, removed) {
				empty = false
			}
		}
	}
	if empty && removeSelf {
		if !c.DryRun {
			if err := os.Remove(dir); err != nil {
				return false
			}
		}
		*removed++
	}
	return empty
}

// Removed is flatpak_dir_cleanup_removed(), which flatpak itself runs
// after most transactions: deployments that were replaced or uninstalled
// while in use are moved to <installation>/.removed and deleted once no
// running instance has them locked.
//
// The in-use test is deploy_dir_is_locked() (1.18; dir_is_locked() in
// 1.16): open files/.ref read-write and ask with F_GETLK whether anyone
// holds a lock that a write lock on the whole file would conflict with;
// every running instance holds a read lock on it (bwrap --lock-file). Where
// flatpak treats a .ref it cannot open as "not in use" and deletes the
// directory, rpak keeps it: rpak only deletes what flatpak would delete,
// not necessarily everything.
func (c *Cleaner) Removed(installation string) {
	root := filepath.Join(installation, ".removed")
	entries, err := readDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if !e.IsDir() {
			continue
		}
		if inUse, err := deployInUse(path); err != nil || inUse {
			if err != nil {
				c.verbose("keeping %s: %v", path, err)
			} else {
				c.verbose("keeping %s: in use by a running instance", path)
			}
			continue
		}
		size := diskUsage(path)
		if !c.DryRun {
			if err := os.RemoveAll(path); err != nil {
				c.fail("%v", err)
				continue
			}
		}
		c.report(size, path)
	}
}

// deployInUse reports whether a running instance holds a lock on the
// deployment's files/.ref. Anything unusual (symlinks, no .ref, no write
// access) is an error, which callers take as "keep".
func deployInUse(deploy string) (bool, error) {
	files := filepath.Join(deploy, "files")
	info, err := os.Lstat(files)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("files is not a directory")
	}
	ref := filepath.Join(files, ".ref")
	fd, err := unix.Open(ref, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		return false, fmt.Errorf("files/.ref: %w", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, fmt.Errorf("files/.ref is not a regular file")
	}
	fl := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(uintptr(fd), unix.F_GETLK, &fl); err != nil {
		return false, err
	}
	return fl.Type != unix.F_UNLCK, nil
}

// diskUsage is g_file_measure_disk_usage (G_FILE_MEASURE_NONE): the
// allocated size of path and everything below it, symlinks not followed.
// Like GLib it counts a file with several hard links once per link.
func diskUsage(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		var st unix.Stat_t
		if unix.Lstat(p, &st) == nil {
			total += st.Blocks * 512
		}
		return nil
	})
	return total
}

// --- data of apps that are no longer installed

// AppData is what an app that is no longer installed left for the user.
type AppData struct {
	ID string
	// Data reports whether ~/.var/app/ID exists; Size is its disk usage.
	Data bool
	Size int64
	// Override reports whether <user base dir>/overrides/ID exists.
	Override bool
}

// UninstalledAppData is the detection of clean_app_data() in the fork's
// flatpak clean: the names in ~/.var/app and <user base dir>/overrides
// that are D-Bus names and not the ID of an app installed in any of the
// installations, sorted. An app counts as installed where
// app/ID/current exists, as flatpak_find_current_ref() checks it in the
// user and every system installation. Listing them changes nothing; see
// RemoveAppData for --delete-data.
func UninstalledAppData(home, userBase string, installations []string) []AppData {
	varApp := filepath.Join(home, ".var", "app")
	overrides := filepath.Join(userBase, "overrides")
	ids := make(map[string]bool)
	for _, dir := range []string{varApp, overrides} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			ids[e.Name()] = true
		}
	}
	var out []AppData
	for id := range ids {
		if !dbusName(id) || installed(id, installations) {
			continue
		}
		a := AppData{ID: id}
		// g_file_query_exists() follows symlinks.
		if _, err := os.Stat(filepath.Join(varApp, id)); err == nil {
			a.Data = true
			a.Size = diskUsage(filepath.Join(varApp, id))
		}
		if _, err := os.Stat(filepath.Join(overrides, id)); err == nil {
			a.Override = true
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RemoveAppData deletes what UninstalledAppData found for one app, as
// clean_app_data() of the fork does with --delete-data: ~/.var/app/ID like
// flatpak_rm_rf() (if it is a symlink, only the link) and the override
// like flatpak_remove_override_keyfile() (unlinked). Before that it checks
// again that ID is a D-Bus name, so a single path component, and that the
// app has not been installed since. Resetting its permissions is up to the
// caller.
func RemoveAppData(home, userBase string, installations []string, a AppData) error {
	if !dbusName(a.ID) {
		return fmt.Errorf("%q is not an app ID", a.ID)
	}
	if installed(a.ID, installations) {
		return fmt.Errorf("%s is installed now, keeping its data", a.ID)
	}
	if a.Data {
		path, err := childPath(filepath.Join(home, ".var", "app"), a.ID)
		if err != nil {
			return err
		}
		// os.RemoveAll unlinks path first and only descends into it
		// through openat(O_NOFOLLOW), as glnx_shutil_rm_rf_at() does:
		// no symlink, at the top or below, is followed.
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	if a.Override {
		path, err := childPath(filepath.Join(userBase, "overrides"), a.ID)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// childPath is dir/name, for a name that cannot lead out of dir.
func childPath(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	if name == "." || name == ".." || strings.ContainsRune(name, '/') || filepath.Dir(path) != filepath.Clean(dir) {
		return "", fmt.Errorf("%q is not a name in %s", name, dir)
	}
	return path, nil
}

// installed is flatpak_find_current_ref() != NULL. flatpak also requires
// the target of the current symlink to make a valid ref; rpak takes any
// app/ID/current as installed, so that it never calls the data of an
// installed app left over.
func installed(id string, installations []string) bool {
	for _, inst := range installations {
		if _, err := os.Lstat(filepath.Join(inst, "app", id, "current")); err == nil {
			return true
		}
	}
	return false
}

// dbusName is g_dbus_is_name() of GLib's gio/gdbusutils.c: a unique
// (":1.42") or well-known ("org.example.App") bus name.
func dbusName(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	if name[0] == ':' {
		return validName(name[1:], true)
	}
	return busNameChar(name[0], false) && validName(name[1:], false)
}

// validName is is_valid_name (s, len, allow_initial_digit, TRUE).
func validName(s string, initialDigit bool) bool {
	if s == "" {
		return false
	}
	hasDot := false
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			i++
			if i == len(s) || !busNameChar(s[i], initialDigit) {
				return false
			}
			hasDot = true
		} else if !busNameChar(s[i], true) {
			return false
		}
	}
	return hasDot
}

// busNameChar is is_valid_bus_name_character (c, TRUE), or with digit
// false is_valid_initial_bus_name_character (c, FALSE, TRUE).
func busNameChar(c byte, digit bool) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '-' || digit && c >= '0' && c <= '9'
}
