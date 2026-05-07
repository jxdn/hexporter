package collectors

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type cpuStats struct {
	user, nice, system, idle, iowait, irq, softirq, steal, total float64
}

// CPUCollector collects CPU usage, frequency, load average, and process counts.
type CPUCollector struct {
	usagePercent   *prometheus.Desc
	iowaitPercent  *prometheus.Desc
	stealPercent   *prometheus.Desc
	coresTotal     *prometheus.Desc
	freqMHz        *prometheus.Desc
	loadAvg        *prometheus.Desc
	processesTotal *prometheus.Desc
	errorDesc      *prometheus.Desc

	mu       sync.Mutex
	prevStat *cpuStats
}

func NewCPUCollector() *CPUCollector {
	return &CPUCollector{
		usagePercent: prometheus.NewDesc("hpc_cpu_usage_percent",
			"CPU usage percentage (user+nice+system+irq+softirq+steal)", nil, nil),
		iowaitPercent: prometheus.NewDesc("hpc_cpu_iowait_percent",
			"CPU iowait percentage", nil, nil),
		stealPercent: prometheus.NewDesc("hpc_cpu_steal_percent",
			"CPU steal time percentage", nil, nil),
		coresTotal: prometheus.NewDesc("hpc_cpu_cores_total",
			"Total number of logical CPU cores", nil, nil),
		freqMHz: prometheus.NewDesc("hpc_cpu_frequency_mhz",
			"Current CPU core frequency in MHz", []string{"core"}, nil),
		loadAvg: prometheus.NewDesc("hpc_load_average",
			"System load average", []string{"period"}, nil),
		processesTotal: prometheus.NewDesc("hpc_processes_total",
			"Number of processes by state", []string{"state"}, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "cpu"}),
	}
}

func (c *CPUCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.usagePercent
	ch <- c.iowaitPercent
	ch <- c.stealPercent
	ch <- c.coresTotal
	ch <- c.freqMHz
	ch <- c.loadAvg
	ch <- c.processesTotal
	ch <- c.errorDesc
}

func (c *CPUCollector) Collect(ch chan<- prometheus.Metric) {
	var collErr float64

	cur, err := readCPUStat()
	if err != nil {
		log.Printf("cpu: /proc/stat: %v", err)
		collErr = 1
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
		return
	}

	c.mu.Lock()
	prev := c.prevStat
	c.prevStat = cur
	c.mu.Unlock()

	if prev != nil && cur.total > prev.total {
		dt := cur.total - prev.total
		used := (cur.user - prev.user + cur.nice - prev.nice +
			cur.system - prev.system + cur.irq - prev.irq +
			cur.softirq - prev.softirq + cur.steal - prev.steal) / dt * 100
		iowait := (cur.iowait - prev.iowait) / dt * 100
		steal := (cur.steal - prev.steal) / dt * 100
		ch <- prometheus.MustNewConstMetric(c.usagePercent, prometheus.GaugeValue, used)
		ch <- prometheus.MustNewConstMetric(c.iowaitPercent, prometheus.GaugeValue, iowait)
		ch <- prometheus.MustNewConstMetric(c.stealPercent, prometheus.GaugeValue, steal)
	} else {
		ch <- prometheus.MustNewConstMetric(c.usagePercent, prometheus.GaugeValue, 0)
		ch <- prometheus.MustNewConstMetric(c.iowaitPercent, prometheus.GaugeValue, 0)
		ch <- prometheus.MustNewConstMetric(c.stealPercent, prometheus.GaugeValue, 0)
	}

	running, sleeping, total := countProcesses()
	ch <- prometheus.MustNewConstMetric(c.processesTotal, prometheus.GaugeValue, float64(running), "running")
	ch <- prometheus.MustNewConstMetric(c.processesTotal, prometheus.GaugeValue, float64(sleeping), "sleeping")
	ch <- prometheus.MustNewConstMetric(c.processesTotal, prometheus.GaugeValue, float64(total), "total")

	cores := countCPUCores()
	ch <- prometheus.MustNewConstMetric(c.coresTotal, prometheus.GaugeValue, float64(cores))

	// Prefer sysfs per-core frequencies; fall back to /proc/cpuinfo.
	if err := collectSysfsFreqs(ch, c.freqMHz); err != nil {
		collectCPUInfoFreqs(ch, c.freqMHz)
	}

	la1, la5, la15, err := readLoadAvg()
	if err != nil {
		log.Printf("cpu: loadavg: %v", err)
		collErr = 1
	} else {
		ch <- prometheus.MustNewConstMetric(c.loadAvg, prometheus.GaugeValue, la1, "1m")
		ch <- prometheus.MustNewConstMetric(c.loadAvg, prometheus.GaugeValue, la5, "5m")
		ch <- prometheus.MustNewConstMetric(c.loadAvg, prometheus.GaugeValue, la15, "15m")
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

func readCPUStat() (*cpuStats, error) {
	f, err := os.Open(procPath("stat"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			return nil, fmt.Errorf("short cpu line in /proc/stat")
		}
		var v [10]float64
		for i := 1; i < len(fields) && i <= 10; i++ {
			v[i-1], _ = strconv.ParseFloat(fields[i], 64)
		}
		s := &cpuStats{
			user: v[0], nice: v[1], system: v[2], idle: v[3],
			iowait: v[4], irq: v[5], softirq: v[6], steal: v[7],
		}
		s.total = s.user + s.nice + s.system + s.idle + s.iowait + s.irq + s.softirq + s.steal
		return s, nil
	}
	return nil, fmt.Errorf("cpu line not found in /proc/stat")
}

func countProcesses() (running, sleeping, total int) {
	matches, _ := filepath.Glob(procPath("[0-9]*/stat"))
	total = len(matches)
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		s := string(data)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 >= len(s) {
			continue
		}
		switch s[idx+2] {
		case 'R':
			running++
		case 'S', 'I':
			sleeping++
		}
	}
	return
}

func countCPUCores() int {
	f, err := os.Open(procPath("cpuinfo"))
	if err != nil {
		return 0
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "processor") {
			count++
		}
	}
	return count
}

func collectSysfsFreqs(ch chan<- prometheus.Metric, desc *prometheus.Desc) error {
	matches, _ := filepath.Glob(sysPath("devices/system/cpu/cpu*/cpufreq/scaling_cur_freq"))
	if len(matches) == 0 {
		return fmt.Errorf("no cpufreq sysfs files")
	}
	for _, m := range matches {
		// .../cpu3/cpufreq/scaling_cur_freq → base of parent's parent = "cpu3"
		cpuDir := filepath.Base(filepath.Dir(filepath.Dir(m)))
		coreNum := strings.TrimPrefix(cpuDir, "cpu")
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		freqKHz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, freqKHz/1000, coreNum)
	}
	return nil
}

func collectCPUInfoFreqs(ch chan<- prometheus.Metric, desc *prometheus.Desc) {
	f, err := os.Open(procPath("cpuinfo"))
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	coreNum := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "processor") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				coreNum = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "cpu MHz") && coreNum != "" {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				freq, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
				if err == nil {
					ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, freq, coreNum)
				}
			}
		}
	}
}

func readLoadAvg() (float64, float64, float64, error) {
	data, err := os.ReadFile(procPath("loadavg"))
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("unexpected /proc/loadavg format")
	}
	la1, _ := strconv.ParseFloat(fields[0], 64)
	la5, _ := strconv.ParseFloat(fields[1], 64)
	la15, _ := strconv.ParseFloat(fields[2], 64)
	return la1, la5, la15, nil
}
