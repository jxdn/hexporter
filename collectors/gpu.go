package collectors

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// GPUCollector exposes GPU metrics, GPU -> PBS job -> user -> framework
// mapping and per-process GPU memory.
//
// Sources, per scrape:
//  1. nvidia-smi --query-gpu      GPU list (index, uuid, name) + basic values
//     (used as the fallback when DCGM is down)
//  2. dcgmi dmon                  all values incl. health and profiling;
//     overrides the nvidia-smi values
//  3. nvidia-smi --query-compute-apps   processes on each GPU
//  4. /proc                       PBS jobs (see jobmap.go)
type GPUCollector struct {
	dcgmEnabled bool
	jobMapping  bool
	procMetrics bool
	dcgm        dcgmReader

	descs      map[string]*prometheus.Desc // metric name -> desc
	jobInfo    *prometheus.Desc
	procMem    *prometheus.Desc
	unreadable *prometheus.Desc
	dcgmUp     *prometheus.Desc
	errorDesc  *prometheus.Desc

	smiMu       sync.Mutex
	smiThrottle string // working name of the throttle field for this driver
}

// smiField maps an nvidia-smi --query-gpu field to a metric (fallback path).
type smiField struct {
	Query    string
	Metric   string
	LabelVal string
	Scale    float64
}

var smiFields = []smiField{
	{"utilization.gpu", "hpc_gpu_utilization_percent", "", 1},
	{"utilization.memory", "hpc_gpu_memory_copy_utilization_percent", "", 1},
	{"memory.used", "hpc_gpu_memory_used_bytes", "", mib},
	{"memory.free", "hpc_gpu_memory_free_bytes", "", mib},
	{"memory.total", "hpc_gpu_memory_total_bytes", "", mib},
	{"temperature.gpu", "hpc_gpu_temperature_celsius", "", 1},
	{"power.draw", "hpc_gpu_power_draw_watts", "", 1},
	{"ecc.errors.corrected.volatile.total", "hpc_gpu_ecc_errors_corrected_total", "", 1},
	{"ecc.errors.uncorrected.volatile.total", "hpc_gpu_ecc_errors_uncorrected_total", "", 1},
	{"clocks.sm", "hpc_gpu_clock_sm_mhz", "", 1},
	{"clocks.mem", "hpc_gpu_clock_mem_mhz", "", 1},
	{"THROTTLE", "hpc_gpu_throttle_reason", "active_mask", 1}, // name resolved at runtime
}

var gpuBaseLabels = []string{"gpu_index", "gpu_name", "gpu_uuid"}

func NewGPUCollector() *GPUCollector {
	c := &GPUCollector{
		dcgmEnabled: envBool("DCGM_ENABLED", true),
		jobMapping:  envBool("GPU_JOB_MAPPING", true),
		procMetrics: envBool("GPU_PROCESS_METRICS", true),
		descs:       map[string]*prometheus.Desc{},
		smiThrottle: "clocks_event_reasons.active",
	}
	for i := range gpuFields {
		f := &gpuFields[i]
		if _, ok := c.descs[f.Metric]; ok {
			continue
		}
		labels := gpuBaseLabels
		if f.LabelKey != "" {
			labels = append(append([]string{}, gpuBaseLabels...), f.LabelKey)
		}
		c.descs[f.Metric] = prometheus.NewDesc(f.Metric, f.Help, labels, nil)
	}
	c.jobInfo = prometheus.NewDesc("hpc_gpu_job_info",
		"1 for each GPU held by a PBS job (or used by a non-PBS process, jobid=\"none\")",
		[]string{"gpu_index", "gpu_uuid", "jobid", "user", "queue", "job_name", "framework", "source"}, nil)
	c.procMem = prometheus.NewDesc("hpc_gpu_process_memory_used_bytes",
		"GPU memory used by a process",
		[]string{"gpu_index", "gpu_uuid", "pid", "process_name", "framework", "jobid", "user"}, nil)
	c.unreadable = prometheus.NewDesc("hpc_gpu_jobmap_unreadable_processes",
		"Processes whose environment could not be read (needs root + SYS_PTRACE)", nil, nil)
	c.dcgmUp = prometheus.NewDesc("hpc_gpu_dcgm_up",
		"1 if DCGM answered during this scrape (0 = nvidia-smi fallback)", nil, nil)
	c.errorDesc = prometheus.NewDesc("hpc_collector_error",
		"1 if the collector encountered an error during the last scrape",
		nil, prometheus.Labels{"collector": "gpu"})
	return c
}

func (c *GPUCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs {
		ch <- d
	}
	ch <- c.jobInfo
	ch <- c.procMem
	ch <- c.unreadable
	ch <- c.dcgmUp
	ch <- c.errorDesc
}

