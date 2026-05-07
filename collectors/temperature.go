package collectors

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// TemperatureCollector reads hwmon and thermal_zone sensors from /sys.
type TemperatureCollector struct {
	hwmonTemp      *prometheus.Desc
	thermalZone    *prometheus.Desc
	errorDesc      *prometheus.Desc
}

func NewTemperatureCollector() *TemperatureCollector {
	return &TemperatureCollector{
		hwmonTemp: prometheus.NewDesc("hpc_temperature_celsius",
			"Hardware sensor temperature in Celsius",
			[]string{"sensor", "chip", "type"}, nil),
		thermalZone: prometheus.NewDesc("hpc_thermal_zone_celsius",
			"Thermal zone temperature in Celsius",
			[]string{"zone", "type"}, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "temperature"}),
	}
}

func (c *TemperatureCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.hwmonTemp
	ch <- c.thermalZone
	ch <- c.errorDesc
}

func (c *TemperatureCollector) Collect(ch chan<- prometheus.Metric) {
	var collErr float64

	if err := c.collectHwmon(ch); err != nil {
		log.Printf("temperature: hwmon: %v", err)
		collErr = 1
	}
	if err := c.collectThermalZones(ch); err != nil {
		log.Printf("temperature: thermal_zone: %v", err)
		collErr = 1
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

func (c *TemperatureCollector) collectHwmon(ch chan<- prometheus.Metric) error {
	hwmonDirs, err := filepath.Glob(sysPath("class/hwmon/hwmon*"))
	if err != nil {
		return err
	}

	for _, hwmonDir := range hwmonDirs {
		chipName := readSysFile(filepath.Join(hwmonDir, "name"))
		if chipName == "" {
			chipName = filepath.Base(hwmonDir)
		}

		inputs, _ := filepath.Glob(filepath.Join(hwmonDir, "temp*_input"))
		for _, inputPath := range inputs {
			data, err := os.ReadFile(inputPath)
			if err != nil {
				continue
			}
			milliC, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
			if err != nil {
				continue
			}
			celsius := milliC / 1000.0

			// Derive sensor label from temp*_label or construct from file name.
			base := strings.TrimSuffix(filepath.Base(inputPath), "_input")
			labelPath := filepath.Join(hwmonDir, base+"_label")
			sensorLabel := readSysFile(labelPath)
			if sensorLabel == "" {
				sensorLabel = base
			}
			sensorLabel = sanitizeLabel(sensorLabel)

			// Determine type: package vs core.
			sensorType := "core"
			lower := strings.ToLower(sensorLabel)
			if strings.Contains(lower, "package") || strings.Contains(lower, "pkg") {
				sensorType = "cpu_package"
			} else if strings.Contains(lower, "core") {
				sensorType = "core"
			} else if strings.Contains(lower, "acpi") {
				sensorType = "acpi"
			}

			ch <- prometheus.MustNewConstMetric(c.hwmonTemp, prometheus.GaugeValue, celsius,
				sensorLabel, chipName, sensorType)
		}
	}
	return nil
}

func (c *TemperatureCollector) collectThermalZones(ch chan<- prometheus.Metric) error {
	zones, err := filepath.Glob(sysPath("class/thermal/thermal_zone*/temp"))
	if err != nil {
		return err
	}

	for _, tempPath := range zones {
		data, err := os.ReadFile(tempPath)
		if err != nil {
			continue
		}
		milliC, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}
		celsius := milliC / 1000.0

		zoneDir := filepath.Dir(tempPath)
		zoneName := strings.TrimPrefix(filepath.Base(zoneDir), "thermal_zone")
		zoneType := readSysFile(filepath.Join(zoneDir, "type"))
		if zoneType == "" {
			zoneType = "unknown"
		}

		ch <- prometheus.MustNewConstMetric(c.thermalZone, prometheus.GaugeValue, celsius,
			zoneName, zoneType)
	}
	return nil
}

// readSysFile reads a sysfs file and returns its trimmed content, or "" on error.
func readSysFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// sanitizeLabel replaces characters invalid in prometheus label values.
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ToLower(s)
	return s
}
