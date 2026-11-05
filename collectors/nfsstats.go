package collectors

import (
	"bufio"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// NFSCollector exposes NFS client RPC counters from HOST_PROC/1/mountstats,
// per RPC operation.
//
// Mounts with the same device id (mountinfo) share one NFS superblock and one
// set of counters (seen on borealis-36: /nfs/home/svu and /app1 on the same
// Isilon). They are reported once, with all their mountpoints in the
// "mountpoints" label, so nothing is counted twice.
type NFSCollector struct {
	ops, retrans, timeouts, rtt, exec *prometheus.Desc
	errorDesc                         *prometheus.Desc
}

func NewNFSCollector() *NFSCollector {
	l := []string{"server", "nfs_dev", "mountpoints", "op"}
	return &NFSCollector{
		ops:      prometheus.NewDesc("hpc_nfs_rpc_ops_total", "NFS RPC operations", l, nil),
		retrans:  prometheus.NewDesc("hpc_nfs_rpc_retransmissions_total", "NFS RPC retransmissions", l, nil),
		timeouts: prometheus.NewDesc("hpc_nfs_rpc_timeouts_total", "NFS RPC major timeouts", l, nil),
		rtt:      prometheus.NewDesc("hpc_nfs_rpc_rtt_milliseconds_total", "NFS RPC round-trip time (network + server)", l, nil),
		exec:     prometheus.NewDesc("hpc_nfs_rpc_execute_milliseconds_total", "NFS RPC execute time (rtt + client queueing)", l, nil),
		errorDesc: prometheus.NewDesc("hpc_collector_error",
			"1 if the collector encountered an error during the last scrape",
			nil, prometheus.Labels{"collector": "nfs"}),
	}
}

func (c *NFSCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.ops, c.retrans, c.timeouts, c.rtt, c.exec, c.errorDesc} {
		ch <- d
	}
}

func (c *NFSCollector) Collect(ch chan<- prometheus.Metric) {
	f, err := openMountFile("mountstats")
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("nfs: mountstats: %v", err)
		ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 1)
		return
	}
	defer f.Close()

	mounts, _ := readHostMounts()
	devs := map[string]string{}
	servers := map[string]string{}
	for _, m := range mounts {
		devs[m.Mountpoint] = m.Dev
		servers[m.Mountpoint] = m.Server
	}
	for _, g := range parseMountStats(f, devs, servers) {
		mps := strings.Join(g.Mountpoints, ",")
		for _, op := range g.OpOrder {
			s := g.Ops[op]
			lv := []string{g.Server, g.Dev, mps, op}
			ch <- prometheus.MustNewConstMetric(c.ops, prometheus.CounterValue, s.Ops, lv...)
			ch <- prometheus.MustNewConstMetric(c.retrans, prometheus.CounterValue, s.Retrans, lv...)
			ch <- prometheus.MustNewConstMetric(c.timeouts, prometheus.CounterValue, s.Timeouts, lv...)
			ch <- prometheus.MustNewConstMetric(c.rtt, prometheus.CounterValue, s.RTTms, lv...)
			ch <- prometheus.MustNewConstMetric(c.exec, prometheus.CounterValue, s.ExecMs, lv...)
		}
	}
	ch <- prometheus.MustNewConstMetric(c.errorDesc, prometheus.GaugeValue, 0)
}

type nfsOpStats struct {
	Ops, Retrans, Timeouts, RTTms, ExecMs float64
}

// nfsGroup is one NFS superblock (one set of counters).
type nfsGroup struct {
	Dev, Server string
	Mountpoints []string
	owner       string // mountpoint whose counters are used
	Ops         map[string]*nfsOpStats
	OpOrder     []string
}

var nfsOpLineRe = regexp.MustCompile(`^\s+([A-Z_0-9]+):\s+(\d.*)$`)

// parseMountStats parses /proc/<pid>/mountstats. devs/servers map mountpoint
// -> device id / server (from the mount table).
func parseMountStats(r io.Reader, devs, servers map[string]string) []*nfsGroup {
	groups := map[string]*nfsGroup{}
	var order []string
	var cur *nfsGroup
	var curMP string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "device ") {
			// device <src> mounted on <mp> with fstype <type> ...
			f := strings.Fields(line)
			cur = nil
			if len(f) < 8 || !strings.HasPrefix(f[7], "nfs") {
				continue
			}
			curMP = unescapeMount(f[4])
			dev := devs[curMP]
			if dev == "" || dev == "-" {
				dev = curMP
			}
			g := groups[dev]
			if g == nil {
				server := servers[curMP]
				if server == "" {
					server = "-"
					if i := strings.Index(f[1], ":/"); i > 0 {
						server = f[1][:i]
					}
				}
				g = &nfsGroup{Dev: dev, Server: server, owner: curMP, Ops: map[string]*nfsOpStats{}}
				groups[dev] = g
				order = append(order, dev)
			}
			g.Mountpoints = append(g.Mountpoints, curMP)
			cur = g
			continue
		}
		if cur == nil || curMP != cur.owner {
			continue // other mountpoints of the same superblock: same counters
		}
		m := nfsOpLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := strings.Fields(m[2])
		if len(v) < 8 {
			continue
		}
		n := make([]float64, 8)
		for i := 0; i < 8; i++ {
			n[i], _ = strconv.ParseFloat(v[i], 64)
		}
		if n[0] == 0 {
			continue
		}
		op := m[1]
		s := cur.Ops[op]
		if s == nil {
			s = &nfsOpStats{}
			cur.Ops[op] = s
			cur.OpOrder = append(cur.OpOrder, op)
		}
		// ops trans timeouts bytes_sent bytes_recv queue rtt execute
		s.Ops += n[0]
		if r := n[1] - n[0]; r > 0 {
			s.Retrans += r
		}
		s.Timeouts += n[2]
		s.RTTms += n[6]
		s.ExecMs += n[7]
	}
	out := make([]*nfsGroup, 0, len(order))
	for _, d := range order {
		out = append(out, groups[d])
	}
	return out
}
