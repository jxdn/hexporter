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

// NetworkCollector gathers /proc/net/dev stats and optional InfiniBand metrics.
type NetworkCollector struct {
	rxBytes   *prometheus.Desc
	txBytes   *prometheus.Desc
	rxPackets *prometheus.Desc
	txPackets *prometheus.Desc
	rxErrors  *prometheus.Desc
	txErrors  *prometheus.Desc

	ibPortState       *prometheus.Desc
	ibPortRate        *prometheus.Desc
	ibRcvErrors       *prometheus.Desc
	ibSymbolErrors    *prometheus.Desc
	ibLinkDowned      *prometheus.Desc
	ibPortRxBytes     *prometheus.Desc
	ibPortTxBytes     *prometheus.Desc

	errorDesc *prometheus.Desc
}

func NewNetworkCollector() *NetworkCollector {
	ifLabels := []string{"interface", "type"}
	ibLabels := []string{"interface", "port"}
	return &NetworkCollector{
		rxBytes:   prometheus.NewDesc("hpc_network_rx_bytes_total", "Total bytes received", ifLabels, nil),
		txBytes:   prometheus.NewDesc("hpc_network_tx_bytes_total", "Total bytes transmitted", ifLabels, nil),
		rxPackets: prometheus.NewDesc("hpc_network_rx_packets_total", "Total packets received", ifLabels, nil),
		txPackets: prometheus.NewDesc("hpc_network_tx_packets_total", "Total packets transmitted", ifLabels, nil),
		rxErrors:  prometheus.NewDesc("hpc_network_rx_errors_total", "Total receive errors", ifLabels, nil),
		txErrors:  prometheus.NewDesc("hpc_network_tx_errors_total", "Total transmit errors", ifLabels, nil),

		ibPortState:    prometheus.NewDesc("hpc_ib_port_state", "InfiniBand port state (1=active, 0=down)", ibLabels, nil),
		ibPortRate:     prometheus.NewDesc("hpc_ib_port_rate_gbps", "InfiniBand port data rate in Gb/s", ibLabels, nil),
		ibRcvErrors:    prometheus.NewDesc("hpc_ib_port_rcv_errors_total", "IB port receive errors", ibLabels, nil),
		ibSymbolErrors: prometheus.NewDesc("hpc_ib_port_symbol_errors_total", "IB port symbol errors", ibLabels, nil),
		ibLinkDowned:   prometheus.NewDesc("hpc_ib_port_link_downed_total", "IB port link downed counter", ibLabels, nil),
		ibPortRxBytes:  prometheus.NewDesc("hpc_ib_port_rx_bytes_total", "IB port bytes received", ibLabels, nil),
		ibPortTxBytes:  prometheus.NewDesc("hpc_ib_port_tx_bytes_total", "IB port bytes transmitted", ibLabels, nil),

		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "network"}),
	}
}

func (c *NetworkCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.rxBytes
	ch <- c.txBytes
	ch <- c.rxPackets
	ch <- c.txPackets
	ch <- c.rxErrors
	ch <- c.txErrors
	ch <- c.ibPortState
	ch <- c.ibPortRate
	ch <- c.ibRcvErrors
	ch <- c.ibSymbolErrors
	ch <- c.ibLinkDowned
	ch <- c.ibPortRxBytes
	ch <- c.ibPortTxBytes
	ch <- c.errorDesc
}

func (c *NetworkCollector) Collect(ch chan<- prometheus.Metric) {
	var collErr float64

	ifaces, err := parseNetDev()
	if err != nil {
		log.Printf("network: /proc/net/dev: %v", err)
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 1)
		return
	}

	for _, iface := range ifaces {
		ifType := ifaceType(iface.name)
		lv := []string{iface.name, ifType}
		ch <- prometheus.MustNewConstMetric(c.rxBytes, prometheus.CounterValue, iface.rxBytes, lv...)
		ch <- prometheus.MustNewConstMetric(c.txBytes, prometheus.CounterValue, iface.txBytes, lv...)
		ch <- prometheus.MustNewConstMetric(c.rxPackets, prometheus.CounterValue, iface.rxPackets, lv...)
		ch <- prometheus.MustNewConstMetric(c.txPackets, prometheus.CounterValue, iface.txPackets, lv...)
		ch <- prometheus.MustNewConstMetric(c.rxErrors, prometheus.CounterValue, iface.rxErrors, lv...)
		ch <- prometheus.MustNewConstMetric(c.txErrors, prometheus.CounterValue, iface.txErrors, lv...)
	}

	// InfiniBand stats via ibstat + perfquery if available.
	if binaryExists("ibstat") {
		if err := c.collectIBStat(ch); err != nil {
			log.Printf("network: ibstat: %v", err)
			collErr = 1
		}
	}
	if binaryExists("perfquery") {
		if err := c.collectPerfQuery(ch); err != nil {
			log.Printf("network: perfquery: %v", err)
			collErr = 1
		}
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

type ifaceStat struct {
	name                             string
	rxBytes, txBytes                 float64
	rxPackets, txPackets             float64
	rxErrors, txErrors               float64
}

func parseNetDev() ([]ifaceStat, error) {
	f, err := os.Open(procPath("net/dev"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ifaces []ifaceStat
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 { // skip header lines
			continue
		}
		line := scanner.Text()
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colonIdx])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 16 {
			continue
		}
		pf := func(i int) float64 {
			v, _ := strconv.ParseFloat(fields[i], 64)
			return v
		}
		// Skip interfaces with zero traffic (not connected).
		if pf(0)+pf(8) == 0 {
			continue
		}
		ifaces = append(ifaces, ifaceStat{
			name:      name,
			rxBytes:   pf(0),
			rxPackets: pf(1),
			rxErrors:  pf(2),
			txBytes:   pf(8),
			txPackets: pf(9),
			txErrors:  pf(10),
		})
	}
	return ifaces, scanner.Err()
}

