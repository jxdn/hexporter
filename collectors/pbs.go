package collectors

import (
	"log"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// PBSCollector checks OpenPBS/Torque service and mom daemon state.
// Gracefully skipped if PBS is not installed.
type PBSCollector struct {
	serviceUp *prometheus.Desc
	momUp     *prometheus.Desc
	errorDesc *prometheus.Desc
}

func NewPBSCollector() *PBSCollector {
	return &PBSCollector{
		serviceUp: prometheus.NewDesc("hpc_pbs_service_up",
			"1 if the pbs service is active", nil, nil),
		momUp: prometheus.NewDesc("hpc_pbs_mom_up",
			"1 if the pbs_mom daemon is active", nil, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "pbs"}),
	}
}

func (c *PBSCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.serviceUp
	ch <- c.momUp
	ch <- c.errorDesc
}

func (c *PBSCollector) Collect(ch chan<- prometheus.Metric) {
	if !binaryExists("systemctl") {
		return
	}

	var collErr float64

	pbsUp := serviceActive("pbs")
	if pbsUp < 0 {
		// Unit not found — PBS likely not installed.
		return
	}
	if pbsUp == -2 {
		log.Printf("pbs: systemctl query failed")
		collErr = 1
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
		return
	}

	ch <- prometheus.MustNewConstMetric(c.serviceUp, prometheus.GaugeValue, float64(pbsUp))

	momVal := serviceActive("pbs_mom")
	if momVal < 0 {
		momVal = 0
	}
	ch <- prometheus.MustNewConstMetric(c.momUp, prometheus.GaugeValue, float64(momVal))

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

// serviceActive returns 1 if the unit is active, 0 if inactive/failed,
// -1 if the unit is not found (not-found), -2 on exec error.
func serviceActive(unit string) int {
	out, err := runCmd("systemctl", "is-active", unit)
	if err != nil {
		// systemctl exits non-zero for inactive; that is expected.
		status := strings.TrimSpace(out)
		if status == "" {
			// Real exec failure.
			return -2
		}
		if status == "inactive" || status == "failed" || status == "deactivating" {
			return 0
		}
		if status == "not-found" || status == "unknown" {
			return -1
		}
		return 0
	}
	if strings.TrimSpace(out) == "active" {
		return 1
	}
	return 0
}
