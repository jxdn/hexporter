#!/usr/bin/env bash
# verify-metrics.sh — scrape a running hexporter and check the v1 features.
# Read-only.  Usage:  bash tools/verify-metrics.sh [http://localhost:9009/metrics]
set -u
URL=${1:-http://localhost:9009/metrics}
TMP=$(mktemp); trap 'rm -f "$TMP"' EXIT
pass=0; warn=0; fail=0
ok()   { printf '  PASS  %s\n' "$*"; pass=$((pass+1)); }
wn()   { printf '  WARN  %s\n' "$*"; warn=$((warn+1)); }
bad()  { printf '  FAIL  %s\n' "$*"; fail=$((fail+1)); }
val()  { grep -E "^$1(\{| )" "$TMP" | head -1 | awk '{print $NF}'; }
cnt()  { grep -cE "^$1(\{| )" "$TMP"; }

start=$(date +%s.%N 2>/dev/null || date +%s)
if ! curl -sf --max-time 30 "$URL" > "$TMP"; then echo "FAIL: cannot scrape $URL"; exit 1; fi
end=$(date +%s.%N 2>/dev/null || date +%s)
secs=$(awk -v a="$start" -v b="$end" 'BEGIN{printf "%.1f", b-a}')

echo "== hexporter at $URL"
grep -E '^hpc_exporter_build_info' "$TMP" | sed 's/^/  /'
echo "  scrape took ${secs}s, $(grep -vc '^#' "$TMP") series"
awk -v s="$secs" 'BEGIN{exit !(s < 30)}' && ok "scrape under 30 s timeout" || bad "scrape took ${secs}s (Prometheus timeout is 30 s)"

echo; echo "== Collectors"
while read -r line; do
  c=$(echo "$line" | sed -E 's/.*collector="([^"]+)".*/\1/'); v=$(echo "$line" | awk '{print $NF}')
  [ "$v" = 0 ] && ok "collector $c" || wn "collector $c reports an error (check: journalctl -u hpc-exporter, or the foreground output)"
done < <(grep -E '^hpc_collector_error' "$TMP")

echo; echo "== GPU"
g=$(cnt hpc_gpu_utilization_percent)
[ "$g" -gt 0 ] && ok "$g GPUs reporting utilization" || bad "no GPU metrics (nvidia-smi not in PATH? NVIDIA_ENABLED=false?)"
d=$(val hpc_gpu_dcgm_up)
case "$d" in 1) ok "DCGM up (hpc_gpu_dcgm_up 1)";; 0) wn "DCGM down: nvidia-smi fallback only (is nvidia-dcgm running?)";; *) wn "hpc_gpu_dcgm_up missing";; esac
p=$(cnt hpc_gpu_pipe_active_ratio)
[ "$p" -gt 0 ] && ok "pipe metrics: $p series (FP64/FP32/FP16/Tensor/INT)" || wn "no pipe metrics (DCGM down, or profiling paused/unsupported)"
x=$(cnt hpc_gpu_last_xid); [ "$x" -gt 0 ] && ok "health metrics (XID on $x GPUs)" || wn "no XID metric (needs DCGM)"
grep -E '^hpc_gpu_last_xid' "$TMP" | awk '$NF!=0' | sed 's/^/        XID! /'
grep -E '^hpc_gpu_row_remap_failure' "$TMP" | awk '$NF!=0' | sed 's/^/        REMAP FAILURE! /'

echo; echo "== GPU -> job -> user -> framework"
u=$(val hpc_gpu_jobmap_unreadable_processes)
if [ -z "$u" ]; then wn "job mapping disabled (GPU_JOB_MAPPING=false?)"
elif [ "$u" = 0 ]; then ok "all process environments readable"
else wn "$u processes unreadable: the exporter must run as root (bash setup-hpc-exporter.sh install)"; fi
j=$(cnt hpc_gpu_job_info)
[ "$j" -gt 0 ] && ok "$j GPU->job rows" || wn "no hpc_gpu_job_info rows (no jobs on the node?)"
grep -E '^hpc_gpu_job_info' "$TMP" | sed -E 's/.*framework="([^"]*)".*gpu_index="([^"]*)".*job_name="([^"]*)".*jobid="([^"]*)".*queue="([^"]*)".*source="([^"]*)".*user="([^"]*)".*/\2\t\4\t\7\t\5\t\3\t\1\t\6/' \
  | sort -n | awk -F'\t' 'BEGIN{printf "        %-4s %-22s %-10s %-10s %-14s %-10s %s\n","GPU","JOBID","USER","QUEUE","JOBNAME","FRAMEWORK","SOURCE"}{printf "        %-4s %-22s %-10s %-10s %-14s %-10s %s\n",$1,$2,$3,$4,$5,$6,$7}'
dup=$(grep -E '^hpc_gpu_job_info' "$TMP" | sed -E 's/.*gpu_index="([^"]*)".*/\1/' | sort | uniq -d | tr '\n' ' ')
[ -z "$dup" ] && ok "one job per GPU" || wn "GPUs with more than one row: $dup (shared GPU, or a process not matched to its job)"
grep -qE '^hpc_gpu_job_info\{.*jobid="none"' "$TMP" && wn "GPU work outside PBS (jobid=\"none\") — expected only for ssh/interactive use" || true

echo; echo "== Filesystems"
grep -E '^hpc_fs_info' "$TMP" | grep -E 'mount_type="(nfs|parallel|smb|fuse|other)"' \
  | sed -E 's/.*export="([^"]*)".*fstype="([^"]*)".*mount_type="([^"]*)".*mountpoint="([^"]*)".*server="([^"]*)".*/\4\t\2\t\3\t\5/' \
  | awk -F'\t' '{printf "        %-18s %-6s %-9s %s\n",$1,$2,$3,$4}'
s=$(grep -E '^hpc_fs_info' "$TMP" | grep -cE 'mount_type="(nfs|parallel)"')
[ "$s" -gt 0 ] && ok "$s shared filesystems seen (host mount table)" || bad "no NFS/parallel mounts: on bare metal leave HOST_PROC unset"
nr=$(grep -E '^hpc_fs_responsive' "$TMP" | awk '$NF==0' | sed -E 's/.*mountpoint="([^"]*)".*/\1/' | tr '\n' ' ')
[ -z "$nr" ] && ok "all mounts responding" || wn "not responding: $nr(hung mount, or HOST_ROOT set wrongly)"
grep -E '^hpc_fs_mounted' "$TMP" | while read -r l; do
  mp=$(echo "$l" | sed -E 's/.*mountpoint="([^"]*)".*/\1/'); v=$(echo "$l" | awk '{print $NF}')
  [ "$v" = 1 ] && echo "        expected $mp: mounted" || echo "        expected $mp: MISSING"; done
n=$(grep -E '^hpc_nfs_rpc_ops_total' "$TMP" | sed -E 's/.*mountpoints="([^"]*)".*/\1/' | sort -u)
[ -n "$n" ] && ok "NFS stats for: $(echo $n)" || wn "no NFS RPC stats"

echo; echo "== Other"
[ -n "$(val hpc_system_uptime_seconds)" ] && ok "uptime" || wn "no uptime"
grep -q '^hpc_ipmi_' "$TMP" && bad "IPMI metrics still present" || ok "IPMI removed"
echo; echo "result: $pass pass, $warn warn, $fail fail"
[ "$fail" -eq 0 ]
