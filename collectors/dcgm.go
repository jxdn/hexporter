package collectors

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DCGM is the primary source of GPU values. hexporter runs the `dcgmi` client
// against the host's nv-hostengine (service nvidia-dcgm, DCGM 4.x for B200).
// One `dcgmi dmon` call per scrape reads every field below for all GPUs.

// gpuField maps one DCGM field to a hexporter metric.
type gpuField struct {
	DCGM      string  // DCGM field name, e.g. DCGM_FI_DEV_GPU_UTIL
	DefaultID int     // used when `dcgmi dmon -l` does not list the name
	Metric    string  // hexporter metric name
	LabelKey  string  // optional extra label (pipe / type / reason)
	LabelVal  string  //
	Scale     float64 // DCGM unit -> metric unit
	Help      string
}

func (f *gpuField) counter() bool { return strings.HasSuffix(f.Metric, "_total") }
func (f *gpuField) prof() bool    { return strings.HasPrefix(f.DCGM, "DCGM_FI_PROF_") }
func (f *gpuField) key() string   { return f.Metric + "|" + f.LabelVal }

const mib = 1024 * 1024

var gpuFields = []gpuField{
	// Basics (same metric names as the old nvidia-smi collector)
	{"DCGM_FI_DEV_GPU_UTIL", 203, "hpc_gpu_utilization_percent", "", "", 1, "GPU utilization % (time a kernel was running)"},
	{"DCGM_FI_DEV_MEM_COPY_UTIL", 204, "hpc_gpu_memory_copy_utilization_percent", "", "", 1, "GPU memory copy utilization %"},
	{"DCGM_FI_DEV_FB_TOTAL", 250, "hpc_gpu_memory_total_bytes", "", "", mib, "GPU memory total"},
	{"DCGM_FI_DEV_FB_FREE", 251, "hpc_gpu_memory_free_bytes", "", "", mib, "GPU memory free"},
	{"DCGM_FI_DEV_FB_USED", 252, "hpc_gpu_memory_used_bytes", "", "", mib, "GPU memory used"},
	{"DCGM_FI_DEV_GPU_TEMP", 150, "hpc_gpu_temperature_celsius", "", "", 1, "GPU temperature"},
	{"DCGM_FI_DEV_MEMORY_TEMP", 140, "hpc_gpu_memory_temperature_celsius", "", "", 1, "GPU HBM temperature"},
	{"DCGM_FI_DEV_POWER_USAGE", 155, "hpc_gpu_power_draw_watts", "", "", 1, "GPU power draw"},
	{"DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", 156, "hpc_gpu_energy_joules_total", "", "", 0.001, "GPU energy since driver load"},
	{"DCGM_FI_DEV_SM_CLOCK", 100, "hpc_gpu_clock_sm_mhz", "", "", 1, "GPU SM clock MHz"},
	{"DCGM_FI_DEV_MEM_CLOCK", 101, "hpc_gpu_clock_mem_mhz", "", "", 1, "GPU memory clock MHz"},
	{"DCGM_FI_DEV_CLOCKS_EVENT_REASONS", 112, "hpc_gpu_throttle_reason", "reason", "active_mask", 1, "GPU clock event (throttle) reason bitmask"},
	// Health
	{"DCGM_FI_DEV_POWER_VIOLATION", 240, "hpc_gpu_power_violation_seconds_total", "", "", 1e-9, "Time the GPU was held back by power limits"},
	{"DCGM_FI_DEV_THERMAL_VIOLATION", 241, "hpc_gpu_thermal_violation_seconds_total", "", "", 1e-9, "Time the GPU was held back by thermal limits"},
	{"DCGM_FI_DEV_XID_ERRORS", 230, "hpc_gpu_last_xid", "", "", 1, "Code of the most recent XID error (0 = none)"},
	{"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", 310, "hpc_gpu_ecc_errors_corrected_total", "", "", 1, "Corrected ECC errors since driver load"},
	{"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", 311, "hpc_gpu_ecc_errors_uncorrected_total", "", "", 1, "Uncorrected ECC errors since driver load"},
	{"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS", 394, "hpc_gpu_remapped_rows", "type", "correctable", 1, "HBM rows remapped"},
	{"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", 393, "hpc_gpu_remapped_rows", "type", "uncorrectable", 1, "HBM rows remapped"},
	{"DCGM_FI_DEV_ROW_REMAP_FAILURE", 395, "hpc_gpu_row_remap_failure", "", "", 1, "1 if a row remap failed (GPU needs RMA)"},
	{"DCGM_FI_DEV_PCIE_REPLAY_COUNTER", 202, "hpc_gpu_pcie_replay_total", "", "", 1, "PCIe replay count"},
	{"DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL", 409, "hpc_gpu_nvlink_errors_total", "type", "crc_flit", 1, "NVLink errors"},
	{"DCGM_FI_DEV_NVLINK_CRC_DATA_ERROR_COUNT_TOTAL", 419, "hpc_gpu_nvlink_errors_total", "type", "crc_data", 1, "NVLink errors"},
	{"DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_TOTAL", 429, "hpc_gpu_nvlink_errors_total", "type", "replay", 1, "NVLink errors"},
	{"DCGM_FI_DEV_NVLINK_RECOVERY_ERROR_COUNT_TOTAL", 439, "hpc_gpu_nvlink_errors_total", "type", "recovery", 1, "NVLink errors"},
	// Profiling (pipes / SM / DRAM / links)
	{"DCGM_FI_PROF_GR_ENGINE_ACTIVE", 1001, "hpc_gpu_gr_engine_active_ratio", "", "", 1, "Graphics engine active ratio (0-1)"},
	{"DCGM_FI_PROF_SM_ACTIVE", 1002, "hpc_gpu_sm_active_ratio", "", "", 1, "Ratio of SMs with work (0-1)"},
	{"DCGM_FI_PROF_SM_OCCUPANCY", 1003, "hpc_gpu_sm_occupancy_ratio", "", "", 1, "SM warp occupancy (0-1)"},
	{"DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", 1004, "hpc_gpu_pipe_active_ratio", "pipe", "tensor", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_DRAM_ACTIVE", 1005, "hpc_gpu_dram_active_ratio", "", "", 1, "HBM bandwidth active ratio (0-1)"},
	{"DCGM_FI_PROF_PIPE_FP64_ACTIVE", 1006, "hpc_gpu_pipe_active_ratio", "pipe", "fp64", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_PIPE_FP32_ACTIVE", 1007, "hpc_gpu_pipe_active_ratio", "pipe", "fp32", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_PIPE_FP16_ACTIVE", 1008, "hpc_gpu_pipe_active_ratio", "pipe", "fp16", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_PCIE_TX_BYTES", 1009, "hpc_gpu_pcie_tx_bytes_per_second", "", "", 1, "PCIe TX bytes/s"},
	{"DCGM_FI_PROF_PCIE_RX_BYTES", 1010, "hpc_gpu_pcie_rx_bytes_per_second", "", "", 1, "PCIe RX bytes/s"},
	{"DCGM_FI_PROF_NVLINK_TX_BYTES", 1011, "hpc_gpu_nvlink_tx_bytes_per_second", "", "", 1, "NVLink TX bytes/s"},
	{"DCGM_FI_PROF_NVLINK_RX_BYTES", 1012, "hpc_gpu_nvlink_rx_bytes_per_second", "", "", 1, "NVLink RX bytes/s"},
	{"DCGM_FI_PROF_PIPE_TENSOR_IMMA_ACTIVE", 1013, "hpc_gpu_pipe_active_ratio", "pipe", "tensor_imma", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_PIPE_TENSOR_HMMA_ACTIVE", 1014, "hpc_gpu_pipe_active_ratio", "pipe", "tensor_hmma", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_PIPE_TENSOR_DFMA_ACTIVE", 1015, "hpc_gpu_pipe_active_ratio", "pipe", "tensor_dfma", 1, "Compute pipe active ratio (0-1)"},
	{"DCGM_FI_PROF_PIPE_INT_ACTIVE", 1016, "hpc_gpu_pipe_active_ratio", "pipe", "int", 1, "Compute pipe active ratio (0-1)"},
}

type resolvedField struct {
	f  *gpuField
	id int
}

// dcgmReader resolves field IDs once and reads values each scrape.
type dcgmReader struct {
	mu       sync.Mutex
	fields   []resolvedField
	initErr  error
	nextInit time.Time
}

// dcgmValues: GPU index -> field -> value (already scaled).
type dcgmValues map[string]map[*gpuField]float64

func (r *dcgmReader) ensureInit() error {
	if r.fields != nil {
		return nil
	}
	if time.Now().Before(r.nextInit) {
		return r.initErr
	}
	r.nextInit = time.Now().Add(5 * time.Minute)
	if !binaryExists("dcgmi") {
		r.initErr = fmt.Errorf("dcgmi not found")
		return r.initErr
	}
	listOut, err := runCmdTimeout(15*time.Second, "dcgmi", "dmon", "-l")
	if err != nil {
		r.initErr = fmt.Errorf("dcgmi dmon -l: %v", err)
		return r.initErr
	}
	names := parseDmonList(listOut)
	profOut, perr := runCmdTimeout(15*time.Second, "dcgmi", "profile", "-l", "-i", "0")
	var profOK map[int]bool
	if perr == nil {
		profOK = parseProfileList(profOut)
	} else {
		log.Printf("gpu: dcgmi profile -l failed (%v); profiling fields will be tried anyway", perr)
	}
	r.fields = resolveFields(gpuFields, names, profOK)
	r.initErr = nil
	log.Printf("gpu: DCGM ready, %d fields", len(r.fields))
	return nil
}

// resolveFields picks the ID for each field (by name, else default) and drops
// profiling fields the GPU does not support.
func resolveFields(fields []gpuField, names map[string]int, profOK map[int]bool) []resolvedField {
	var out []resolvedField
	for i := range fields {
		f := &fields[i]
		id := f.DefaultID
		for _, k := range dcgmNameKeys(f.DCGM) {
			if v, ok := names[k]; ok {
				if v != f.DefaultID {
					log.Printf("gpu: %s has id %d on this DCGM (default %d)", f.DCGM, v, f.DefaultID)
				}
				id = v
				break
			}
		}
		if f.prof() && len(profOK) > 0 && !profOK[id] {
			continue
		}
		out = append(out, resolvedField{f, id})
	}
	return out
}

// read returns values for all GPUs. If the full read fails (e.g. profiling
// paused for Nsight), it retries with device fields only.
func (r *dcgmReader) read() (dcgmValues, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureInit(); err != nil {
		return nil, err
	}
	vals, err := dmonRead(r.fields)
	if err == nil {
		return vals, nil
	}
	var dev []resolvedField
	for _, rf := range r.fields {
		if !rf.f.prof() {
			dev = append(dev, rf)
		}
	}
	if len(dev) == len(r.fields) {
		return nil, err
	}
	log.Printf("gpu: dcgmi dmon with profiling fields failed (%v); retrying device fields only", err)
	return dmonRead(dev)
}

func dmonRead(fields []resolvedField) (dcgmValues, error) {
	ids := make([]string, len(fields))
	for i, rf := range fields {
		ids[i] = strconv.Itoa(rf.id)
	}
	// 2 samples 1 s apart: the first sample after a watch starts can be empty.
	out, err := runCmdTimeout(20*time.Second, "dcgmi", "dmon", "-e", strings.Join(ids, ","), "-c", "2", "-d", "1000")
	if err != nil {
		return nil, fmt.Errorf("dcgmi dmon: %v: %s", err, firstLine(out))
	}
	rows := parseDmonRows(out, len(fields))
	if len(rows) == 0 {
		return nil, fmt.Errorf("dcgmi dmon: no GPU rows: %s", firstLine(out))
	}
	vals := dcgmValues{}
	for gpu, cells := range rows {
		m := map[*gpuField]float64{}
		for i, c := range cells {
			if v, ok := cellValue(fields[i].f, c); ok {
				m[fields[i].f] = v
			}
		}
		vals[gpu] = m
	}
	return vals, nil
}

// parseDmonList parses `dcgmi dmon -l`: "<long name> <short name> <field id>".
func parseDmonList(out string) map[string]int {
	m := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		id, err := strconv.Atoi(f[len(f)-1])
		if err != nil {
			continue
		}
		m[normDCGMName(f[0])] = id
	}
	return m
}

// parseProfileList returns the field IDs listed by `dcgmi profile -l`.
func parseProfileList(out string) map[int]bool {
	m := map[int]bool{}
	for _, tok := range strings.FieldsFunc(out, func(r rune) bool { return r == '|' || r == ' ' || r == '\t' || r == '\n' }) {
		if n, err := strconv.Atoi(tok); err == nil && n >= 1000 && n < 2000 {
			m[n] = true
		}
	}
	return m
}

// parseDmonRows keeps the last "GPU <idx> v1 v2 ..." row per GPU.
func parseDmonRows(out string, n int) map[string][]string {
	rows := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "GPU" {
			continue
		}
		cells := f[2:]
		if len(cells) > n {
			cells = cells[:n]
		}
		rows[f[1]] = cells
	}
	return rows
}

