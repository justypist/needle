// Package collect reads host metrics from /proc, /sys and statfs. It only
// ever reads: nothing here opens a file for writing or issues a syscall that
// changes system state, so it is safe to run as an unprivileged user.
package collect

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"needle/internal/proto"
)

const (
	maxInterfaces  = 16
	maxFilesystems = 12
	maxZones       = 8
)

// virtualNetPrefixes are interface names that exist because something is
// bridged on top of a real NIC. Their counters duplicate traffic the physical
// device already reports, and on a container host they crowd out the NICs a
// human actually wants to see.
var virtualNetPrefixes = []string{"veth", "br-", "docker", "virbr", "vnet", "tap", "vmnet", "lxc"}

// fsTypes are the filesystem types worth showing a usage bar for. Anything
// not listed (cgroup, debugfs, pstore, ...) is noise. Overlay mounts are
// deliberately absent: on a container host /proc/mounts carries one per
// running container and they would drown the real filesystems.
var fsTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "vfat": true, "exfat": true, "ntfs": true,
	"ntfs3": true, "fuseblk": true, "squashfs": true, "ubifs": true,
	"jffs2": true, "tmpfs": true,
}

// fsSkipPrefixes are mount points whose usage is not interesting or is
// reported elsewhere. /var/lib/docker catches the per-container overlay
// mount points that also happen to be tmpfs.
var fsSkipPrefixes = []string{
	"/proc", "/sys", "/dev", "/run/credentials", "/run/user",
	"/var/lib/docker", "/var/lib/containers", "/snap",
}

type cpuTimes struct{ total, idle uint64 }

type netCounters struct{ rx, tx uint64 }

type diskCounters struct{ read, write uint64 }

// snapshot is the raw counter state at one instant. Rates are the delta
// between two snapshots.
type snapshot struct {
	at   time.Time
	cpu  cpuTimes
	core []cpuTimes
	net  map[string]netCounters
	disk map[string]diskCounters
}

// Collector holds the previous counter snapshot so it can turn monotonic
// kernel counters into per-second rates.
type Collector struct {
	proc string
	sys  string
	host proto.Host
	prev *snapshot
}

// New resolves the machine identity once. Every field is derived from files
// that every Linux user can read.
func New() *Collector {
	c := &Collector{proc: "/proc", sys: "/sys"}
	c.host = proto.Host{
		ID:       machineID(),
		Name:     hostname(),
		OS:       osPrettyName(),
		Kernel:   kernelRelease(),
		Arch:     runtime.GOARCH,
		CPUModel: cpuModel(),
		Cores:    runtime.NumCPU(),
		Agent:    "needle-client",
	}
	return c
}

// Host returns the identity resolved at construction time.
func (c *Collector) Host() proto.Host { return c.host }

// Prime reads the counters once and discards the result, so that the first
// real Collect has a baseline to diff against. Call it before the loop and
// wait a second or two.
func (c *Collector) Prime() error {
	s, err := c.readCounters(time.Now())
	if err != nil {
		return err
	}
	c.prev = s
	return nil
}

// Collect reads every source and returns one report ready to be posted.
// Optional sources (thermal zones) are silently omitted when absent.
func (c *Collector) Collect() proto.Report {
	at := time.Now()
	cur, err := c.readCounters(at)
	if err != nil && c.prev == nil {
		// Without any baseline we cannot produce rates; report gauges only.
		cur = &snapshot{at: at, net: map[string]netCounters{}, disk: map[string]diskCounters{}}
	}
	prev := c.prev
	if cur != nil {
		c.prev = cur
	}

	elapsed := 1.0
	if prev != nil && cur != nil {
		if d := cur.at.Sub(prev.at).Seconds(); d > 0 {
			elapsed = d
		}
	}

	sample := proto.Sample{TS: at.UnixMilli()}
	var series []proto.Series

	if cur != nil {
		fillCPU(&sample, prev, cur)
		fillMemory(&sample, c.proc)
		fillLoad(&sample, c.proc)
		series = append(series, cpuSeries(prev, cur)...)
		series = append(series, fillNet(&sample, prev, cur, elapsed)...)
		series = append(series, fillDisk(&sample, prev, cur, elapsed)...)
		series = append(series, fillFilesystems(&sample)...)
		series = append(series, fillThermal(&sample, c.sys)...)
	}

	return proto.Report{
		Host:     c.host,
		Interval: 0,
		Sample:   sample,
		Series:   series,
	}
}

