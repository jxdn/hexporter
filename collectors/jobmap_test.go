package collectors

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeProc writes a minimal /proc/<pid> entry.
type fakeProc struct {
	pid, ppid int
	uid       string
	comm      string
	cmdline   string
	env       []string // nil = unreadable environ
	cgroup    string
}

func writeProc(t *testing.T, root string, p fakeProc) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(p.pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Name:\t" + p.comm + "\nPid:\t" + strconv.Itoa(p.pid) + "\nPPid:\t" + strconv.Itoa(p.ppid) +
		"\nUid:\t" + p.uid + "\t" + p.uid + "\t" + p.uid + "\t" + p.uid + "\n"
	must(t, os.WriteFile(filepath.Join(d, "status"), []byte(status), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.ReplaceAll(p.cmdline, " ", "\x00")), 0o644))
	cg := p.cgroup
	if cg == "" {
		cg = "0::/user.slice"
	}
	must(t, os.WriteFile(filepath.Join(d, "cgroup"), []byte(cg+"\n"), 0o644))
	if p.env != nil {
		must(t, os.WriteFile(filepath.Join(d, "environ"), []byte(strings.Join(p.env, "\x00")+"\x00"), 0o644))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// borealis-36: job 9025 holds 8 B200s, vLLM workers have no PBS env.
var borealisUUIDs = []string{
	"GPU-8de81aa8-185e-ead6-148b-f173bdb8576f", "GPU-36f1299f-0307-74fb-78f8-52906b579eb1",
	"GPU-f38a81cc-ce3f-2c81-2918-877837406094", "GPU-a07e2fe1-b1fe-13a3-47a1-8dd1ce86c055",
	"GPU-7b17ea0c-bec5-a927-30d0-6cf7712287eb", "GPU-af2ee800-155b-87b7-1ee7-ad1afdce28be",
	"GPU-f3eb5a9b-7eff-b050-44c0-a39a8ff77136", "GPU-364fb509-a1f3-897d-3012-1625727204a4",
}

const jobCG = "0::/pbs_jobs.service/jobs/9025.1/1"

func borealisProc(t *testing.T) string {
	root := t.TempDir()
	t.Setenv("HOST_PROC", root)
	jobEnv := []string{"PBS_JOBID=9025.vip-ha-pbs", "USER=zidni", "PBS_O_LOGNAME=zidni",
		"PBS_QUEUE=large", "PBS_JOBNAME=glm53", "CUDA_VISIBLE_DEVICES=" + strings.Join(borealisUUIDs, ",")}
	writeProc(t, root, fakeProc{pid: 1, uid: "0", comm: "systemd", cmdline: "/usr/lib/systemd/systemd", env: []string{"PATH=/bin"}})
	writeProc(t, root, fakeProc{pid: 3090264, ppid: 1, uid: "21001", comm: "bash", cmdline: "/bin/bash /var/spool/pbs/mom_priv/jobs/9025.SC", env: jobEnv, cgroup: jobCG})
	writeProc(t, root, fakeProc{pid: 3104444, ppid: 3090264, uid: "21001", comm: "python", cmdline: "python -m vllm.entrypoints.openai.api_server --tensor-parallel-size 8", env: jobEnv, cgroup: jobCG})
	for i := 0; i < 8; i++ {
		writeProc(t, root, fakeProc{pid: 3104937 + i, ppid: 3104444, uid: "21001", comm: "VLLM::Worker_TP" + strconv.Itoa(i),
			cmdline: "VLLM::Worker_TP" + strconv.Itoa(i), env: []string{"USER=zidni"}, cgroup: jobCG})
	}
	// child without env and outside the PBS cgroup -> matched by parent
	writeProc(t, root, fakeProc{pid: 4000, ppid: 3104444, uid: "21001", comm: "python",
		cmdline: "python -c from multiprocessing.spawn import spawn_main", env: []string{}})
	// non-PBS ssh user
	writeProc(t, root, fakeProc{pid: 6001, ppid: 1, uid: "21005", comm: "ollama", cmdline: "/usr/local/bin/ollama runner", env: []string{"USER=dave"}})
	// unreadable environ
	writeProc(t, root, fakeProc{pid: 7001, ppid: 1, uid: "0", comm: "secret", cmdline: "secret"})
	return root
}

func TestScanJobsAndMatching(t *testing.T) {
	borealisProc(t)
	s := scanJobs()

	j := s.Jobs["9025.vip-ha-pbs"]
	if j == nil {
		t.Fatalf("job 9025 not found, jobs=%v", s.Jobs)
	}
	if j.User != "zidni" || j.Queue != "large" || j.Name != "glm53" {
		t.Errorf("job details = %+v", j)
	}
	if s.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", s.Unreadable)
	}

	cases := []struct {
		pid      int
		job, how string
	}{
		{3104444, "9025.vip-ha-pbs", "environ"},
		{3104937, "9025.vip-ha-pbs", "cgroup"}, // vLLM worker, no PBS env
		{4000, "9025.vip-ha-pbs", "parent"},
		{6001, "", ""},
	}
	for _, c := range cases {
		job, how := s.jobForPID(c.pid)
		if job != c.job || how != c.how {
			t.Errorf("pid %d: got (%q,%q) want (%q,%q)", c.pid, job, how, c.job, c.how)
		}
	}
	if u := userForPID(6001); u != "dave" {
		t.Errorf("user 6001 = %q", u)
	}
	if u := userForPID(7001); u != "uid:0" {
		t.Errorf("user 7001 = %q", u)
	}
}

