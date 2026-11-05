package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"hpc-exporter/collectors"
)

// version is set at build time: -ldflags "-X main.version=1.0.0"
var version = "dev"

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

func enabled(name string) bool { return os.Getenv(name) != "false" }

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("hpc-exporter %s (%s)\n", version, runtime.Version())
		return
	}

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
	var names []string
	if enabled("NFS_ENABLED") {
		cols = append(cols, collectors.NewNFSCollector())
		names = append(names, "nfs")
	}
	if enabled("NVIDIA_ENABLED") {
		cols = append(cols, collectors.NewGPUCollector())
		names = append(names, "gpu")
	}
	if enabled("CENTRIFY_ENABLED") {
		cols = append(cols, collectors.NewCentrifyCollector())
		names = append(names, "centrify")
	}
	if enabled("PBS_ENABLED") {
		cols = append(cols, collectors.NewPBSCollector())
		names = append(names, "pbs")
	}

	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "hpc_exporter_build_info",
		Help:        "hexporter version (value 1)",
		ConstLabels: prometheus.Labels{"version": version, "goversion": runtime.Version()},
	})
	buildInfo.Set(1)

	reg := prometheus.NewRegistry()
	reg.MustRegister(&hpcExporter{cols: cols}, buildInfo)

	// ContinueOnError: a bad series (e.g. a duplicate from odd hardware) is
	// logged and dropped instead of failing the whole scrape with HTTP 500.
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      log.New(os.Stderr, "", log.LstdFlags),
		ErrorHandling: promhttp.ContinueOnError,
	}))
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hpc-exporter %s - metrics at /metrics\n", version)
	})

	log.Printf("hpc-exporter %s listening on :%s/metrics (optional collectors: %v)", version, port, names)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
