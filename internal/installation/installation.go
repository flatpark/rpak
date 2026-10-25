// Package installation finds flatpak installations the way flatpak does
// (common/flatpak-dir.c of flatpak 1.16 and 1.18, unchanged between them).
package installation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// DefaultID is the id of the default system installation
// (SYSTEM_DIR_DEFAULT_ID).
const DefaultID = "default"

// Installation is a flatpak installation directory.
type Installation struct {
	// ID is "user" for the user installation, DefaultID for the default
	// system installation, and the id from installations.d otherwise.
	ID   string
	Path string
	User bool
	// Priority orders system installations, highest first.
	Priority int64
}

// Title is the heading flatpak clean prints for the installation
// (get_installation_title() in the fork's app/flatpak-builtins-clean.c).
func (i Installation) Title() string {
	switch {
	case i.User:
		return "User installation"
	case i.ID == DefaultID:
		return "System installation (default)"
	}
	// flatpak_dir_get_name()
	return fmt.Sprintf("Installation system (%s)", i.ID)
}

// Option is the command-line option that selects the installation.
func (i Installation) Option() string {
	switch {
	case i.User:
		return "--user"
	case i.ID == DefaultID:
		return "--system"
	}
	return "--installation=" + i.ID
}

// Env holds what the lookup depends on; nil fields mean the process's
// own environment and home directory.
type Env struct {
	LookupEnv func(string) (string, bool)
	HomeDir   func() (string, error)
}

func (e Env) lookupEnv(name string) (string, bool) {
	if e.LookupEnv != nil {
		return e.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

func (e Env) getenv(name string) string {
	v, _ := e.lookupEnv(name)
	return v
}

// User is the user installation: flatpak_get_user_base_dir_location().
func (e Env) User() (Installation, error) {
	home := e.HomeDir
	if home == nil {
		home = os.UserHomeDir
	}
	path, err := prefetch.UserBaseDir(e.getenv, home)
	if err != nil {
		return Installation{}, err
	}
	return Installation{ID: "user", Path: path, User: true}, nil
}

// SystemDefault is the default system installation:
// flatpak_get_system_default_base_dir_location() uses $FLATPAK_SYSTEM_DIR if
// set and not empty, else FLATPAK_SYSTEMDIR, which is
// ${localstatedir}/lib/flatpak, /var/lib/flatpak in distribution builds.
func (e Env) SystemDefault() Installation {
	path := e.getenv("FLATPAK_SYSTEM_DIR")
	if path == "" {
		path = "/var/lib/flatpak"
	}
	return Installation{ID: DefaultID, Path: path}
}

// configDir is get_config_dir_location(): $FLATPAK_CONFIG_DIR if set (even
// if empty), else FLATPAK_CONFIGDIR, /etc/flatpak.
func (e Env) configDir() string {
	if dir, ok := e.lookupEnv("FLATPAK_CONFIG_DIR"); ok {
		return dir
	}
	return "/etc/flatpak"
}

// Configured returns the system installations in the configuration, highest
// priority first, as system_locations_from_configuration() and
// get_system_locations() read them from <config dir>/installations.d/*.conf:
// groups named [Installation "ID"] with a Path key. Problems in a file are
// returned as warnings; flatpak would refuse to run commands that look at
// all installations, rpak skips the file.
func (e Env) Configured() ([]Installation, []string) {
	dir := filepath.Join(e.configDir(), "installations.d")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{err.Error()}
	}
	var out []Installation
	var warnings []string
	seen := map[string]bool{}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		// G_FILE_QUERY_INFO_NONE: symlinks are followed.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		text, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		found, problems := parseConfig(string(text), seen)
		for _, p := range problems {
			warnings = append(warnings, fmt.Sprintf("%s: %s", path, p))
		}
		out = append(out, found...)
	}
	// system_locations_compare_func; the default installation (priority 0)
	// is not part of this list.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out, warnings
}