// gpuInfo is one GPU with its values keyed by metric|labelVal.
type gpuInfo struct {
	Index, UUID, Name string
	vals              map[string]float64
}

// gpuApp is one process on a GPU from nvidia-smi.
type gpuApp struct {
	UUID   string
	PID    int
	Name   string
	MemMiB float64
}

func (c *GPUCollector) Collect(ch chan<- prometheus.Metric) {
	if !binaryExists("nvidia-smi") {
		return
	}
	var collErr float64

	gpus, err := c.querySMI()
	if err != nil {
		log.Printf("gpu: nvidia-smi: %v", err)
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 1)
		return
	}
	byIndex := map[string]*gpuInfo{}
	for _, g := range gpus {
		byIndex[g.Index] = g
	}

	// DCGM values override the nvidia-smi ones.
	dcgmUp := 0.0
	if c.dcgmEnabled {
		if vals, err := c.dcgm.read(); err != nil {
			log.Printf("gpu: DCGM unavailable, using nvidia-smi: %v", err)
		} else {
			dcgmUp = 1
			for idx, fv := range vals {
				g := byIndex[idx]
				if g == nil {
					continue
				}
				for f, v := range fv {
					g.vals[f.key()] = v
				}
			}
		}
		ch <- prometheus.MustNewConstMetric(c.dcgmUp, prometheus.GaugeValue, dcgmUp)
	}

	// Emit value metrics.
	fieldByKey := map[string]*gpuField{}
	for i := range gpuFields {
		fieldByKey[gpuFields[i].key()] = &gpuFields[i]
	}
	for _, g := range gpus {
		for k, v := range g.vals {
			f := fieldByKey[k]
			if f == nil {
				continue
			}
			lv := []string{g.Index, g.Name, g.UUID}
			if f.LabelKey != "" {
				lv = append(lv, f.LabelVal)
			}
			vt := prometheus.GaugeValue
			if f.counter() {
				vt = prometheus.CounterValue
			}
			ch <- prometheus.MustNewConstMetric(c.descs[f.Metric], vt, v, lv...)
		}
	}

	// Processes, jobs, frameworks.
	apps, err := queryApps()
	if err != nil {
		log.Printf("gpu: nvidia-smi --query-compute-apps: %v", err)
		collErr = 1
	}
	if c.jobMapping || c.procMetrics {
		var scan *jobScan
		if c.jobMapping {
			scan = scanJobs()
			ch <- prometheus.MustNewConstMetric(c.unreadable, prometheus.GaugeValue, float64(scan.Unreadable))
		}
		assign, procRows := mapGPUs(gpus, apps, scan)
		if c.jobMapping {
			for _, a := range assign {
				ch <- prometheus.MustNewConstMetric(c.jobInfo, prometheus.GaugeValue, 1,
					a.GPUIndex, a.UUID, a.JobID, a.User, a.Queue, a.JobName, a.Framework, a.source())
			}
		}
		if c.procMetrics {
			for _, p := range procRows {
				ch <- prometheus.MustNewConstMetric(c.procMem, prometheus.GaugeValue, p.MemMiB*mib,
					p.GPUIndex, p.UUID, strconv.Itoa(p.PID), p.Name, p.Framework, p.JobID, p.User)
			}
		}
	}

	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, collErr)
}

// querySMI reads the GPU list plus fallback values from nvidia-smi.
func (c *GPUCollector) querySMI() ([]*gpuInfo, error) {
	c.smiMu.Lock()
	throttle := c.smiThrottle
	c.smiMu.Unlock()

	build := func(throttleField string) []string {
		q := []string{"index", "uuid", "name"}
		for _, f := range smiFields {
			if f.Query == "THROTTLE" {
				q = append(q, throttleField)
			} else {
				q = append(q, f.Query)
			}
		}
		return q
	}
	q := build(throttle)
	out, err := runCmd("nvidia-smi", "--query-gpu="+strings.Join(q, ","), "--format=csv,noheader,nounits")
	if err != nil && throttle == "clocks_event_reasons.active" {
		// Older drivers only know the old field name.
		throttle = "clocks_throttle_reasons.active"
		q = build(throttle)
		out, err = runCmd("nvidia-smi", "--query-gpu="+strings.Join(q, ","), "--format=csv,noheader,nounits")
		if err == nil {
			c.smiMu.Lock()
			c.smiThrottle = throttle
			c.smiMu.Unlock()
		}
	}
	if err != nil {
		return nil, err
	}
	return parseSMIGPUs(out, len(q))
}

