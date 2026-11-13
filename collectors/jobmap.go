package collectors

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// PBS job discovery from the host's /proc.
//
// Every process started by PBS has PBS_JOBID (plus user, queue, job name and,
// with the cgroup hook, CUDA_VISIBLE_DEVICES) in its environment. Some
// children drop that environment (vLLM workers do), so a process can also be
// matched to a job through its PBS cgroup (/pbs_jobs.service/jobs/<token>/)
// or through a parent process that has PBS_JOBID.
//
// Reading another user's /proc/<pid>/environ needs root + CAP_SYS_PTRACE.
// /proc/<pid>/cgroup and /proc/<pid>/status are world-readable.

// pbsJob is one PBS job found on this node.
type pbsJob struct {
	ID    string
	User  string
	Queue string
	Name  string
	CVD   string // CUDA_VISIBLE_DEVICES (UUIDs or indices), "" if unset
	PIDs  []int
}

// jobScan is the result of one /proc scan.
type jobScan struct {
	Jobs       map[string]*pbsJob // job id -> job
	pidJob     map[int]string     // pid -> job id (from the process's own environ)
	cgroupJob  map[string]string  // PBS cgroup token -> job id
	Unreadable int                // processes whose environ could not be read
	Scanned    int
}

var pbsCgroupRe = regexp.MustCompile(`pbs_jobs\.service/jobs/([^/\s]+)`)

// scanJobs walks HOST_PROC and returns all PBS jobs and their processes.
func scanJobs() *jobScan {
	s := &jobScan{
		Jobs:      map[string]*pbsJob{},
		pidJob:    map[int]string{},
		cgroupJob: map[string]string{},
	}
	ents, err := os.ReadDir(procPath(""))
	if err != nil {
		return s
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		env, err := readEnviron(pid)
		if err != nil {
			// A process that exited after ReadDir is not "unreadable": on a
			// busy node a few short-lived processes vanish during every scan.
			if processGone(pid, err) {
				continue
			}
			s.Scanned++
			s.Unreadable++
			continue
		}
		s.Scanned++
		id := env["PBS_JOBID"]
		if id == "" {
			continue
		}
		s.pidJob[pid] = id
		j := s.Jobs[id]
		if j == nil {
			j = &pbsJob{
				ID:    id,
				User:  firstNonEmpty(env["USER"], env["LOGNAME"], env["PBS_O_LOGNAME"]),
				Queue: env["PBS_QUEUE"],
				Name:  env["PBS_JOBNAME"],
			}
			s.Jobs[id] = j
		}
		if j.CVD == "" {
			j.CVD = env["CUDA_VISIBLE_DEVICES"]
		}
		if j.User == "" {
			j.User = "uid:" + procUID(pid)
		}
		j.PIDs = append(j.PIDs, pid)
		if tok := cgroupToken(pid); tok != "" {
			if _, ok := s.cgroupJob[tok]; !ok {
				s.cgroupJob[tok] = id
			}
		}
	}
	return s
}

// jobForPID finds the PBS job of a process. how is "environ", "cgroup",
// "parent" or "" (not a PBS process).
func (s *jobScan) jobForPID(pid int) (jobID, how string) {
	if id, ok := s.pidJob[pid]; ok {
		return id, "environ"
	}
	if tok := cgroupToken(pid); tok != "" {
		if id, ok := s.cgroupJob[tok]; ok {
			return id, "cgroup"
		}
		// Token not seen on a job process: match on the job number,
		// e.g. token "9025.1" -> job "9025.server" or "9025[3].server".
		num := tok
		if i := strings.IndexAny(tok, ".["); i > 0 {
			num = tok[:i]
		}
		for id := range s.Jobs {
			if strings.HasPrefix(id, num+".") || strings.HasPrefix(id, num+"[") {
				return id, "cgroup"
			}
		}
	}
	cur := pid
	for i := 0; i < 64; i++ {
		cur = procPPID(cur)
		if cur <= 1 {
			break
		}
		if id, ok := s.pidJob[cur]; ok {
			return id, "parent"
		}
	}
	return "", ""
}

// userForPID returns USER from the process environment, else "uid:<n>".
func userForPID(pid int) string {
	if env, err := readEnviron(pid); err == nil {
		if u := firstNonEmpty(env["USER"], env["LOGNAME"]); u != "" {
			return u
		}
	}
	return "uid:" + procUID(pid)
}

// readEnviron parses /proc/<pid>/environ (NUL-separated KEY=VALUE).
func readEnviron(pid int) (map[string]string, error) {
	b, err := os.ReadFile(procPath(strconv.Itoa(pid) + "/environ"))
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if k, v, ok := strings.Cut(string(kv), "="); ok {
			m[k] = v
		}
	}
	return m, nil
}

// processGone reports whether a failed /proc/<pid> read means the process
// has exited (ESRCH, or its /proc directory no longer exists) rather than
// that its environ is not readable.
func processGone(pid int, err error) bool {
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	_, serr := os.Stat(procPath(strconv.Itoa(pid)))
	return errors.Is(serr, fs.ErrNotExist)
}

// cgroupToken returns the PBS job token from /proc/<pid>/cgroup
// ("0::/pbs_jobs.service/jobs/9025.1/1" -> "9025.1"), or "".
func cgroupToken(pid int) string {
	b, err := os.ReadFile(procPath(strconv.Itoa(pid) + "/cgroup"))
	if err != nil {
		return ""
	}
	if m := pbsCgroupRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// procStatusField returns a field from /proc/<pid>/status.
func procStatusField(pid int, key string) string {
	f, err := os.Open(procPath(strconv.Itoa(pid) + "/status"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// procPPID returns the parent PID, or 0.
func procPPID(pid int) int {
	n, _ := strconv.Atoi(procStatusField(pid, "PPid"))
	return n
}

// procUID returns the real UID as a string, or "?".
func procUID(pid int) string {
	if f := strings.Fields(procStatusField(pid, "Uid")); len(f) > 0 {
		return f[0]
	}
	return "?"
}

// procComm returns the process name (status Name).
func procComm(pid int) string { return procStatusField(pid, "Name") }

// procCmdline returns the command line (NUL -> space), capped at 512 bytes.
func procCmdline(pid int) string {
	b, err := os.ReadFile(procPath(strconv.Itoa(pid) + "/cmdline"))
	if err != nil {
		return ""
	}
	if len(b) > 512 {
		b = b[:512]
	}
	return strings.TrimSpace(string(bytes.ReplaceAll(b, []byte{0}, []byte{' '})))
}

// visibleDevices splits CUDA_VISIBLE_DEVICES into UUIDs, converting indices
// with byIndex. Unknown entries are dropped.
func visibleDevices(cvd string, byIndex map[string]string, known map[string]bool) []string {
	var out []string
	for _, tok := range strings.Split(cvd, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		uuid := tok
		if !strings.HasPrefix(tok, "GPU-") && !strings.HasPrefix(tok, "MIG-") {
			uuid = byIndex[tok]
		}
		if known[uuid] {
			out = append(out, uuid)
		}
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func baseName(p string) string {
	if strings.Contains(p, "/") {
		return filepath.Base(p)
	}
	return p
}
