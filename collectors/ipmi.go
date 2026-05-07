package collectors

import (
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// IPMICollector gathers power and SEL data via ipmitool, plus system uptime.
// Gracefully skipped if ipmitool is not found.
type IPMICollector struct {
	powerWatts    *prometheus.Desc
	selEntries    *prometheus.Desc
	uptimeSeconds *prometheus.Desc
	errorDesc     *prometheus.Desc
}

func NewIPMICollector() *IPMICollector {
	return &IPMICollector{
		powerWatts: prometheus.NewDesc("hpc_ipmi_power_watts",
			"Instantaneous system power reading from IPMI DCMI", nil, nil),
		selEntries: prometheus.NewDesc("hpc_ipmi_sel_entries_total",
			"System Event Log entry count by severity", []string{"severity"}, nil),
		uptimeSeconds: prometheus.NewDesc("hpc_system_uptime_seconds",
			"System uptime in seconds", nil, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "ipmi"}),
	}
}

func (c *IPMICollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.powerWatts
	ch <- c.selEntries
	ch <- c.uptimeSeconds
	ch <- c.errorDesc
}

func (c *IPMICollector) Collect(ch chan<- prometheus.Metric) {
	if !binaryExists("ipmitool") {
		return
	}

	var collErr float64

	if watts, err := readIPMIPower(); err != nil {
		log.Printf("ipmi: power: %v", err)
		collErr = 1
	} else {
		ch <- prometheus.MustNewConstMetric(c.powerWatts, prometheus.GaugeValue, watts)
	}

	info, warning, critical := readIPMISEL()
	ch <- prometheus.MustNewConstMetric(c.selEntries, prometheus.GaugeValue, float64(info), "info")
	ch <- prometheus.MustNewConstMetric(c.selEntries, prometheus.GaugeValue, float64(warning), "warning")
	ch <- prometheus.MustNewConstMetric(c.selEntries, prometheus.GaugeValue, float64(critical), "critical")

	if uptime, err := readUptime(); err != nil {
		log.Printf("ipmi: uptime: %v", err)
		collErr = 1
	} else {
		ch <- prometheus.MustNewConstMetric(c.uptimeSeconds, prometheus.GaugeValue, uptime)
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

var rePower = regexp.MustCompile(`Instantaneous power reading:\s+(\d+(?:\.\d+)?)\s+Watts`)

func readIPMIPower() (float64, error) {
	out, err := runCmd("ipmitool", "dcmi", "power", "reading")
	if err != nil {
		return 0, err
	}
	m := rePower.FindStringSubmatch(out)
	if m == nil {
		return 0, nil
	}
	return strconv.ParseFloat(m[1], 64)
}

func readIPMISEL() (info, warning, critical int) {
	out, err := runCmd("ipmitool", "sel", "elist", "last", "20")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		lower := strings.ToLower(line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch {
		case strings.Contains(lower, "critical"):
			critical++
		case strings.Contains(lower, "warning") || strings.Contains(lower, "non-critical"):
			warning++
		default:
			info++
		}
	}
	return
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
