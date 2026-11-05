package collectors

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type diskIOStat struct {
	readsCompleted  float64
	writesCompleted float64
	sectorsRead     float64
	sectorsWritten  float64
	ioInProgress    float64
	msDoingIO       float64
}

// DiskCollector gathers filesystem space, filesystem health (responsive,
// read-only, expected mounts) and block-device I/O statistics.
//
// Mounts come from the host's mount table (HOST_PROC/1/mounts) and statfs is
// done on HOST_ROOT+mountpoint, with a timeout so a hung NFS/dpc mount can't
// block the scrape.
type DiskCollector struct {
	totalBytes  *prometheus.Desc
	usedBytes   *prometheus.Desc
	freeBytes   *prometheus.Desc
	inodesTotal *prometheus.Desc
	inodesUsed  *prometheus.Desc
	inodesFree  *prometheus.Desc

	fsInfo       *prometheus.Desc
	fsResponsive *prometheus.Desc
	fsStatDur    *prometheus.Desc
	fsReadonly   *prometheus.Desc
	fsMounted    *prometheus.Desc

	readIOPS   *prometheus.Desc
	writeIOPS  *prometheus.Desc
	readBytes  *prometheus.Desc
	writeBytes *prometheus.Desc
	ioQueue    *prometheus.Desc
	ioLatency  *prometheus.Desc

	errorDesc *prometheus.Desc

	timeout  time.Duration
	expected []string

	mu        sync.Mutex
	prevStats map[string]*diskIOStat
	inflight  map[string]bool // statfs calls still stuck from earlier scrapes
}

func NewDiskCollector() *DiskCollector {
	labels := []string{"mountpoint", "device", "fstype", "mount_type"}
	devLabels := []string{"device"}
	return &DiskCollector{
		totalBytes:  prometheus.NewDesc("hpc_disk_total_bytes", "Disk total size in bytes", labels, nil),
		usedBytes:   prometheus.NewDesc("hpc_disk_used_bytes", "Disk used bytes", labels, nil),
		freeBytes:   prometheus.NewDesc("hpc_disk_free_bytes", "Disk free bytes available to non-root", labels, nil),
		inodesTotal: prometheus.NewDesc("hpc_disk_inodes_total", "Total inodes", labels, nil),
		inodesUsed:  prometheus.NewDesc("hpc_disk_inodes_used", "Used inodes", labels, nil),
		inodesFree:  prometheus.NewDesc("hpc_disk_inodes_free", "Free inodes", labels, nil),

		fsInfo: prometheus.NewDesc("hpc_fs_info", "Mounted filesystem (value 1)",
			[]string{"mountpoint", "fstype", "mount_type", "server", "export", "nfs_vers", "dev"}, nil),
		fsResponsive: prometheus.NewDesc("hpc_fs_responsive",
			"1 if statfs answered within FS_STAT_TIMEOUT", []string{"mountpoint", "mount_type"}, nil),
		fsStatDur: prometheus.NewDesc("hpc_fs_stat_duration_seconds",
			"Time statfs took", []string{"mountpoint"}, nil),
		fsReadonly: prometheus.NewDesc("hpc_fs_readonly",
			"1 if mounted read-only", []string{"mountpoint", "mount_type"}, nil),
		fsMounted: prometheus.NewDesc("hpc_fs_mounted",
			"For EXPECTED_MOUNTS: 1 if mounted, 0 if missing", []string{"mountpoint"}, nil),

		readIOPS:   prometheus.NewDesc("hpc_disk_read_iops_total", "Total read I/Os completed", devLabels, nil),
		writeIOPS:  prometheus.NewDesc("hpc_disk_write_iops_total", "Total write I/Os completed", devLabels, nil),
		readBytes:  prometheus.NewDesc("hpc_disk_read_bytes_total", "Total bytes read", devLabels, nil),
		writeBytes: prometheus.NewDesc("hpc_disk_write_bytes_total", "Total bytes written", devLabels, nil),
		ioQueue:    prometheus.NewDesc("hpc_disk_io_queue_depth", "Current I/Os in progress", devLabels, nil),
		ioLatency:  prometheus.NewDesc("hpc_disk_io_latency_ms", "Average I/O latency in ms (delta)", devLabels, nil),

		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "disk"}),

		timeout:   envDuration("FS_STAT_TIMEOUT", 5*time.Second),
		expected:  envList("EXPECTED_MOUNTS"),
		prevStats: make(map[string]*diskIOStat),
		inflight:  make(map[string]bool),
	}
}