// busyPct is the share of time spent outside idle/iowait between two
// counter readings, in percent.
func busyPct(a, b cpuTimes) float64 {
	dt := float64(b.total - a.total)
	if dt <= 0 {
		return 0
	}
	return clampPct((dt - float64(b.idle-a.idle)) / dt * 100)
}

func fillCPU(s *proto.Sample, prev, cur *snapshot) {
	if prev != nil {
		s.CPU = busyPct(prev.cpu, cur.cpu)
	}
}

func cpuSeries(prev, cur *snapshot) []proto.Series {
	if prev == nil || cur == nil || len(cur.core) == 0 || len(cur.core) != len(prev.core) {
		return nil
	}
	out := make([]proto.Series, 0, len(cur.core))
	for i := range cur.core {
		out = append(out, proto.Series{
			Kind:  "cpu",
			Label: "cpu" + strconv.Itoa(i),
			A:     busyPct(prev.core[i], cur.core[i]),
		})
	}
	return out
}

func fillMemory(s *proto.Sample, proc string) {
	m := procFields(proc+"/meminfo", map[string]bool{
		"MemTotal": true, "MemFree": true, "MemAvailable": true,
		"Cached": true, "Buffers": true, "SReclaimable": true,
		"SwapTotal": true, "SwapFree": true, "Shmem": true,
	})
	total := m["MemTotal"]
	// MemAvailable is what the kernel considers reclaimable for new work;
	// falling back to MemFree keeps older kernels (OpenWrt) working.
	avail := m["MemFree"]
	if v, ok := m["MemAvailable"]; ok {
		avail = v
	}
	s.MemTotal = total
	if total > avail {
		s.MemUsed = total - avail
	}
	s.MemCached = m["Cached"] + m["SReclaimable"] - m["Shmem"]
	s.SwapTotal = m["SwapTotal"]
	if m["SwapTotal"] > m["SwapFree"] {
		s.SwapUsed = m["SwapTotal"] - m["SwapFree"]
	}
}

func fillLoad(s *proto.Sample, proc string) {
	if b, err := os.ReadFile(proc + "/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 4 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
			s.Load5, _ = strconv.ParseFloat(f[1], 64)
			s.Load15, _ = strconv.ParseFloat(f[2], 64)
			// /proc/loadavg field 4 is "runnable/total"; the total is what a
			// dashboard reader expects a process count to mean.
			if parts := strings.SplitN(f[3], "/", 2); len(parts) == 2 {
				if n, err := strconv.Atoi(parts[1]); err == nil {
					s.Procs = n
				}
			}
		}
	}
	if b, err := os.ReadFile(proc + "/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil && up > 0 {
				s.Uptime = uint64(up)
			}
		}
	}
}

func fillNet(s *proto.Sample, prev, cur *snapshot, elapsed float64) []proto.Series {
	var rx, tx uint64
	labels := make([]string, 0, len(cur.net))
	for name := range cur.net {
		labels = append(labels, name)
	}
	sort.Strings(labels)
	if len(labels) > maxInterfaces {
		labels = labels[:maxInterfaces]
	}

	out := make([]proto.Series, 0, len(labels))
	for _, name := range labels {
		cn := cur.net[name]
		s.NetRxTot += cn.rx
		s.NetTxTot += cn.tx
		rx += cn.rx
		tx += cn.tx
		var ra, rb float64
		if pn, ok := prev.net[name]; ok {
			ra = rate(cn.rx, pn.rx, elapsed)
			rb = rate(cn.tx, pn.tx, elapsed)
		}
		out = append(out, proto.Series{Kind: "net", Label: name, A: ra, B: rb})
	}
	if prev != nil {
		var prx, ptx uint64
		for _, name := range labels {
			if pn, ok := prev.net[name]; ok {
				prx += pn.rx
				ptx += pn.tx
			}
		}
		s.NetRx = rate(rx, prx, elapsed)
		s.NetTx = rate(tx, ptx, elapsed)
	}
	return out
}

