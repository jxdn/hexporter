package collectors

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// HealthCollector gathers EDAC memory error counts, OOM kills, MCE events
// and system uptime.
type HealthCollector struct {
	uptime    *prometheus.Desc
	edacCE    *prometheus.Desc
	edacUE    *prometheus.Desc
	oomKills  *prometheus.Desc
	mceErrors *prometheus.Desc
	errorDesc *prometheus.Desc
}

func NewHealthCollector() *HealthCollector {
	return &HealthCollector{
		uptime: prometheus.NewDesc("hpc_system_uptime_seconds",
			"System uptime in seconds", nil, nil),
		edacCE: prometheus.NewDesc("hpc_edac_correctable_errors_total",
			"EDAC correctable memory errors per memory controller", []string{"mc"}, nil),
		edacUE: prometheus.NewDesc("hpc_edac_uncorrectable_errors_total",
			"EDAC uncorrectable memory errors per memory controller", []string{"mc"}, nil),
		oomKills: prometheus.NewDesc("hpc_oom_kills_total",
			"Total OOM kill events since boot", nil, nil),
		mceErrors: prometheus.NewDesc("hpc_mce_errors_total",
			"Machine Check Exception events reported by rasdaemon", nil, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "health"}),
	}
}

func (c *HealthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.uptime
	ch <- c.edacCE
	ch <- c.edacUE
	ch <- c.oomKills
	ch <- c.mceErrors
	ch <- c.errorDesc
}

func (c *HealthCollector) Collect(ch chan<- prometheus.Metric) {
	var collErr float64

	if up, err := readUptime(); err != nil {
		log.Printf("health: uptime: %v", err)
		collErr = 1
	} else {
		ch <- prometheus.MustNewConstMetric(c.uptime, prometheus.GaugeValue, up)
	}

	// EDAC memory errors.
	mcDirs, _ := filepath.Glob(sysPath("devices/system/edac/mc/mc*"))
	for _, mcDir := range mcDirs {
		mcName := filepath.Base(mcDir)
		mcIdx := strings.TrimPrefix(mcName, "mc")

		if v, err := readUintSysFile(filepath.Join(mcDir, "ce_count")); err == nil {
			ch <- prometheus.MustNewConstMetric(c.edacCE, prometheus.CounterValue, float64(v), mcIdx)
		} else {
			log.Printf("health: edac CE %s: %v", mcDir, err)
			collErr = 1
		}
		if v, err := readUintSysFile(filepath.Join(mcDir, "ue_count")); err == nil {
			ch <- prometheus.MustNewConstMetric(c.edacUE, prometheus.CounterValue, float64(v), mcIdx)
		} else {
			log.Printf("health: edac UE %s: %v", mcDir, err)
			collErr = 1
		}
	}

	// OOM kills from /proc/vmstat.
	if oom, err := readOOMKills(); err != nil {
		log.Printf("health: vmstat: %v", err)
		collErr = 1
	} else {
		ch <- prometheus.MustNewConstMetric(c.oomKills, prometheus.CounterValue, float64(oom))
	}

	// MCE count via ras-mc-ctl if rasdaemon is available.
	if binaryExists("ras-mc-ctl") {
		if count, err := readMCECount(); err != nil {
			log.Printf("health: ras-mc-ctl: %v", err)
		} else {
			ch <- prometheus.MustNewConstMetric(c.mceErrors, prometheus.CounterValue, float64(count))
		}
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

func readUintSysFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func readOOMKills() (uint64, error) {
	f, err := os.Open(procPath("vmstat"))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "oom_kill ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return strconv.ParseUint(fields[1], 10, 64)
			}
		}
	}
	// oom_kill field absent on older kernels — return 0, not an error.
	return 0, scanner.Err()
}

func readMCECount() (int, error) {
	out, err := runCmd("ras-mc-ctl", "--summary")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "machine check") || strings.Contains(lower, "mce") {
			// Lines like "Machine check: N events"
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "events" || f == "errors" {
					if i > 0 {
						if n, err := strconv.Atoi(fields[i-1]); err == nil {
							count += n
						}
					}
				}
			}
		}
	}
	return count, nil
}

func readUptime() (float64, error) {
	data, err := os.ReadFile(procPath("uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, nil
	}
	return strconv.ParseFloat(fields[0], 64)
}