// A process that exits between listing /proc and reading its environ must
// not be counted as unreadable.
func TestScanJobsSkipsExitedProcess(t *testing.T) {
	root := borealisProc(t)
	// Listed by ReadDir, but /proc/<pid> no longer resolves: the process exited.
	must(t, os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "8001")))
	s := scanJobs()
	if s.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (only pid 7001; exited pid 8001 skipped)", s.Unreadable)
	}
	if s.Scanned != 14 {
		t.Errorf("scanned = %d, want 14", s.Scanned)
	}
}

func TestCgroupNumberFallback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOST_PROC", root)
	writeProc(t, root, fakeProc{pid: 10, ppid: 1, uid: "1", comm: "bash", env: []string{"PBS_JOBID=77[3].srv", "USER=a"}, cgroup: "0::/pbs_jobs.service/jobs/77[3].srv/1"})
	writeProc(t, root, fakeProc{pid: 11, ppid: 1, uid: "1", comm: "w", env: []string{}, cgroup: "0::/pbs_jobs.service/jobs/77.9/1"})
	s := scanJobs()
	if job, how := s.jobForPID(11); job != "77[3].srv" || how != "cgroup" {
		t.Errorf("got %q %q", job, how)
	}
}

func TestMapGPUsBorealis(t *testing.T) {
	borealisProc(t)
	var gpus []*gpuInfo
	var apps []gpuApp
	for i, u := range borealisUUIDs {
		gpus = append(gpus, &gpuInfo{Index: strconv.Itoa(i), UUID: u, Name: "NVIDIA B200", vals: map[string]float64{}})
		apps = append(apps, gpuApp{UUID: u, PID: 3104937 + (7 - i), Name: "VLLM::Worker_TP" + strconv.Itoa(7-i), MemMiB: 168114})
	}
	assign, rows := mapGPUs(gpus, apps, scanJobs())
	if len(assign) != 8 {
		t.Fatalf("assignments = %d, want 8 (one per GPU)", len(assign))
	}
	for _, a := range assign {
		if a.JobID != "9025.vip-ha-pbs" || a.User != "zidni" || a.Framework != "vllm" || a.source() != "both" {
			t.Errorf("GPU %s: %+v source=%s", a.GPUIndex, a, a.source())
		}
	}
	if len(rows) != 8 || rows[0].JobID != "9025.vip-ha-pbs" || rows[0].Framework != "vllm" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestMapGPUsIdleAndNonPBS(t *testing.T) {
	borealisProc(t)
	gpus := []*gpuInfo{
		{Index: "0", UUID: borealisUUIDs[0], vals: map[string]float64{}},
		{Index: "7", UUID: borealisUUIDs[7], vals: map[string]float64{}},
		{Index: "9", UUID: "GPU-free", vals: map[string]float64{}},
		{Index: "10", UUID: "GPU-ssh", vals: map[string]float64{}},
	}
	apps := []gpuApp{
		{UUID: borealisUUIDs[0], PID: 3104937, Name: "VLLM::Worker_TP0"},
		{UUID: "GPU-ssh", PID: 6001, Name: "/usr/local/bin/ollama"},
	}
	assign, _ := mapGPUs(gpus, apps, scanJobs())
	got := map[string]*gpuAssignment{}
	for _, a := range assign {
		got[a.GPUIndex] = a
	}
	if a := got["7"]; a == nil || a.source() != "env" || a.Framework != "vllm" {
		t.Errorf("idle allocated GPU 7 = %+v (want env, framework inherited from job)", a)
	}
	if _, ok := got["9"]; ok {
		t.Errorf("free GPU must have no assignment")
	}
	if a := got["10"]; a == nil || a.JobID != "none" || a.User != "dave" || a.Framework != "ollama" {
		t.Errorf("non-PBS GPU = %+v", a)
	}
}

func TestVisibleDevices(t *testing.T) {
	byIndex := map[string]string{"0": "GPU-a", "1": "GPU-b"}
	known := map[string]bool{"GPU-a": true, "GPU-b": true}
	got := visibleDevices("1, GPU-a,GPU-zzz,7", byIndex, known)
	if strings.Join(got, ",") != "GPU-b,GPU-a" {
		t.Errorf("got %v", got)
	}
}
