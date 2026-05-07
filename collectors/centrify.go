package collectors

import (
	"log"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// CentrifyCollector checks Centrify DirectControl service health and AD membership.
// Gracefully skipped if centrifydc is not installed.
type CentrifyCollector struct {
	serviceUp    *prometheus.Desc
	domainJoined *prometheus.Desc
	dcReachable  *prometheus.Desc
	info         *prometheus.Desc
	errorDesc    *prometheus.Desc
}

func NewCentrifyCollector() *CentrifyCollector {
	return &CentrifyCollector{
		serviceUp: prometheus.NewDesc("hpc_centrify_service_up",
			"1 if centrifydc service is active", nil, nil),
		domainJoined: prometheus.NewDesc("hpc_centrify_domain_joined",
			"1 if the host is joined to an AD domain", nil, nil),
		dcReachable: prometheus.NewDesc("hpc_centrify_dc_reachable",
			"1 if the domain controller is reachable", nil, nil),
		info: prometheus.NewDesc("hpc_centrify_info",
			"Centrify domain and zone information (gauge=1)",
			[]string{"domain", "zone"}, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "centrify"}),
	}
}

func (c *CentrifyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.serviceUp
	ch <- c.domainJoined
	ch <- c.dcReachable
	ch <- c.info
	ch <- c.errorDesc
}

func (c *CentrifyCollector) Collect(ch chan<- prometheus.Metric) {
	// Skip entirely if centrifydc is not installed.
	if !binaryExists("adinfo") && !binaryExists("centrifydc") {
		return
	}

	var collErr float64

	// Service state via systemctl.
	serviceUp := 0.0
	if out, err := runCmd("systemctl", "is-active", "centrifydc"); err == nil {
		if strings.TrimSpace(out) == "active" {
			serviceUp = 1
		}
	} else {
		log.Printf("centrify: systemctl is-active centrifydc: %v", err)
		collErr = 1
	}
	ch <- prometheus.MustNewConstMetric(c.serviceUp, prometheus.GaugeValue, serviceUp)

	// Domain join status and zone via adinfo --show.
	joined := 0.0
	domain := "unknown"
	zone := "unknown"
	if out, err := runCmd("adinfo", "--show"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			lower := strings.ToLower(line)
			if strings.Contains(lower, "joined") {
				joined = 1
			}
			if strings.HasPrefix(lower, "domain:") || strings.HasPrefix(lower, "ad domain:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					domain = strings.TrimSpace(parts[1])
				}
			}
			if strings.HasPrefix(lower, "zone:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					zone = strings.TrimSpace(parts[1])
				}
			}
		}
	} else {
		log.Printf("centrify: adinfo --show: %v", err)
		collErr = 1
	}
	ch <- prometheus.MustNewConstMetric(c.domainJoined, prometheus.GaugeValue, joined)
	ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, domain, zone)

	// DC connectivity via adinfo --test.
	dcOK := 0.0
	if out, err := runCmd("adinfo", "--test"); err == nil {
		lower := strings.ToLower(out)
		if strings.Contains(lower, "success") || strings.Contains(lower, "passed") ||
			strings.Contains(lower, "ok") {
			dcOK = 1
		}
	} else {
		log.Printf("centrify: adinfo --test: %v", err)
		collErr = 1
	}
	ch <- prometheus.MustNewConstMetric(c.dcReachable, prometheus.GaugeValue, dcOK)

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}