func (c *DiskCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.totalBytes, c.usedBytes, c.freeBytes, c.inodesTotal, c.inodesUsed, c.inodesFree,
		c.fsInfo, c.fsResponsive, c.fsStatDur, c.fsReadonly, c.fsMounted,
		c.readIOPS, c.writeIOPS, c.readBytes, c.writeBytes, c.ioQueue, c.ioLatency, c.errorDesc,
	} {
		ch <- d
	}
}

type statResult struct {
	st   syscall.Statfs_t
	err  error
	dur  time.Duration
	hung bool
}

// statWithTimeout runs statfs in a goroutine. If it doesn't return within the
// timeout the mount counts as hung; the goroutine is left to finish on its own
// and no new statfs is started on that path until it does.
func (c *DiskCollector) statWithTimeout(path string) statResult {
	c.mu.Lock()
	if c.inflight[path] {
		c.mu.Unlock()
		return statResult{hung: true, dur: c.timeout}
	}
	c.inflight[path] = true
	c.mu.Unlock()

	done := make(chan statResult, 1)
	go func() {
		start := time.Now()
		var st syscall.Statfs_t
		err := syscall.Statfs(path, &st)
		done <- statResult{st: st, err: err, dur: time.Since(start)}
		c.mu.Lock()
		delete(c.inflight, path)
		c.mu.Unlock()
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(c.timeout):
		return statResult{hung: true, dur: c.timeout}
	}
}

