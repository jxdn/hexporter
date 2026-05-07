package main

import (
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"hpc-exporter/collectors"
)

// hpcExporter fans out to all sub-collectors concurrently.
type hpcExporter struct {
	cols []prometheus.Collector
}

func (e *hpcExporter) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range e.cols {
		c.Describe(ch)
	}
}

func (e *hpcExporter) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	for _, c := range e.cols {
		wg.Add(1)
		go func(col prometheus.Collector) {
			defer wg.Done()
			col.Collect(ch)
		}(c)
	}
	wg.Wait()
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9009"
	}

	cols := []prometheus.Collector{
		collectors.NewCPUCollector(),
		collectors.NewMemoryCollector(),
		collectors.NewDiskCollector(),
		collectors.NewNetworkCollector(),
		collectors.NewTemperatureCollector(),
		collectors.NewHealthCollector(),
	}

	if os.Getenv("NVIDIA_ENABLED") != "false" {
		cols = append(cols, collectors.NewGPUCollector())
	}
	if os.Getenv("IPMI_ENABLED") != "false" {
		cols = append(cols, collectors.NewIPMICollector())
	}
	if os.Getenv("CENTRIFY_ENABLED") != "false" {
		cols = append(cols, collectors.NewCentrifyCollector())
	}
	if os.Getenv("PBS_ENABLED") != "false" {
		cols = append(cols, collectors.NewPBSCollector())
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(&hpcExporter{cols: cols})

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog: log.New(os.Stderr, "", log.LstdFlags),
	}))

	log.Printf("HPC Exporter listening on :%s/metrics", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