func normDCGMName(s string) string {
	s = strings.ToLower(s)
	for _, p := range []string{"dcgm_fi_dev_", "dcgm_fi_", "dev_"} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

// dcgmNameKeys returns the keys a DCGM field name may appear under in dmon -l.
func dcgmNameKeys(name string) []string {
	k := normDCGMName(name)
	keys := []string{k}
	if s := strings.TrimPrefix(k, "prof_"); s != k {
		keys = append(keys, s)
	}
	return keys
}

// naIsZero lists fields where DCGM's N/A means "nothing happened yet":
// XID is N/A until the first XID error since nv-hostengine started.
var naIsZero = map[string]bool{"DCGM_FI_DEV_XID_ERRORS": true}

// cellValue converts one dmon cell to the metric unit.
// Note: on DCGM 4.5 / B200 the violation counters (240, 241) are in
// nanoseconds, not the documented microseconds: they equal nvidia-smi's
// "Clocks Event Reasons Counters" (in us) x 1000. Hence Scale 1e-9.
func cellValue(f *gpuField, cell string) (float64, bool) {
	if v, ok := parseNum(cell); ok {
		return v * f.Scale, true
	}
	if naIsZero[f.DCGM] && strings.Contains(strings.ToUpper(cell), "N/A") {
		return 0, true
	}
	return 0, false
}

// parseNum parses a numeric cell; "N/A", "[N/A]", "" and the like fail.
func parseNum(s string) (float64, bool) {
	s = strings.Trim(strings.TrimSpace(s), "[]")
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 64)
		return float64(v), err == nil
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