// ifaceType returns "ib" for InfiniBand interfaces (ib*, ibs*), else "eth".
func ifaceType(name string) string {
	if strings.HasPrefix(name, "ib") {
		return "ib"
	}
	return "eth"
}

// collectIBStat runs ibstat and parses port state and rate.
func (c *NetworkCollector) collectIBStat(ch chan<- prometheus.Metric) error {
	out, err := runCmd("ibstat")
	if err != nil {
		return err
	}

	var caName, portNum string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CA '") {
			caName = strings.Trim(strings.TrimPrefix(line, "CA '"), "'")
		}
		if strings.HasPrefix(line, "Port ") {
			portNum = strings.TrimSuffix(strings.TrimPrefix(line, "Port "), ":")
		}
		if caName == "" || portNum == "" {
			continue
		}
		lv := []string{caName, portNum}
		if strings.HasPrefix(line, "State:") {
			state := strings.TrimSpace(strings.TrimPrefix(line, "State:"))
			val := 0.0
			if strings.EqualFold(state, "Active") {
				val = 1
			}
			ch <- prometheus.MustNewConstMetric(c.ibPortState, prometheus.GaugeValue, val, lv...)
		}
		if strings.HasPrefix(line, "Rate:") {
			rateStr := strings.TrimSpace(strings.TrimPrefix(line, "Rate:"))
			rate, _ := strconv.ParseFloat(rateStr, 64)
			ch <- prometheus.MustNewConstMetric(c.ibPortRate, prometheus.GaugeValue, rate, lv...)
		}
	}
	return nil
}

// collectPerfQuery runs perfquery and parses error/bandwidth counters.
// It queries all local ports by iterating CA devices found in /sys/class/infiniband/.
func (c *NetworkCollector) collectPerfQuery(ch chan<- prometheus.Metric) error {
	// Discover CA devices.
	caDevs := ibCADevices()
	if len(caDevs) == 0 {
		caDevs = []string{""} // let perfquery use default
	}

	for _, ca := range caDevs {
		args := []string{}
		if ca != "" {
			args = append(args, "-C", ca)
		}
		out, err := runCmd("perfquery", args...)
		if err != nil {
			continue
		}

		// Header line: "# Port counters: Lid N port P (PortSelect M)"
		portNum := "1"
		caName := ca
		if caName == "" {
			caName = "default"
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# Port counters:") {
				// Extract port number.
				if idx := strings.Index(line, "port "); idx >= 0 {
					rest := line[idx+5:]
					fields := strings.Fields(rest)
					if len(fields) > 0 {
						portNum = fields[0]
					}
				}
			}
		}
		lv := []string{caName, portNum}

		pf := func(line, prefix string) float64 {
			if !strings.HasPrefix(line, prefix) {
				return -1
			}
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return -1
			}
			v, _ := strconv.ParseFloat(parts[len(parts)-1], 64)
			return v
		}

		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if v := pf(line, "PortRcvErrors"); v >= 0 {
				ch <- prometheus.MustNewConstMetric(c.ibRcvErrors, prometheus.CounterValue, v, lv...)
			}
			if v := pf(line, "SymbolErrorCounter"); v >= 0 {
				ch <- prometheus.MustNewConstMetric(c.ibSymbolErrors, prometheus.CounterValue, v, lv...)
			}
			if v := pf(line, "LinkDownedCounter"); v >= 0 {
				ch <- prometheus.MustNewConstMetric(c.ibLinkDowned, prometheus.CounterValue, v, lv...)
			}
			if v := pf(line, "PortRcvData"); v >= 0 {
				// IB counts in 4-byte words.
				ch <- prometheus.MustNewConstMetric(c.ibPortRxBytes, prometheus.CounterValue, v*4, lv...)
			}
			if v := pf(line, "PortXmitData"); v >= 0 {
				ch <- prometheus.MustNewConstMetric(c.ibPortTxBytes, prometheus.CounterValue, v*4, lv...)
			}
		}
	}
	return nil
}

// ibCADevices returns the list of InfiniBand CA device names from sysfs.
func ibCADevices() []string {
	matches, _ := filepath.Glob(sysPath("class/infiniband/*"))
	var devs []string
	for _, m := range matches {
		devs = append(devs, filepath.Base(m))
	}
	return devs
}
