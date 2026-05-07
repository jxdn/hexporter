package collectors

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
)

var skipFSTypes = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true,
	"cgroup": true, "cgroup2": true, "proc": true,
	"sysfs": true, "none": true, "devpts": true,
	"securityfs": true, "pstore": true, "efivarfs": true,
	"autofs": true, "hugetlbfs": true, "mqueue": true,
	"debugfs": true, "tracefs": true, "fusectl": true,
	"configfs": true, "ramfs": true, "squashfs": true,
	"bpf": true, "rpc_pipefs": true,
}

type mountEntry struct {
	device     string
	mountpoint string
	fstype     string
	mountType  string // "local" or "nfs"
}

type diskIOStat struct {
	readsCompleted  float64
	writesCompleted float64
	sectorsRead     float64
	sectorsWritten  float64
	ioInProgress    float64
	msDoingIO       float64
}

// DiskCollector gathers disk space and I/O statistics.
type DiskCollector struct {
	totalBytes  *prometheus.Desc
	usedBytes   *prometheus.Desc
	freeBytes   *prometheus.Desc
	inodesTotal *prometheus.Desc
	inodesUsed  *prometheus.Desc
	inodesFree  *prometheus.Desc

	readIOPS   *prometheus.Desc
	writeIOPS  *prometheus.Desc
	readBytes  *prometheus.Desc
	writeBytes *prometheus.Desc
	ioQueue    *prometheus.Desc
	ioLatency  *prometheus.Desc

	errorDesc *prometheus.Desc

	mu        sync.Mutex
	prevStats map[string]*diskIOStat
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

		readIOPS:   prometheus.NewDesc("hpc_disk_read_iops_total", "Total read I/Os completed", devLabels, nil),
		writeIOPS:  prometheus.NewDesc("hpc_disk_write_iops_total", "Total write I/Os completed", devLabels, nil),
		readBytes:  prometheus.NewDesc("hpc_disk_read_bytes_total", "Total bytes read", devLabels, nil),
		writeBytes: prometheus.NewDesc("hpc_disk_write_bytes_total", "Total bytes written", devLabels, nil),
		ioQueue:    prometheus.NewDesc("hpc_disk_io_queue_depth", "Current I/Os in progress", devLabels, nil),
		ioLatency:  prometheus.NewDesc("hpc_disk_io_latency_ms", "Average I/O latency in ms (delta)", devLabels, nil),

		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "disk"}),
		prevStats: make(map[string]*diskIOStat),
	}
}

func (c *DiskCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalBytes
	ch <- c.usedBytes
	ch <- c.freeBytes
	ch <- c.inodesTotal
	ch <- c.inodesUsed
	ch <- c.inodesFree
	ch <- c.readIOPS
	ch <- c.writeIOPS
	ch <- c.readBytes
	ch <- c.writeBytes
	ch <- c.ioQueue
	ch <- c.ioLatency
	ch <- c.errorDesc
}

func (c *DiskCollector) Collect(ch chan<- prometheus.Metric) {
	var collErr float64

	mounts, err := parseMounts()
	if err != nil {
		log.Printf("disk: mounts: %v", err)
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 1)
		return
	}

	for _, m := range mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.mountpoint, &st); err != nil {
			log.Printf("disk: statfs %s: %v", m.mountpoint, err)
			collErr = 1
			continue
		}
		bsize := float64(st.Bsize)
		total := float64(st.Blocks) * bsize
		free := float64(st.Bavail) * bsize
		used := (float64(st.Blocks) - float64(st.Bfree)) * bsize
		inodesUsed := float64(st.Files) - float64(st.Ffree)
		if inodesUsed < 0 {
			inodesUsed = 0
		}

		lv := []string{m.mountpoint, m.device, m.fstype, m.mountType}
		ch <- prometheus.MustNewConstMetric(c.totalBytes, prometheus.GaugeValue, total, lv...)
		ch <- prometheus.MustNewConstMetric(c.usedBytes, prometheus.GaugeValue, used, lv...)
		ch <- prometheus.MustNewConstMetric(c.freeBytes, prometheus.GaugeValue, free, lv...)
		ch <- prometheus.MustNewConstMetric(c.inodesTotal, prometheus.GaugeValue, float64(st.Files), lv...)
		ch <- prometheus.MustNewConstMetric(c.inodesUsed, prometheus.GaugeValue, inodesUsed, lv...)
		ch <- prometheus.MustNewConstMetric(c.inodesFree, prometheus.GaugeValue, float64(st.Ffree), lv...)
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

			if prev != nil {
				dOps := (cur.readsCompleted - prev.readsCompleted) + (cur.writesCompleted - prev.writesCompleted)
				dMs := cur.msDoingIO - prev.msDoingIO
				latency := 0.0
				if dOps > 0 {
					latency = dMs / dOps
				}
				ch <- prometheus.MustNewConstMetric(c.ioLatency, prometheus.GaugeValue, latency, dev)
			} else {
				ch <- prometheus.MustNewConstMetric(c.ioLatency, prometheus.GaugeValue, 0, dev)
			}
		}
		c.mu.Unlock()
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

func parseMounts() ([]mountEntry, error) {
	f, err := os.Open(procPath("mounts"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []mountEntry
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device, mountpoint, fstype := fields[0], fields[1], fields[2]
		if skipFSTypes[fstype] {
			continue
		}
		if seen[mountpoint] {
			continue
		}
		seen[mountpoint] = true

		mountType := "local"
		if fstype == "nfs" || fstype == "nfs4" || strings.HasPrefix(fstype, "nfs") {
			mountType = "nfs"
		}

		mounts = append(mounts, mountEntry{
			device:     device,
			mountpoint: mountpoint,
			fstype:     fstype,
			mountType:  mountType,
		})
	}
	return mounts, scanner.Err()
}

// parseDiskStats parses /proc/diskstats and returns stats keyed by device name.
// Only includes whole-disk devices (partitions are skipped by checking if the
// device name is used as a block device without a numeric suffix on the parent).
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
		// Skip partition entries (e.g. sda1, sdb2) and dm-* with all-zero I/O.
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

func isPartition(dev string) bool {
	// Partitions typically end with a digit after a non-digit.
	// e.g. sda1, nvme0n1p1. Whole disks: sda, nvme0n1, sdb.
	if len(dev) == 0 {
		return false
	}
	last := dev[len(dev)-1]
	if last < '0' || last > '9' {
		return false
	}
	// nvme devices: nvme0n1 is whole disk, nvme0n1p1 is partition.
	if strings.Contains(dev, "nvme") && strings.Contains(dev, "p") {
		return true
	}
	// sdX1 style: if removing trailing digits leaves a name ending in a letter.
	base := strings.TrimRight(dev, "0123456789")
	if len(base) > 0 {
		last := base[len(base)-1]
		if (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') {
			return true
		}
	}
	return false
}