// parseConfig is append_locations_from_config_file().
func parseConfig(text string, seen map[string]bool) ([]Installation, []string) {
	var out []Installation
	var problems []string
	for _, g := range metadata.KeyFile(text) {
		if !strings.HasPrefix(g.Name, `Installation "`) {
			continue
		}
		id := g.Name[len(`Installation "`):]
		if !strings.HasSuffix(id, `"`) {
			problems = append(problems, fmt.Sprintf("installation without closing quote (%s), ignored", g.Name))
			continue
		}
		id = id[:len(id)-1]
		if !goodID(id) {
			problems = append(problems, fmt.Sprintf("bad installation ID %q, ignored", id))
			continue
		}
		if seen[id] {
			problems = append(problems, fmt.Sprintf("duplicate installation ID %q, ignored", id))
			continue
		}
		path, ok := g.Values["Path"]
		if !ok {
			problems = append(problems, fmt.Sprintf("no Path for installation %q", id))
			continue
		}
		seen[id] = true
		// g_ascii_strtoll: leading number, 0 if none.
		priority, _ := strconv.ParseInt(leadingInt(g.Values["Priority"]), 10, 64)
		out = append(out, Installation{ID: id, Path: path, Priority: priority})
	}
	return out, problems
}

// goodID is is_good_installation_id().
func goodID(id string) bool {
	switch id {
	case "", "user", DefaultID, "system":
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] >= 0x80 || id[i] == ' ' || id[i] == '/' || id[i] == '\n' {
			return false
		}
	}
	return len(id) <= 80
}

func leadingInt(s string) string {
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return s[:end]
}

// ByID is flatpak_dir_get_system_by_id(): "default" is the default system
// installation, anything else must be configured.
func (e Env) ByID(id string) (Installation, error) {
	if id == DefaultID {
		return e.SystemDefault(), nil
	}
	configured, _ := e.Configured()
	for _, inst := range configured {
		if inst.ID == id {
			return inst, nil
		}
	}
	return Installation{}, fmt.Errorf("could not find installation %s", id)
}

// Select returns the installations that options (--user, --system,
// --installation=ID, as args.Parsed keeps them) stand for, the way
// flatpak_option_context_parse() picks them. With no options it is the
// default system installation and the user installation, plus with all,
// every other configured system installation (FLATPAK_BUILTIN_FLAG_ALL_DIRS,
// which flatpak clean uses).
func (e Env) Select(options []string, all bool) ([]Installation, []string, error) {
	var user, system bool
	var ids []string
	for _, o := range options {
		switch {
		case o == "--user":
			user = true
		case o == "--system":
			system = true
		case strings.HasPrefix(o, "--installation="):
			ids = append(ids, strings.TrimPrefix(o, "--installation="))
		}
	}
	var out []Installation
	var warnings []string
	add := func(inst Installation) {
		for _, have := range out {
			if have.User == inst.User && have.ID == inst.ID {
				return
			}
		}
		out = append(out, inst)
	}
	addUser := func() error {
		inst, err := e.User()
		if err != nil {
			return err
		}
		add(inst)
		return nil
	}
	if system || (!user && len(ids) == 0) {
		add(e.SystemDefault())
	}
	if user || (!system && len(ids) == 0) {
		if err := addUser(); err != nil {
			return nil, nil, err
		}
	}
	for _, id := range ids {
		inst, err := e.ByID(id)
		if err != nil {
			return nil, nil, err
		}
		add(inst)
	}
	if all && !user && !system && len(ids) == 0 {
		configured, w := e.Configured()
		warnings = w
		for _, inst := range configured {
			add(inst)
		}
	}
	return out, warnings, nil
}

// All is every installation flatpak knows of: the user installation and
// the system ones (flatpak_dir_get_system_list()), as
// flatpak_find_current_ref() searches them.
func (e Env) All() []Installation {
	var out []Installation
	if user, err := e.User(); err == nil {
		out = append(out, user)
	}
	out = append(out, e.SystemDefault())
	configured, _ := e.Configured()
	return append(out, configured...)
}

// AuthenticatedRemotes returns the remotes of the installation whose
// configuration (the [remote "NAME"] groups of repo/config) names an
// authenticator, xa.authenticator-name: pulling from them can make
// flatpak ask for a user name and password. A missing repository has
// none.
func (i Installation) AuthenticatedRemotes() ([]string, error) {
	text, err := os.ReadFile(filepath.Join(i.Path, "repo", "config"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range metadata.KeyFile(string(text)) {
		name, ok := strings.CutPrefix(g.Name, `remote "`)
		if !ok || !strings.HasSuffix(name, `"`) {
			continue
		}
		if g.Values["xa.authenticator-name"] != "" {
			out = append(out, name[:len(name)-1])
		}
	}
	return out, nil
}
