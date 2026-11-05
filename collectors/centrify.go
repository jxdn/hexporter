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
			"1 if adclient is connected to a domain controller (adinfo: CentrifyDC mode connected)", nil, nil),
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

	// Join status, zone and DC connection from `adinfo` (= --all). It answers
	// from adclient's state, so it is cheap and sends nothing to the DC.
	joined, dcOK := 0.0, 0.0
	domain, zone := "unknown", "unknown"
	if out, err := runCmd("adinfo"); err == nil {
		a := parseAdinfo(out)
		if a.domain != "" {
			joined, domain = 1, a.domain
		}
		if a.zone != "" {
			zone = a.zone
		}
		if a.mode == "connected" {
			dcOK = 1
		}
	} else if strings.Contains(strings.ToLower(out), "not joined") {
		// exit 10: not joined; not a collector error
	} else {
		log.Printf("centrify: adinfo: %v", err)
		collErr = 1
	}
	ch <- prometheus.MustNewConstMetric(c.domainJoined, prometheus.GaugeValue, joined)
	ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, domain, zone)
	ch <- prometheus.MustNewConstMetric(c.dcReachable, prometheus.GaugeValue, dcOK)

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

type adinfoState struct{ domain, zone, mode string }

// parseAdinfo reads `adinfo` output ("Joined to domain:  x", "Zone:  y",
// "CentrifyDC mode:   connected").
func parseAdinfo(out string) adinfoState {
	var a adinfoState
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "joined to domain":
			a.domain = v
		case "zone":
			a.zone = v
		case "centrifydc mode":
			a.mode = strings.ToLower(v)
		}
	}
	return a
}
