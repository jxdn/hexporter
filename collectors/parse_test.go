package collectors

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyFramework(t *testing.T) {
	cases := map[string]string{
		"VLLM::Worker_TP3": "vllm",
		"python -m vllm.entrypoints.openai.api_server --model x": "vllm",
		"ray::RayWorkerWrapper.execute_method":                   "vllm",
		"python3 -m sglang.launch_server --model-path Qwen":      "sglang",
		"text-generation-launcher --model-id x":                  "tgi",
		"tritonserver --model-repository=/models":                "triton",
		"trtllm-serve /engines":                                  "tensorrt-llm",
		"/usr/local/bin/ollama runner --model blob":              "ollama",
		"llama-server -m model.gguf":                             "llama.cpp",
		"python -m ipykernel_launcher -f kernel.json":            "jupyter",
		"torchrun --nproc_per_node=8 train.py":                   "training",
		"python my_script.py":                                    "other",
	}
	for in, want := range cases {
		if got := classifyFramework(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestFrameworkFromParent(t *testing.T) {
	borealisProc(t)
	// pid 4000 is a generic multiprocessing child; its parent runs vLLM.
	if fw := frameworkForPID(4000, "python"); fw != "vllm" {
		t.Errorf("got %q", fw)
	}
}

func TestParseSMIGPUs(t *testing.T) {
	n := 3 + len(smiFields)
	out := "0, GPU-8de81aa8-185e-ead6-148b-f173bdb8576f, NVIDIA B200, 0, 0, 168128, 14, 183359, 33, 190.50, 0, 0, 1965, 3996, 0x0000000000000000\n" +
		"1, GPU-x, NVIDIA B200, [N/A], 0, 1, 1, 2, 30, 180, [N/A], [N/A], 345, 3996, 0x0000000000000004\n"
	gpus, err := parseSMIGPUs(out, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 || gpus[0].Name != "NVIDIA B200" {
		t.Fatalf("gpus = %+v", gpus)
	}
	if v := gpus[0].vals["hpc_gpu_memory_used_bytes|"]; v != 168128*mib {
		t.Errorf("mem used = %v", v)
	}
	if _, ok := gpus[1].vals["hpc_gpu_utilization_percent|"]; ok {
		t.Errorf("[N/A] must be skipped")
	}
	if v := gpus[1].vals["hpc_gpu_throttle_reason|active_mask"]; v != 4 {
		t.Errorf("throttle hex = %v", v)
	}
}

func TestParseApps(t *testing.T) {
	apps := parseApps("GPU-a, 3104943, VLLM::Worker_TP7, 168114\nGPU-b, 6001, /usr/local/bin/ollama, 100\n")
	if len(apps) != 2 || apps[0].PID != 3104943 || apps[1].Name != "ollama" || apps[0].MemMiB != 168114 {
		t.Errorf("apps = %+v", apps)
	}
}

func TestDCGMParsing(t *testing.T) {
	list := `____________________________________________
                    Long Name          Short Name          Field ID
gpu_util                      GPUTL              203
dcgm_fi_prof_sm_active        SMACT              1002
fb_used                       FBUSD              999
`
	names := parseDmonList(list)
	if names["gpu_util"] != 203 || names["prof_sm_active"] != 1002 || names["fb_used"] != 999 {
		t.Fatalf("names = %v", names)
	}
	fields := []gpuField{
		{DCGM: "DCGM_FI_DEV_GPU_UTIL", DefaultID: 203, Metric: "a", Scale: 1},
		{DCGM: "DCGM_FI_DEV_FB_USED", DefaultID: 252, Metric: "b", Scale: mib},          // renumbered -> 999
		{DCGM: "DCGM_FI_PROF_SM_ACTIVE", DefaultID: 1002, Metric: "c", Scale: 1},        // supported
		{DCGM: "DCGM_FI_PROF_PIPE_FP64_ACTIVE", DefaultID: 1006, Metric: "d", Scale: 1}, // not supported
		{DCGM: "DCGM_FI_DEV_XID_ERRORS", DefaultID: 230, Metric: "e", Scale: 1},         // not listed -> default
	}
	res := resolveFields(fields, names, map[int]bool{1002: true})
	var ids []int
	for _, r := range res {
		ids = append(ids, r.id)
	}
	if len(ids) != 4 || ids[0] != 203 || ids[1] != 999 || ids[2] != 1002 || ids[3] != 230 {
		t.Errorf("resolved ids = %v", ids)
	}

	dmon := "#Entity   GPUTL  FBUSD  SMACT  XID\nID\nGPU 0  0  0  N/A  0\nGPU 1  1  1  1  1\n" +
		"GPU 0  12  168128  0.910  48\nGPU 1  N/A  N/A  N/A  N/A\n"
	rows := parseDmonRows(dmon, 4)
	if strings.Join(rows["0"], " ") != "12 168128 0.910 48" {
		t.Errorf("last row GPU 0 = %v", rows["0"])
	}
	if strings.Join(rows["1"], " ") != "N/A N/A N/A N/A" {
		t.Errorf("last row GPU 1 = %v", rows["1"])
	}
	prof := parseProfileList("| A.1 | 1001 | x |\n| A.1 | 1014 | y |\n| 203 |")
	if !prof[1001] || !prof[1014] || prof[203] {
		t.Errorf("profile list = %v", prof)
	}
}

func TestParseNum(t *testing.T) {
	for in, want := range map[string]float64{"12": 12, " 0.5 ": 0.5, "0x10": 16, "4.5e10": 4.5e10} {
		if v, ok := parseNum(in); !ok || v != want {
			t.Errorf("%q -> %v %v", in, v, ok)
		}
	}
	for _, in := range []string{"N/A", "[N/A]", "", "abc"} {
		if _, ok := parseNum(in); ok {
			t.Errorf("%q should fail", in)
		}
	}
}

// borealis-36 mount table (autofs placeholder + real NFS at the same path).
const borealisMounts = `sysfs /sys sysfs rw,nosuid 0 0
tmpfs /dev/shm tmpfs rw 0 0
/dev/nvme0n1p7 / xfs rw,relatime 0 0
/dev/nvme0n1p1 /boot/efi vfat rw 0 0
systemd-1 /nfs/home/svu autofs rw,fd=48 0 0
hpcnas100gsto.nus.edu.sg:/ifs/hpcnas/hpc_home /nfs/home/svu nfs rw,relatime,vers=3,hard,proto=tcp 0 0
hpcnas100gsto.nus.edu.sg:/ifs/hpcnas/svu1 /app1 nfs ro,relatime,vers=3 0 0
hpcnas100gsto2.nus.edu.sg:/ifs/HPCScratch/hpctmp /hpctmp nfs rw,vers=3 0 0
/hpfs-main /scratch dpc rw,relatime 0 0
/dev/sdb1 /data\040disk ext4 rw 0 0
`

const borealisMountInfo = `1 0 259:7 / / rw - xfs /dev/nvme0n1p7 rw
40 1 0:52 /ifs/hpcnas/hpc_home /nfs/home/svu rw - nfs x rw
41 1 0:52 /ifs/hpcnas/svu1 /app1 rw - nfs x rw
42 1 0:53 / /hpctmp rw - nfs x rw
43 1 0:60 / /scratch rw - dpc /hpfs-main rw
`

func TestParseMounts(t *testing.T) {
	devs := parseMountInfoDevs(strings.NewReader(borealisMountInfo))
	ms := parseMounts(strings.NewReader(borealisMounts), devs)
	got := map[string]hostMount{}
	for _, m := range ms {
		got[m.Mountpoint] = m
	}
	if len(ms) != 7 {
		t.Errorf("mounts = %d, want 7: %+v", len(ms), ms)
	}
	if m := got["/scratch"]; m.Class != "parallel" || m.FSType != "dpc" || m.Export != "/hpfs-main" || m.Dev != "0:60" {
		t.Errorf("/scratch = %+v", m)
	}
	if m := got["/nfs/home/svu"]; m.Class != "nfs" || m.Server != "hpcnas100gsto.nus.edu.sg" ||
		m.Export != "/ifs/hpcnas/hpc_home" || m.NFSVers != "3" || m.Dev != "0:52" {
		t.Errorf("/nfs/home/svu = %+v (autofs placeholder must not win)", m)
	}
	if !got["/app1"].ReadOnly || got["/hpctmp"].ReadOnly {
		t.Errorf("readonly detection wrong")
	}
	if _, ok := got["/data disk"]; !ok {
		t.Errorf(`\040 not unescaped`)
	}
	if _, ok := got["/dev/shm"]; ok {
		t.Errorf("tmpfs must be skipped")
	}
	t.Setenv("FS_PARALLEL_TYPES", "ext4")
	if fsClass("ext4") != "parallel" {
		t.Errorf("FS_PARALLEL_TYPES override ignored")
	}
}

const borealisMountStats = `device /dev/nvme0n1p7 mounted on / with fstype xfs
device hpcnas100gsto.nus.edu.sg:/ifs/hpcnas/hpc_home mounted on /nfs/home/svu with fstype nfs statvers=1.1
	opts:	rw,vers=3
	xprt:	tcp 0 1 2 0 0 100 100 0 100 0 2 0 0
	per-op statistics
	        NULL: 1 1 0 44 24 0 0 0 0
	     GETATTR: 5200000 5200010 1 620000000 580000000 900 41000000 43000000 0
	        READ: 380000 380000 0 50000000 30000000000 80 38000000 39000000 0
device hpcnas100gsto.nus.edu.sg:/ifs/hpcnas/svu1 mounted on /app1 with fstype nfs statvers=1.1
	per-op statistics
	     GETATTR: 5200000 5200010 1 620000000 580000000 900 41000000 43000000 0
	        READ: 380000 380000 0 50000000 30000000000 80 38000000 39000000 0
device hpcnas100gsto2.nus.edu.sg:/ifs/HPCScratch/hpctmp mounted on /hpctmp with fstype nfs statvers=1.1
	per-op statistics
	     GETATTR: 9000 9000 0 1 1 1 7700 8000 0
device /hpfs-main mounted on /scratch with fstype dpc
`

func TestParseMountStatsSharedSuperblock(t *testing.T) {
	devs := parseMountInfoDevs(strings.NewReader(borealisMountInfo))
	gs := parseMountStats(strings.NewReader(borealisMountStats), devs, map[string]string{})
	if len(gs) != 2 {
		t.Fatalf("groups = %d, want 2 (svu+app1 share 0:52)", len(gs))
	}
	g := gs[0]
	if g.Dev != "0:52" || strings.Join(g.Mountpoints, ",") != "/nfs/home/svu,/app1" || g.Server != "hpcnas100gsto.nus.edu.sg" {
		t.Errorf("group = %+v", g)
	}
	ga := g.Ops["GETATTR"]
	if ga.Ops != 5200000 || ga.Retrans != 10 || ga.Timeouts != 1 || ga.RTTms != 41000000 || ga.ExecMs != 43000000 {
		t.Errorf("GETATTR counted wrong (double counted?): %+v", ga)
	}
	if strings.Join(g.OpOrder, ",") != "NULL,GETATTR,READ" {
		t.Errorf("ops = %v", g.OpOrder)
	}
	if gs[1].Dev != "0:53" || gs[1].Ops["GETATTR"].Ops != 9000 {
		t.Errorf("hpctmp = %+v", gs[1])
	}
}

func TestStatInflightCountsAsHung(t *testing.T) {
	c := NewDiskCollector()
	c.inflight["/stuck"] = true
	if r := c.statWithTimeout("/stuck"); !r.hung {
		t.Errorf("a path with a stuck statfs must be reported hung without a new call")
	}
	if r := c.statWithTimeout("/"); r.hung || r.err != nil {
		t.Errorf("statfs / = %+v", r)
	}
}

func TestOpenMountFileFallsBackToSelf(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "self", "mountstats"), []byte("self copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_PROC", dir)
	f, err := openMountFile("mountstats") // no 1/mountstats: like a non-root user on bare metal
	if err != nil {
		t.Fatalf("expected fallback to self, got %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "self copy\n" {
		t.Fatalf("read %q", b)
	}
	if _, err := openMountFile("nothere"); err == nil {
		t.Fatal("expected error when neither copy exists")
	}
}

func TestParseAdinfoBorealis(t *testing.T) {
	out := `Local host name:   borealis-36
Joined to domain:  stf.nus.edu.sg
Joined as:         borealis-36-z.stf.nus.edu.sg
Pre-win2K name:    borealis-36-z
Current DC:        stfhssdc02.stf.nus.edu.sg
Preferred site:    Default-First-Site-Name
Zone:              stf.nus.edu.sg/Program Data/Centrify/Zones/SVU
CentrifyDC mode:   connected
Licensed Features: Enabled
`
	a := parseAdinfo(out)
	if a.domain != "stf.nus.edu.sg" || a.zone != "stf.nus.edu.sg/Program Data/Centrify/Zones/SVU" || a.mode != "connected" {
		t.Fatalf("got %+v", a)
	}
	if b := parseAdinfo("CentrifyDC mode:   disconnected\n"); b.mode != "disconnected" || b.domain != "" {
		t.Fatalf("got %+v", b)
	}
}

func TestProcessRunning(t *testing.T) {
	dir := t.TempDir()
	for pid, comm := range map[string]string{"1": "systemd", "4321": "pbs_mom"} {
		os.MkdirAll(filepath.Join(dir, pid), 0o755)
		os.WriteFile(filepath.Join(dir, pid, "comm"), []byte(comm+"\n"), 0o644)
	}
	t.Setenv("HOST_PROC", dir)
	if !processRunning("pbs_mom") || processRunning("pbs_server") {
		t.Fatal("processRunning wrong")
	}
}

func TestLookBinExtraPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "dcgmi-test-only")
	os.WriteFile(bin, []byte("#!/bin/sh\necho hi\n"), 0o755)
	t.Setenv("EXTRA_PATH", dir)
	if p, err := lookBin("dcgmi-test-only"); err != nil || p != bin {
		t.Fatalf("lookBin = %q, %v", p, err)
	}
	if out, err := runCmd("dcgmi-test-only"); err != nil || out != "hi\n" {
		t.Fatalf("runCmd = %q, %v", out, err)
	}
	if _, err := lookBin("no-such-binary-xyz"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestCellValueXIDAndViolation(t *testing.T) {
	var xid, pviol, util *gpuField
	for i := range gpuFields {
		switch gpuFields[i].DefaultID {
		case 230:
			xid = &gpuFields[i]
		case 240:
			pviol = &gpuFields[i]
		case 203:
			util = &gpuFields[i]
		}
	}
	if v, ok := cellValue(xid, "N/A"); !ok || v != 0 {
		t.Errorf("XID N/A = %v,%v want 0,true", v, ok)
	}
	if v, ok := cellValue(xid, "79"); !ok || v != 79 {
		t.Errorf("XID 79 = %v,%v", v, ok)
	}
	if _, ok := cellValue(util, "N/A"); ok {
		t.Error("utilization N/A must stay missing")
	}
	// borealis-36: DCGM 18736324736544 == nvidia-smi 18736324736 us == 18736.3 s
	if v, _ := cellValue(pviol, "18736324736544"); v < 18736 || v > 18737 {
		t.Errorf("power violation = %v s, want ~18736.3", v)
	}
}