// parseSMIGPUs parses `nvidia-smi --query-gpu=index,uuid,name,<smiFields>`.
func parseSMIGPUs(out string, nfields int) ([]*gpuInfo, error) {
	var gpus []*gpuInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitCSV(line)
		if len(f) < nfields {
			return nil, fmt.Errorf("unexpected nvidia-smi line: %q", line)
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := &gpuInfo{Index: f[0], UUID: f[1], Name: f[2], vals: map[string]float64{}}
		for i, sf := range smiFields {
			if v, ok := parseNum(f[3+i]); ok {
				g.vals[sf.Metric+"|"+sf.LabelVal] = v * sf.Scale
			}
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

// queryApps lists compute processes on all GPUs.
func queryApps() ([]gpuApp, error) {
	out, err := runCmd("nvidia-smi", "--query-compute-apps=gpu_uuid,pid,process_name,used_memory",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseApps(out), nil
}

func parseApps(out string) []gpuApp {
	var apps []gpuApp
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := splitCSV(line)
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(f[1]))
		if err != nil {
			continue
		}
		mem, _ := parseNum(f[3])
		apps = append(apps, gpuApp{UUID: strings.TrimSpace(f[0]), PID: pid,
			Name: baseName(strings.TrimSpace(f[2])), MemMiB: mem})
	}
	return apps
}

// gpuAssignment is one (GPU, job) pair. JobID "none" = process outside PBS.
type gpuAssignment struct {
	GPUIndex, UUID                         string
	JobID, User, Queue, JobName, Framework string
	FromEnv, FromProc                      bool
}

func (a *gpuAssignment) source() string {
	switch {
	case a.FromEnv && a.FromProc:
		return "both"
	case a.FromEnv:
		return "env"
	default:
		return "proc"
	}
}

// procRow is one GPU process with its job and framework.
type procRow struct {
	GPUIndex, UUID, Name, Framework, JobID, User string
	PID                                          int
	MemMiB                                       float64
}

// mapGPUs merges CUDA_VISIBLE_DEVICES allocation and running processes into
// GPU -> job assignments. scan may be nil (job mapping disabled).
func mapGPUs(gpus []*gpuInfo, apps []gpuApp, scan *jobScan) ([]*gpuAssignment, []procRow) {
	byIndex := map[string]string{}
	indexOf := map[string]string{}
	known := map[string]bool{}
	for _, g := range gpus {
		byIndex[g.Index] = g.UUID
		indexOf[g.UUID] = g.Index
		known[g.UUID] = true
	}
	assign := map[string]*gpuAssignment{}
	var order []string
	get := func(uuid, jobID, user, queue, name string) *gpuAssignment {
		k := uuid + "\x00" + jobID
		if a, ok := assign[k]; ok {
			return a
		}
		a := &gpuAssignment{GPUIndex: indexOf[uuid], UUID: uuid, JobID: jobID, User: user, Queue: queue, JobName: name}
		assign[k] = a
		order = append(order, k)
		return a
	}

	// 1) Allocation from the job's CUDA_VISIBLE_DEVICES (covers idle GPUs).
	if scan != nil {
		ids := make([]string, 0, len(scan.Jobs))
		for id := range scan.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			j := scan.Jobs[id]
			for _, uuid := range visibleDevices(j.CVD, byIndex, known) {
				get(uuid, j.ID, j.User, j.Queue, j.Name).FromEnv = true
			}
		}
	}

	// 2) Running processes.
	jobFW := map[string][]string{}
	fws := map[string][]string{}
	var rows []procRow
	for _, p := range apps {
		if !known[p.UUID] {
			continue
		}
		fw := frameworkForPID(p.PID, p.Name)
		jobID, user, queue, name := "none", userForPID(p.PID), "", ""
		if scan != nil {
			if id, _ := scan.jobForPID(p.PID); id != "" {
				j := scan.Jobs[id]
				jobID, user, queue, name = j.ID, j.User, j.Queue, j.Name
			}
		} else {
			jobID = "unknown"
		}
		if scan != nil {
			a := get(p.UUID, jobID, user, queue, name)
			a.FromProc = true
			k := p.UUID + "\x00" + jobID
			fws[k] = addUnique(fws[k], fw)
			if jobID != "none" {
				jobFW[jobID] = addUnique(jobFW[jobID], fw)
			}
		}
		rows = append(rows, procRow{GPUIndex: indexOf[p.UUID], UUID: p.UUID, Name: p.Name, Framework: fw,
			JobID: jobID, User: user, PID: p.PID, MemMiB: p.MemMiB})
	}

	out := make([]*gpuAssignment, 0, len(order))
	for _, k := range order {
		a := assign[k]
		switch {
		case len(fws[k]) > 0:
			a.Framework = strings.Join(fws[k], ",")
		case len(jobFW[a.JobID]) > 0: // idle GPU of a job: take the job's framework
			a.Framework = strings.Join(jobFW[a.JobID], ",")
		default:
			a.Framework = "none"
		}
		out = append(out, a)
	}
	return out, rows
}

func addUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
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
