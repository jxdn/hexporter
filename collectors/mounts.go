package collectors

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// Host mount table helpers shared by the disk and NFS collectors.
//
// The mount table is read from HOST_PROC/1/mounts: PID 1 sees the host's
// mount namespace. HOST_PROC/mounts would be the container's own view.

// hostMount is one real (non-virtual) filesystem mounted on the host.
type hostMount struct {
	Device     string
	Mountpoint string
	FSType     string
	Class      string // local | nfs | parallel | smb | fuse | other
	Server     string // "host" from "host:/export", "-" otherwise
	Export     string // "/export", or the device
	NFSVers    string // from vers= option, "-" if none
	Dev        string // major:minor from mountinfo, "-" if unknown
	ReadOnly   bool
}

var virtualFSTypes = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "cgroup": true, "cgroup2": true,
	"proc": true, "sysfs": true, "none": true, "devpts": true, "securityfs": true,
	"pstore": true, "efivarfs": true, "autofs": true, "hugetlbfs": true, "mqueue": true,
	"debugfs": true, "tracefs": true, "fusectl": true, "configfs": true, "ramfs": true,
	"squashfs": true, "bpf": true, "rpc_pipefs": true, "binfmt_misc": true, "nsfs": true,
	"selinuxfs": true, "nfsd": true,
}

var parallelFSTypes = map[string]bool{
	"dpc": true, "lustre": true, "gpfs": true, "beegfs": true, "wekafs": true,
	"ceph": true, "cephfs": true, "panfs": true, "pvfs2": true, "orangefs": true,
	"glusterfs": true, "fuse.glusterfs": true,
}

var localFSTypes = map[string]bool{
	"xfs": true, "ext2": true, "ext3": true, "ext4": true, "btrfs": true,
	"zfs": true, "vfat": true, "f2fs": true,
}

// fsClass classifies a filesystem type. "skip" = virtual.
// FS_PARALLEL_TYPES adds extra parallel types (comma list).
func fsClass(fstype string) string {
	for _, t := range envList("FS_PARALLEL_TYPES") {
		if t == fstype {
			return "parallel"
		}
	}
	switch {
	case virtualFSTypes[fstype]:
		return "skip"
	case fstype == "nfs" || fstype == "nfs4":
		return "nfs"
	case parallelFSTypes[fstype]:
		return "parallel"
	case fstype == "cifs" || fstype == "smb3" || fstype == "smbfs":
		return "smb"
	case fstype == "fuse" || strings.HasPrefix(fstype, "fuse."):
		return "fuse"
	case localFSTypes[fstype]:
		return "local"
	default:
		return "other"
	}
}

// openMountFile opens a per-process mount file (mounts, mountinfo,
// mountstats) of the host's mount namespace. PID 1's copy is the host view
// inside a container; a non-root process on bare metal may not read PID 1's
// mountstats, so it falls back to its own copy, which on bare metal is the
// same namespace.
func openMountFile(name string) (*os.File, error) {
	f, err := os.Open(procPath("1/" + name))
	if err == nil {
		return f, nil
	}
	if f2, err2 := os.Open(procPath("self/" + name)); err2 == nil {
		return f2, nil
	}
	return nil, err
}

// readHostMounts returns the host's real mounts, one per mountpoint.
func readHostMounts() ([]hostMount, error) {
	devs := map[string]string{}
	if f, err := openMountFile("mountinfo"); err == nil {
		devs = parseMountInfoDevs(f)
		f.Close()
	}
	f, err := openMountFile("mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMounts(f, devs), nil
}

// parseMounts parses /proc/mounts format. Virtual filesystems are skipped;
// if two real mounts share a mountpoint, the later (visible) one wins.
func parseMounts(r io.Reader, devs map[string]string) []hostMount {
	var order []string
	byMP := map[string]hostMount{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		src, mp, fstype, opts := f[0], unescapeMount(f[1]), f[2], f[3]
		cls := fsClass(fstype)
		if cls == "skip" {
			continue
		}
		m := hostMount{Device: src, Mountpoint: mp, FSType: fstype, Class: cls,
			Server: "-", Export: src, NFSVers: "-", Dev: "-"}
		if i := strings.Index(src, ":/"); i > 0 {
			m.Server, m.Export = src[:i], src[i+1:]
		}
		for _, o := range strings.Split(opts, ",") {
			switch {
			case o == "ro":
				m.ReadOnly = true
			case strings.HasPrefix(o, "vers="):
				m.NFSVers = strings.TrimPrefix(o, "vers=")
			}
		}
		if d, ok := devs[mp]; ok {
			m.Dev = d
		}
		if _, seen := byMP[mp]; !seen {
			order = append(order, mp)
		}
		byMP[mp] = m
	}
	out := make([]hostMount, 0, len(order))
	for _, mp := range order {
		out = append(out, byMP[mp])
	}
	return out
}

// parseMountInfoDevs maps mountpoint -> major:minor from /proc/<pid>/mountinfo.
// NFS mounts with the same device share one superblock (and one set of
// mountstats counters).
func parseMountInfoDevs(r io.Reader) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		m[unescapeMount(f[4])] = f[2]
	}
	return m
}

// unescapeMount decodes octal escapes used in mount tables (\040 = space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