func fillDisk(s *proto.Sample, prev, cur *snapshot, elapsed float64) []proto.Series {
	labels := make([]string, 0, len(cur.disk))
	for name := range cur.disk {
		labels = append(labels, name)
	}
	sort.Strings(labels)
	if len(labels) > maxInterfaces {
		labels = labels[:maxInterfaces]
	}

	var read, write uint64
	out := make([]proto.Series, 0, len(labels))
	for _, name := range labels {
		cd := cur.disk[name]
		read += cd.read
		write += cd.write
		s.DiskRTot += cd.read
		s.DiskWTot += cd.write
		var ra, rb float64
		if pd, ok := prev.disk[name]; ok {
			ra = rate(cd.read, pd.read, elapsed)
			rb = rate(cd.write, pd.write, elapsed)
		}
		out = append(out, proto.Series{Kind: "disk", Label: name, A: ra, B: rb})
	}
	if prev != nil {
		var pr, pw uint64
		for _, name := range labels {
			if pd, ok := prev.disk[name]; ok {
				pr += pd.read
				pw += pd.write
			}
		}
		s.DiskRead = rate(read, pr, elapsed)
		s.DiskWrite = rate(write, pw, elapsed)
	}
	return out
}

func fillFilesystems(s *proto.Sample) []proto.Series {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	type fsMount struct {
		label       string
		used, total uint64
	}
	var mounts []fsMount
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		dev, mount, fstype := unescapeMount(fields[0]), unescapeMount(fields[1]), fields[2]
		if !fsTypes[fstype] || seen[mount] || skipMount(mount) {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		total := st.Blocks * bsize
		if total == 0 {
			continue
		}
		// A tmpfs smaller than this is plumbing (/run/lock and friends), not
		// storage anyone monitors.
		if fstype == "tmpfs" && total < 64<<20 {
			continue
		}
		seen[mount] = true
		label := mount
		if label == "" {
			label = dev
		}
		mounts = append(mounts, fsMount{label: label, used: (st.Blocks - st.Bfree) * bsize, total: total})
	}

	// Rank by capacity so the cap keeps the filesystems that matter instead
	// of whichever ones sort first alphabetically.
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].total > mounts[j].total })
	if len(mounts) > maxFilesystems {
		mounts = mounts[:maxFilesystems]
	}
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].label < mounts[j].label })

	out := make([]proto.Series, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, proto.Series{Kind: "fs", Label: m.label, A: float64(m.used), B: float64(m.total)})
	}
	return out
}

func fillThermal(s *proto.Sample, sys string) []proto.Series {
	zones, err := filepath.Glob(filepath.Join(sys, "class/thermal/thermal_zone*"))
	if err != nil || len(zones) == 0 {
		return nil
	}
	sort.Strings(zones)
	var out []proto.Series
	for _, z := range zones {
		if len(out) >= maxZones {
			break
		}
		raw, err := os.ReadFile(filepath.Join(z, "temp"))
		if err != nil {
			continue
		}
		milli, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		c := float64(milli) / 1000
		if c <= -100 || c >= 200 {
			continue
		}
		kind := readTrim(filepath.Join(z, "type"))
		if kind == "" {
			kind = filepath.Base(z)
		}
		out = append(out, proto.Series{Kind: "temp", Label: kind, A: c})
		if s.Temp == nil || c > *s.Temp {
			v := c
			s.Temp = &v
		}
	}
	return out
}

