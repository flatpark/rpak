package doctor

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type sysctl struct {
	name  string // as sysctl names it
	value string // "" if the file does not exist
}

type systemInfo struct {
	userns []sysctl
	// portals are the xdg-desktop-portal processes of the user, by name.
	portals []string
	// portalFiles are the portal definitions and configuration files.
	portalFiles []portalFile
	nvidia      string // driver version, "" without the NVIDIA driver
	nvidiaErr   error  // reading /proc/driver/nvidia/version
	gpus        []GPU
}

type portalFile struct {
	path    string
	content string // for the configuration files, which are short
}

var usernsSysctls = []string{
	"user/max_user_namespaces",
	"kernel/unprivileged_userns_clone",
	"kernel/apparmor_restrict_unprivileged_userns",
}

func (d *doctor) systemInfo() systemInfo {
	var s systemInfo
	for _, name := range usernsSysctls {
		v, err := os.ReadFile(d.path("/proc/sys/" + name))
		if err == nil {
			s.userns = append(s.userns, sysctl{strings.ReplaceAll(name, "/", "."), strings.TrimSpace(string(v))})
		}
	}
	s.portals = PortalProcesses(d.path("/proc"), d.cfg.Uid)
	s.portalFiles = d.portalFiles()
	if v, err := os.ReadFile(d.path("/proc/driver/nvidia/version")); err == nil {
		s.nvidia = NvidiaVersion(string(v))
	} else {
		s.nvidiaErr = err
	}
	s.gpus = GPUs(d.path("/sys/class/drm"))
	return s
}

// GPU is a graphics card, as /sys/class/drm/cardN shows it.
type GPU struct {
	Card   string // "card0"
	Vendor string // PCI ids, "8086", without 0x
	Device string
	Driver string // the kernel driver, "" if none is bound
	Boot   bool   // the card the firmware set up (boot_vga)
}

// VendorName is the name of the vendors that matter for flatpak's GL
// extensions; others are shown by their id.
func (g GPU) VendorName() string {
	switch g.Vendor {
	case "8086":
		return "Intel"
	case "10de":
		return "NVIDIA"
	case "1002":
		return "AMD"
	}
	return g.Vendor
}

// GPUs lists the cards in drm (/sys/class/drm) by number: the cardN
// entries, not their connectors (card0-HDMI-A-1). It reads only sysfs,
// neither pci.ids nor lspci.
func GPUs(drm string) []GPU {
	entries, err := os.ReadDir(drm)
	if err != nil {
		return nil
	}
	var out []GPU
	num := map[string]int{}
	for _, e := range entries {
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "card"))
		if !strings.HasPrefix(e.Name(), "card") || err != nil || n < 0 {
			continue
		}
		dev := filepath.Join(drm, e.Name(), "device")
		read := func(name string) string {
			b, _ := os.ReadFile(filepath.Join(dev, name))
			return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"))
		}
		g := GPU{Card: e.Name(), Vendor: read("vendor"), Device: read("device"), Boot: read("boot_vga") == "1"}
		if link, err := os.Readlink(filepath.Join(dev, "driver")); err == nil {
			g.Driver = filepath.Base(link)
		}
		num[g.Card] = n
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return num[out[i].Card] < num[out[j].Card] })
	return out
}

