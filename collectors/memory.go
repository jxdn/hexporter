package collectors

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// MemoryCollector parses /proc/meminfo for memory and swap metrics.
type MemoryCollector struct {
	totalBytes     *prometheus.Desc
	usedBytes      *prometheus.Desc
	availableBytes *prometheus.Desc
	freeBytes      *prometheus.Desc
	buffersBytes   *prometheus.Desc
	cachedBytes    *prometheus.Desc
	swapTotal      *prometheus.Desc
	swapUsed       *prometheus.Desc
	swapFree       *prometheus.Desc
	errorDesc      *prometheus.Desc
}

func NewMemoryCollector() *MemoryCollector {
	return &MemoryCollector{
		totalBytes:     prometheus.NewDesc("hpc_memory_total_bytes", "Total RAM", nil, nil),
		usedBytes:      prometheus.NewDesc("hpc_memory_used_bytes", "Used RAM (total - free - buffers - cached)", nil, nil),
		availableBytes: prometheus.NewDesc("hpc_memory_available_bytes", "Available RAM (MemAvailable)", nil, nil),
		freeBytes:      prometheus.NewDesc("hpc_memory_free_bytes", "Free RAM (MemFree)", nil, nil),
		buffersBytes:   prometheus.NewDesc("hpc_memory_buffers_bytes", "Buffers RAM", nil, nil),
		cachedBytes:    prometheus.NewDesc("hpc_memory_cached_bytes", "Cached RAM", nil, nil),
		swapTotal:      prometheus.NewDesc("hpc_swap_total_bytes", "Total swap", nil, nil),
		swapUsed:       prometheus.NewDesc("hpc_swap_used_bytes", "Used swap", nil, nil),
		swapFree:       prometheus.NewDesc("hpc_swap_free_bytes", "Free swap", nil, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "memory"}),
	}
}

func (c *MemoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalBytes
	ch <- c.usedBytes
	ch <- c.availableBytes
	ch <- c.freeBytes
	ch <- c.buffersBytes
	ch <- c.cachedBytes
	ch <- c.swapTotal
	ch <- c.swapUsed
	ch <- c.swapFree
	ch <- c.errorDesc
}

func (c *MemoryCollector) Collect(ch chan<- prometheus.Metric) {
	info, err := parseMemInfo()
	if err != nil {
		log.Printf("memory: %v", err)
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 1)
		return
	}

	get := func(key string) float64 { return info[key] * 1024 }

	total := get("MemTotal")
	free := get("MemFree")
	buffers := get("Buffers")
	cached := get("Cached")
	available := get("MemAvailable")
	used := total - free - buffers - cached

	ch <- prometheus.MustNewConstMetric(c.totalBytes, prometheus.GaugeValue, total)
	ch <- prometheus.MustNewConstMetric(c.usedBytes, prometheus.GaugeValue, used)
	ch <- prometheus.MustNewConstMetric(c.availableBytes, prometheus.GaugeValue, available)
	ch <- prometheus.MustNewConstMetric(c.freeBytes, prometheus.GaugeValue, free)
	ch <- prometheus.MustNewConstMetric(c.buffersBytes, prometheus.GaugeValue, buffers)
	ch <- prometheus.MustNewConstMetric(c.cachedBytes, prometheus.GaugeValue, cached)

	swapTotal := get("SwapTotal")
	swapFree := get("SwapFree")
	ch <- prometheus.MustNewConstMetric(c.swapTotal, prometheus.GaugeValue, swapTotal)
	ch <- prometheus.MustNewConstMetric(c.swapUsed, prometheus.GaugeValue, swapTotal-swapFree)
	ch <- prometheus.MustNewConstMetric(c.swapFree, prometheus.GaugeValue, swapFree)

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 0)
}

// parseMemInfo returns /proc/meminfo values in kB keyed by field name (without "kB").
func parseMemInfo() (map[string]float64, error) {
	f, err := os.Open(procPath("meminfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info := make(map[string]float64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(parts[1])
		valStr = strings.TrimSuffix(valStr, " kB")
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		info[key] = val
	}
	return info, scanner.Err()
}
