package clean

import (
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// dirent is a directory entry as readdir() returns it, with d_ino, which
// os.ReadDir does not expose (and which can differ from st_ino).
type dirent struct {
	dir  string
	name string
	ino  uint64
	typ  uint8 // DT_*
}

// readDirents reads dir with getdents64, in directory order.
func readDirents(dir string) ([]dirent, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(fd)
	var out []dirent
	buf := make([]byte, 32*1024)
	for {
		n, err := unix.ReadDirent(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, &fs.PathError{Op: "readdirent", Path: dir, Err: err}
		}
		if n <= 0 {
			return out, nil
		}
		// struct linux_dirent64: d_ino u64, d_off s64, d_reclen u16,
		// d_type u8, d_name NUL-terminated.
		for b := buf[:n]; len(b) >= 19; {
			reclen := int(binary.NativeEndian.Uint16(b[16:18]))
			if reclen < 19 || reclen > len(b) {
				break
			}
			name := b[19:reclen]
			for i, c := range name {
				if c == 0 {
					name = name[:i]
					break
				}
			}
			if s := string(name); s != "." && s != ".." {
				out = append(out, dirent{dir: dir, name: s, ino: binary.NativeEndian.Uint64(b[0:8]), typ: b[18]})
			}
			b = b[reclen:]
		}
	}
}

func (d *dirent) Name() string { return d.name }

func (d *dirent) IsDir() bool { return d.Type().IsDir() }

// Type is the type from d_type, or from lstat() where the file system does
// not fill it in, as GLib does then.
func (d *dirent) Type() fs.FileMode {
	switch d.typ {
	case unix.DT_DIR:
		return fs.ModeDir
	case unix.DT_REG:
		return 0
	case unix.DT_LNK:
		return fs.ModeSymlink
	case unix.DT_UNKNOWN:
		if info, err := d.Info(); err == nil {
			return info.Mode().Type()
		}
		return fs.ModeIrregular
	}
	return fs.ModeIrregular
}

func (d *dirent) Info() (fs.FileInfo, error) {
	return os.Lstat(filepath.Join(d.dir, d.name))
}
