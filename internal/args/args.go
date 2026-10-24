// Package args interprets `flatpak update` / `flatpak install` command lines
// the way flatpak 1.18's GOption parsing would, without changing them: rpak
// passes the original argv to flatpak byte for byte and only uses this to
// work out what flatpak is about to pull.
package args

import "strings"

// Installation options as rpak passes them to flatpak query commands.
const (
	User   = "--user"
	System = "--system"
)

// Parsed is the part of a command line that matters for prefetching.
type Parsed struct {
	Command     string
	Positionals []string
	// Installations holds "--user", "--system" and "--installation=NAME"
	// options in command-line order, without duplicates.
	Installations []string
	// ExplicitInstallation reports whether the user chose installations.
	// If not, Installations holds the defaults: user and system.
	ExplicitInstallation bool
	Arch                 string
	// SkipReason is non-empty when flatpak will not pull from a configured
	// remote (local cache, bundles, sideloads, ...), so there is nothing
	// rpak can prefetch.
	SkipReason string
	// NoDeploy reports --no-deploy: flatpak only pulls, and a later
	// "flatpak install/update --no-pull" deploys from the repository, so
	// the extra-data in it must stay.
	NoDeploy bool
	// Unknown lists options this parser does not know; they are treated as
	// flags without a value.
	Unknown []string
}

// Long options that take a value, from the GOptionEntry tables of flatpak
// 1.18 (app/flatpak-builtins-{update,install}.c and user_entries in
// app/flatpak-main.c). GLib accepts both --opt=value and --opt value.
var valueOptions = map[string]map[string]bool{
	"update": {
		"--arch": true, "--commit": true, "--subpath": true,
		"--sideload-repo": true, "--installation": true,
	},
	"install": {
		"--arch": true, "--gpg-file": true, "--subpath": true,
		"--sideload-repo": true, "--installation": true,
	},
}

// Long options without a value, same sources plus global_entries.
var flagOptions = map[string]map[string]bool{
	"update": {
		"--force-remove": true, "--no-pull": true, "--no-deploy": true,
		"--no-related": true, "--no-deps": true, "--no-static-deltas": true,
		"--runtime": true, "--app": true, "--appstream": true,
		"--assumeyes": true, "--noninteractive": true,
	},
	"install": {
		"--no-pull": true, "--no-deploy": true, "--no-related": true,
		"--no-deps": true, "--no-auto-pin": true, "--no-static-deltas": true,
		"--runtime": true, "--app": true, "--include-sdk": true,
		"--include-debug": true, "--bundle": true, "--from": true,
		"--image": true, "--assumeyes": true, "--reinstall": true,
		"--noninteractive": true, "--or-update": true,
	},
}

var globalFlags = map[string]bool{
	"--user": true, "--system": true, "--verbose": true,
	"--ostree-verbose": true, "--help": true,
}

// Short options. None of update/install's short options takes a value:
// -y (--assumeyes), -u (--user), -v (--verbose, repeatable), -? (--help).
// -h is not a flatpak option, but flatpak only prints an error for it.
var shortFlags = map[rune]bool{'y': true, 'u': true, 'v': true, '?': true, 'h': true}

// Options after which flatpak does not pull refs from a configured remote,
// or pulls something other than what remote-info would describe.
var skipOptions = map[string]bool{
	"--no-pull": true, "--commit": true, "--bundle": true, "--from": true,
	"--image": true, "--sideload-repo": true, "--appstream": true,
	"--help": true, // flatpak only prints the help
}

// Parse interprets argv (the arguments after "update"/"install").
func Parse(command string, argv []string) Parsed {
	p := Parsed{Command: command}
	values, flags := valueOptions[command], flagOptions[command]
	seenUnknown := make(map[string]bool)
	unknown := func(name string) {
		if !seenUnknown[name] {
			seenUnknown[name] = true
			p.Unknown = append(p.Unknown, name)
		}
	}
	skip := func(reason string) {
		if p.SkipReason == "" {
			p.SkipReason = reason
		}
	}

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			// GLib stops option parsing at "--".
			p.Positionals = append(p.Positionals, argv[i+1:]...)
			i = len(argv)
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg, "=")
			if values[name] && !hasValue && i+1 < len(argv) {
				i++
				value, hasValue = argv[i], true
			}
			if skipOptions[name] {
				skip(name)
			}
			switch name {
			case "--user", "--system":
				p.addInstallation(name)
			case "--installation":
				if hasValue {
					p.addInstallation("--installation=" + value)
				}
			case "--arch":
				p.Arch = value
			case "--no-deploy":
				p.NoDeploy = true
			}
			if !values[name] && !flags[name] && !globalFlags[name] {
				unknown(name)
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			// GLib allows grouped short flags such as -uy or -vv.
			for _, c := range arg[1:] {
				if c == 'u' {
					p.addInstallation(User)
				}
				if c == '?' || c == 'h' {
					skip("--help")
				}
				if !shortFlags[c] {
					unknown("-" + string(c))
				}
			}
		default:
			// GLib does not stop at the first positional argument.
			p.Positionals = append(p.Positionals, arg)
		}
	}

	if !p.ExplicitInstallation {
		// update covers the user and the default system installation; for
		// install rpak looks at both, see the flatpak package.
		p.Installations = []string{User, System}
	}
	if command == "install" {
		for _, arg := range p.Positionals {
			if LocalLocation(arg) {
				skip("install from " + arg)
				break
			}
		}
	}
	return p
}

func (p *Parsed) addInstallation(option string) {
	p.ExplicitInstallation = true
	for _, existing := range p.Installations {
		if existing == option {
			return
		}
	}
	p.Installations = append(p.Installations, option)
}

// SplitInstallRemote splits install's positional arguments into REMOTE and
// REFs, following flatpak_builtin_install(): with a single argument it is
// always a REF; otherwise the first one is the REMOTE if such a remote is
// configured (isRemote), and a REF otherwise.
func SplitInstallRemote(positionals []string, isRemote func(string) bool) (remote string, refs []string) {
	if len(positionals) >= 2 && isRemote(positionals[0]) {
		return positionals[0], positionals[1:]
	}
	return "", positionals
}

// LocalLocation reports whether an install argument is a bundle, a
// .flatpakref, an OCI image or a repository location rather than a ref:
// flatpak_builtin_install() switches to install_bundle/install_from/
// install_image or treats it as a remote URL in those cases.
func LocalLocation(arg string) bool {
	for _, prefix := range []string{"/", "./", "../", "http:", "https:", "file:", "oci:", "oci-archive:", "docker:"} {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return strings.HasSuffix(arg, ".flatpakref") || strings.HasSuffix(arg, ".flatpak")
}