// readCounters gathers every monotonic counter we diff over time.
func (c *Collector) readCounters(at time.Time) (*snapshot, error) {
	s := &snapshot{
		at:   at,
		net:  readNet(c.proc + "/net/dev"),
		disk: readDisk(c.sys, c.proc+"/diskstats"),
	}
	file, err := os.Open(c.proc + "/stat")
	if err != nil {
		return s, err
	}
	defer file.Close()

	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		t := parseCPUTimes(fields[1:])
		if fields[0] == "cpu" {
			s.cpu = t
			continue
		}
		s.core = append(s.core, t)
	}
	return s, nil
}

// parseCPUTimes sums user/nice/system/irq/softirq/steal into total and
// user/nice into nice, idle/iowait into idle.
func parseCPUTimes(v []string) cpuTimes {
	var total, idle uint64
	for i, raw := range v {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			continue
		}
		total += n
		switch i {
		case 3, 4: // idle, iowait
			idle += n
		}
	}
	return cpuTimes{total: total, idle: idle}
}

func readNet(path string) map[string]netCounters {
	out := map[string]netCounters{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "lo" || name == "" || virtualInterface(name) {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[name] = netCounters{rx: rx, tx: tx}
	}
	return out
}

func virtualInterface(name string) bool {
	for _, p := range virtualNetPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// readDisk walks /sys/block so we only see whole block devices. Partitions
// live under their parent and are skipped automatically; stacked devices
// (dm-*, md*) are skipped because their members are already counted, which
// would otherwise double every write.
func readDisk(sys, diskstats string) map[string]diskCounters {
	stats := readDiskstats(diskstats)
	out := map[string]diskCounters{}
	entries, err := os.ReadDir(filepath.Join(sys, "block"))
	if err != nil {
		for name, d := range stats {
			out[name] = d
		}
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if skipBlockDevice(name, filepath.Join(sys, "block", name)) {
			continue
		}
		if d, ok := stats[name]; ok {
			out[name] = d
		}
	}
	return out
}

func skipBlockDevice(name, dir string) bool {
	for _, p := range []string{"loop", "ram", "zram", "fd", "sr", "dm-", "md"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	// A non-empty slaves/ directory means this device is built on top of
	// others that we already count.
	slaves, err := os.ReadDir(filepath.Join(dir, "slaves"))
	return err == nil && len(slaves) > 0
}

// readDiskstats reads /proc/diskstats. Fields 3 and 6 (0-based) are sectors
// read and written; both are 512-byte sectors regardless of the device's
// logical block size, per the kernel's documentation.
func readDiskstats(path string) map[string]diskCounters {
	out := map[string]diskCounters{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		rs, err1 := strconv.ParseUint(fields[5], 10, 64)
		ws, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = diskCounters{read: rs * 512, write: ws * 512}
	}
	return out
}

func procFields(path string, want map[string]bool) map[string]uint64 {
	out := map[string]uint64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := line[:i]
		if !want[key] {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo is in kB; the agent reports bytes.
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		out[key] = v
	}
	return out
}

func rate(cur, prev uint64, elapsed float64) float64 {
	if elapsed <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / elapsed
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// unescapeMount decodes the octal escapes the kernel writes for spaces and
// other awkward characters in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func skipMount(mount string) bool {
	for _, p := range fsSkipPrefixes {
		if mount == p || strings.HasPrefix(mount, p+"/") {
			return true
		}
	}
	return false
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// machineID prefers the systemd machine-id, then the older dbus location,
// and falls back to a hash of the hostname so a host without either still
// gets a stable identity.
func machineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if v := readTrim(p); v != "" {
			return v
		}
	}
	sum := sha256.Sum256([]byte(hostname()))
	return hex.EncodeToString(sum[:8])
}

func hostname() string {
	if v, err := os.Hostname(); err == nil && v != "" {
		return v
	}
	return "unknown"
}

func kernelRelease() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func osPrettyName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return "Linux"
}

func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		for _, key := range []string{"model name", "Hardware", "Model"} {
			if v, ok := strings.CutPrefix(line, key); ok {
				// The separator between key and value is a tab on x86 and a
				// colon on ARM, sometimes both.
				if s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ":")); s != "" {
					return s
				}
			}
		}
		if line == "" {
			break
		}
	}
	return ""
}