// PortalProcesses returns the names of the xdg-desktop-portal processes
// (the frontend and its backends) that uid runs, sorted. comm is cut to 15
// characters ("xdg-desktop-por"), so for those the name is taken from the
// command line.
func PortalProcesses(proc string, uid int) []string {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(proc, e.Name())
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || !strings.HasPrefix(string(comm), "xdg-desktop-por") {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		argv0, _, _ := bytes.Cut(cmdline, []byte{0})
		name := filepath.Base(string(argv0))
		if strings.HasPrefix(name, "xdg-desktop-portal") {
			seen[name] = true
		}
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// portalFiles lists what xdg-desktop-portal picks its backends from: the
// .portal files of the installed backends and the *portals.conf files that
// say which to use, system-wide and the user's.
func (d *doctor) portalFiles() []portalFile {
	var out []portalFile
	portals, _ := filepath.Glob(d.path("/usr/share/xdg-desktop-portal/portals/*.portal"))
	for _, p := range portals {
		out = append(out, portalFile{path: strings.TrimPrefix(p, d.cfg.Root)})
	}
	config := d.getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(d.cfg.Home, ".config")
	}
	dirs := []string{d.path("/usr/share/xdg-desktop-portal"), d.path("/etc/xdg/xdg-desktop-portal"), filepath.Join(config, "xdg-desktop-portal")}
	for _, dir := range dirs {
		confs, _ := filepath.Glob(filepath.Join(dir, "*portals.conf"))
		for _, c := range confs {
			text, _ := os.ReadFile(c)
			if len(text) > 4096 {
				text = text[:4096]
			}
			path := c
			if d.cfg.Root != "" && strings.HasPrefix(c, d.cfg.Root) {
				path = strings.TrimPrefix(c, d.cfg.Root)
			}
			out = append(out, portalFile{path: path, content: strings.TrimRight(string(text), "\n")})
		}
	}
	return out
}

// NvidiaVersion is the driver version in /proc/driver/nvidia/version:
// "NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.95.05  Tue ...".
func NvidiaVersion(text string) string {
	first, _, _ := strings.Cut(text, "\n")
	for _, f := range strings.Fields(first) {
		parts := strings.Split(f, ".")
		if len(parts) < 2 {
			continue
		}
		ok := true
		for _, p := range parts {
			if _, err := strconv.Atoi(p); err != nil {
				ok = false
			}
		}
		if ok {
			return f
		}
	}
	return ""
}

// gpuItems are the graphics cards and the NVIDIA driver's version.
func (d *doctor) gpuItems(s *builder, sys systemInfo) {
	if len(sys.gpus) == 0 {
		s.add("GPU: none found in /sys/class/drm")
	}
	var bound []string
	nvidiaBound := false
	for _, g := range sys.gpus {
		driver := "no driver"
		if g.Driver != "" {
			driver = "driver " + g.Driver
			bound = append(bound, g.VendorName())
		}
		nvidiaBound = nvidiaBound || g.Driver == "nvidia"
		boot := ""
		if g.Boot {
			boot = " (boot)"
		}
		name := g.VendorName() + " "
		if name == g.Vendor+" " {
			name = ""
		}
		s.add("GPU %s: %s%s:%s, %s%s", g.Card, name, g.Vendor, g.Device, driver, boot)
	}
	if len(bound) > 1 {
		s.add("hybrid graphics: %s", strings.Join(bound, " + "))
	}

	// The version is said if the file is there, and if it is not, only
	// where a card uses the nvidia driver.
	var pe *fs.PathError
	switch {
	case sys.nvidia != "":
		s.add("NVIDIA driver %s", sys.nvidia)
	case sys.nvidiaErr == nil:
		s.add("NVIDIA driver version: none in /proc/driver/nvidia/version")
	case !nvidiaBound:
	case errors.Is(sys.nvidiaErr, fs.ErrNotExist):
		s.add("NVIDIA driver version: /proc/driver/nvidia/version does not exist")
	case errors.As(sys.nvidiaErr, &pe):
		s.add("NVIDIA driver version: /proc/driver/nvidia/version: %v", pe.Err)
	default:
		s.add("NVIDIA driver version: %v", sys.nvidiaErr)
	}
}

// glExtension is an installed GL extension.
type glExtension struct {
	id, ref, where string
}

// GLKind sorts a GL extension id: "default", "nvidia" or "other"; "" for
// what is not one. GL32 are the 32-bit ones.
func GLKind(id string) string {
	rest, ok := strings.CutPrefix(id, "org.freedesktop.Platform.GL.")
	if !ok {
		rest, ok = strings.CutPrefix(id, "org.freedesktop.Platform.GL32.")
	}
	switch {
	case !ok || rest == "":
		return ""
	case rest == "default":
		return "default"
	case strings.HasPrefix(rest, "nvidia-"):
		return "nvidia"
	}
	return "other"
}

func (d *doctor) systemSection(sys systemInfo, insts []*instInfo) Section {
	s := d.section("System")
	r := d.r
	for _, c := range sys.userns {
		s.add("%s=%s", c.name, c.value)
	}

	var backends []string
	for _, p := range sys.portals {
		if p != "xdg-desktop-portal" {
			backends = append(backends, strings.TrimPrefix(p, "xdg-desktop-portal-"))
		}
	}
	running := "none"
	if len(backends) > 0 {
		running = strings.Join(backends, ", ")
	}
	if contains(sys.portals, "xdg-desktop-portal") {
		s.add("xdg-desktop-portal: running; backends running: %s", running)
	} else {
		s.add("xdg-desktop-portal: not running; backends running: %s", running)
	}
	for _, f := range sys.portalFiles {
		it := s.add("%s", f.path)
		if f.content != "" {
			it.block(r, f.content)
		}
	}

	d.gpuItems(s, sys)

	var exts []glExtension
	for _, in := range insts {
		for _, e := range in.runtimes.refs {
			id, _, _ := strings.Cut(e.ref, "/")
			if GLKind(id) != "" {
				exts = append(exts, glExtension{id: id, ref: e.ref, where: in.inst.Title()})
			}
		}
	}
	if len(exts) == 0 {
		s.add("GL extensions: none installed")
	}
	for _, kind := range []string{"default", "nvidia", "other"} {
		for _, e := range exts {
			if GLKind(e.id) == kind {
				s.add("GL extension (%s): %s  %s", kind, e.ref, e.where)
			}
		}
	}
	return s.done()
}
