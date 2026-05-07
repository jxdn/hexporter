package collectors

import (
	"log"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// GPUCollector gathers NVIDIA GPU metrics via nvidia-smi.
// The entire collector is silently skipped if nvidia-smi is not found.
type GPUCollector struct {
	utilization    *prometheus.Desc
	memUsed        *prometheus.Desc
	memFree        *prometheus.Desc
	memTotal       *prometheus.Desc
	temperature    *prometheus.Desc
	powerDraw      *prometheus.Desc
	eccCorrected   *prometheus.Desc
	eccUncorrected *prometheus.Desc
	clockSM        *prometheus.Desc
	clockMem       *prometheus.Desc
	throttle       *prometheus.Desc
	errorDesc      *prometheus.Desc
}

func NewGPUCollector() *GPUCollector {
	labels := []string{"gpu_index", "gpu_name", "gpu_uuid"}
	throttleLabels := []string{"gpu_index", "gpu_name", "gpu_uuid", "reason"}
	return &GPUCollector{
		utilization:    prometheus.NewDesc("hpc_gpu_utilization_percent", "GPU utilization %", labels, nil),
		memUsed:        prometheus.NewDesc("hpc_gpu_memory_used_bytes", "GPU memory used", labels, nil),
		memFree:        prometheus.NewDesc("hpc_gpu_memory_free_bytes", "GPU memory free", labels, nil),
		memTotal:       prometheus.NewDesc("hpc_gpu_memory_total_bytes", "GPU memory total", labels, nil),
		temperature:    prometheus.NewDesc("hpc_gpu_temperature_celsius", "GPU temperature", labels, nil),
		powerDraw:      prometheus.NewDesc("hpc_gpu_power_draw_watts", "GPU power draw", labels, nil),
		eccCorrected:   prometheus.NewDesc("hpc_gpu_ecc_errors_corrected_total", "Corrected ECC errors", labels, nil),
		eccUncorrected: prometheus.NewDesc("hpc_gpu_ecc_errors_uncorrected_total", "Uncorrected ECC errors", labels, nil),
		clockSM:        prometheus.NewDesc("hpc_gpu_clock_sm_mhz", "GPU SM clock MHz", labels, nil),
		clockMem:       prometheus.NewDesc("hpc_gpu_clock_mem_mhz", "GPU memory clock MHz", labels, nil),
		throttle:       prometheus.NewDesc("hpc_gpu_throttle_reason", "GPU throttle reason bitmask", throttleLabels, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "gpu"}),
	}
}

func (c *GPUCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.utilization
	ch <- c.memUsed
	ch <- c.memFree
	ch <- c.memTotal
	ch <- c.temperature
	ch <- c.powerDraw
	ch <- c.eccCorrected
	ch <- c.eccUncorrected
	ch <- c.clockSM
	ch <- c.clockMem
	ch <- c.throttle
	ch <- c.errorDesc
}

func (c *GPUCollector) Collect(ch chan<- prometheus.Metric) {
	if !binaryExists("nvidia-smi") {
		return
	}

	out, err := runCmd("nvidia-smi",
		"--query-gpu=index,name,uuid,utilization.gpu,memory.used,memory.free,memory.total,"+
			"temperature.gpu,power.draw,ecc_errors.corrected.volatile.total,"+
			"ecc_errors.uncorrected.volatile.total,clocks.sm,clocks.mem,"+
			"clocks_throttle_reasons.active",
		"--format=csv,noheader,nounits")
	if err != nil {
		log.Printf("gpu: nvidia-smi: %v", err)
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 1)
		return
	}

	var collErr float64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, ", ")
		if len(fields) < 14 {
			fields = splitCSV(line)
		}
		if len(fields) < 14 {
			log.Printf("gpu: unexpected nvidia-smi line: %q", line)
			collErr = 1
			continue
		}

		idx := strings.TrimSpace(fields[0])
		name := strings.TrimSpace(fields[1])
		uuid := strings.TrimSpace(fields[2])
		lv := []string{idx, name, uuid}

		pf := func(i int) (float64, bool) {
			s := strings.TrimSpace(fields[i])
			if s == "N/A" || s == "[N/A]" || s == "" {
				return 0, false
			}
			v, err := strconv.ParseFloat(s, 64)
			return v, err == nil
		}

		if v, ok := pf(3); ok {
			ch <- prometheus.MustNewConstMetric(c.utilization, prometheus.GaugeValue, v, lv...)
		}
		const mib = 1024 * 1024
		if v, ok := pf(4); ok {
			ch <- prometheus.MustNewConstMetric(c.memUsed, prometheus.GaugeValue, v*mib, lv...)
		}
		if v, ok := pf(5); ok {
			ch <- prometheus.MustNewConstMetric(c.memFree, prometheus.GaugeValue, v*mib, lv...)
		}
		if v, ok := pf(6); ok {
			ch <- prometheus.MustNewConstMetric(c.memTotal, prometheus.GaugeValue, v*mib, lv...)
		}
		if v, ok := pf(7); ok {
			ch <- prometheus.MustNewConstMetric(c.temperature, prometheus.GaugeValue, v, lv...)
		}
		if v, ok := pf(8); ok {
			ch <- prometheus.MustNewConstMetric(c.powerDraw, prometheus.GaugeValue, v, lv...)
		}
		if v, ok := pf(9); ok {
			ch <- prometheus.MustNewConstMetric(c.eccCorrected, prometheus.CounterValue, v, lv...)
		}
		if v, ok := pf(10); ok {
			ch <- prometheus.MustNewConstMetric(c.eccUncorrected, prometheus.CounterValue, v, lv...)
		}
		if v, ok := pf(11); ok {
			ch <- prometheus.MustNewConstMetric(c.clockSM, prometheus.GaugeValue, v, lv...)
		}
		if v, ok := pf(12); ok {
			ch <- prometheus.MustNewConstMetric(c.clockMem, prometheus.GaugeValue, v, lv...)
		}

		// Throttle reason: parse hex bitmask and emit as gauge with reason label.
		throttleStr := strings.TrimSpace(fields[13])
		var mask int64
		if strings.HasPrefix(throttleStr, "0x") || strings.HasPrefix(throttleStr, "0X") {
			mask, _ = strconv.ParseInt(throttleStr[2:], 16, 64)
		} else {
			mask, _ = strconv.ParseInt(throttleStr, 10, 64)
		}
		ch <- prometheus.MustNewConstMetric(c.throttle, prometheus.GaugeValue, float64(mask),
			append(lv, "active_mask")...)
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

// splitCSV splits a CSV line respecting quoted fields.
func splitCSV(line string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, ch := range line {
		switch {
		case ch == '"':
			inQuote = !inQuote
		case ch == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(ch)
		}
	}
	fields = append(fields, cur.String())
	return fields
}