func (c *DiskCollector) Collect(ch chan<- prometheus.Metric) {
	var collErr float64

	mounts, err := readHostMounts()
	if err != nil {
		log.Printf("disk: mounts: %v", err)
		collErr = 1
	}

	// statfs all mounts in parallel, each with its own timeout.
	results := make([]statResult, len(mounts))
	var wg sync.WaitGroup
	for i := range mounts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.statWithTimeout(hostPath(mounts[i].Mountpoint))
		}(i)
	}
	wg.Wait()

	mounted := map[string]bool{}
	for i, m := range mounts {
		mounted[m.Mountpoint] = true
		ch <- prometheus.MustNewConstMetric(c.fsInfo, prometheus.GaugeValue, 1,
			m.Mountpoint, m.FSType, m.Class, m.Server, m.Export, m.NFSVers, m.Dev)
		ro := 0.0
		if m.ReadOnly {
			ro = 1
		}
		ch <- prometheus.MustNewConstMetric(c.fsReadonly, prometheus.GaugeValue, ro, m.Mountpoint, m.Class)

		r := results[i]
		ch <- prometheus.MustNewConstMetric(c.fsStatDur, prometheus.GaugeValue, r.dur.Seconds(), m.Mountpoint)
		if r.hung {
			log.Printf("disk: statfs %s: no answer within %s (hung mount?)", m.Mountpoint, c.timeout)
			ch <- prometheus.MustNewConstMetric(c.fsResponsive, prometheus.GaugeValue, 0, m.Mountpoint, m.Class)
			continue
		}
		if r.err != nil {
			log.Printf("disk: statfs %s: %v", m.Mountpoint, r.err)
			ch <- prometheus.MustNewConstMetric(c.fsResponsive, prometheus.GaugeValue, 0, m.Mountpoint, m.Class)
			collErr = 1
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.fsResponsive, prometheus.GaugeValue, 1, m.Mountpoint, m.Class)

		st := r.st
		bsize := float64(st.Bsize)
		total := float64(st.Blocks) * bsize
		free := float64(st.Bavail) * bsize
		used := (float64(st.Blocks) - float64(st.Bfree)) * bsize
		inodesUsed := float64(st.Files) - float64(st.Ffree)
		if inodesUsed < 0 {
			inodesUsed = 0
		}
		lv := []string{m.Mountpoint, m.Device, m.FSType, m.Class}
		ch <- prometheus.MustNewConstMetric(c.totalBytes, prometheus.GaugeValue, total, lv...)
		ch <- prometheus.MustNewConstMetric(c.usedBytes, prometheus.GaugeValue, used, lv...)
		ch <- prometheus.MustNewConstMetric(c.freeBytes, prometheus.GaugeValue, free, lv...)
		ch <- prometheus.MustNewConstMetric(c.inodesTotal, prometheus.GaugeValue, float64(st.Files), lv...)
		ch <- prometheus.MustNewConstMetric(c.inodesUsed, prometheus.GaugeValue, inodesUsed, lv...)
		ch <- prometheus.MustNewConstMetric(c.inodesFree, prometheus.GaugeValue, float64(st.Ffree), lv...)
	}

	for _, mp := range c.expected {
		v := 0.0
		if mounted[mp] {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.fsMounted, prometheus.GaugeValue, v, mp)
	}

	// I/O stats from /proc/diskstats
	iostats, err := parseDiskStats()
	if err != nil {
		log.Printf("disk: diskstats: %v", err)
		collErr = 1
	} else {
		c.mu.Lock()
		for dev, cur := range iostats {
			prev := c.prevStats[dev]
			c.prevStats[dev] = cur

			ch <- prometheus.MustNewConstMetric(c.readIOPS, prometheus.CounterValue, cur.readsCompleted, dev)
			ch <- prometheus.MustNewConstMetric(c.writeIOPS, prometheus.CounterValue, cur.writesCompleted, dev)
			ch <- prometheus.MustNewConstMetric(c.readBytes, prometheus.CounterValue, cur.sectorsRead*512, dev)
			ch <- prometheus.MustNewConstMetric(c.writeBytes, prometheus.CounterValue, cur.sectorsWritten*512, dev)
			ch <- prometheus.MustNewConstMetric(c.ioQueue, prometheus.GaugeValue, cur.ioInProgress, dev)

			latency := 0.0
			if prev != nil {
				dOps := (cur.readsCompleted - prev.readsCompleted) + (cur.writesCompleted - prev.writesCompleted)
				if dOps > 0 {
					latency = (cur.msDoingIO - prev.msDoingIO) / dOps
				}
			}
			ch <- prometheus.MustNewConstMetric(c.ioLatency, prometheus.GaugeValue, latency, dev)
		}
		c.mu.Unlock()
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

// parseDiskStats parses /proc/diskstats and returns stats keyed by device name
// (whole disks only; partitions are skipped).
func parseDiskStats() (map[string]*diskIOStat, error) {
	f, err := os.Open(procPath("diskstats"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stats := make(map[string]*diskIOStat)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		dev := fields[2]
		if isPartition(dev) {
			continue
		}
		pf := func(i int) float64 {
			v, _ := strconv.ParseFloat(fields[i], 64)
			return v
		}
		stats[dev] = &diskIOStat{
			readsCompleted:  pf(3),
			sectorsRead:     pf(5),
			writesCompleted: pf(7),
			sectorsWritten:  pf(9),
			ioInProgress:    pf(11),
			msDoingIO:       pf(12),
		}
	}
	return stats, scanner.Err()
}

// isPartition reports whether a block device name is a partition
// (sda1, nvme0n1p1) rather than a whole disk (sda, nvme0n1).
func isPartition(dev string) bool {
	if len(dev) == 0 {
		return false
	}
	last := dev[len(dev)-1]
	if last < '0' || last > '9' {
		return false
	}
	if strings.Contains(dev, "nvme") && strings.Contains(dev, "p") {
		return true
	}
	base := strings.TrimRight(dev, "0123456789")
	if len(base) > 0 {
		last := base[len(base)-1]
		if (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') {
			return true
		}
	}
	return false
}
