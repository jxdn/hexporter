#!/usr/bin/env bash
# test-shared-fs.sh — detect shared filesystems (NFS, dpc, Lustre, GPFS, ...) on a node
# and check that each one responds. Read-only.
#
#   bash test-shared-fs.sh
#   EXPECTED="/scratch,/nfs/home/svu,/app1,/hpctmp,/scratch_aisg" bash test-shared-fs.sh
#
# Env:
#   EXPECTED     comma list of mountpoints that must be mounted (optional)
#   TIMEOUT      seconds before a statfs counts as hung (default 5)
#   PROC_ROOT    /proc of the host (default /proc)
#   ROOT_PREFIX  prefix for host paths, e.g. /host/root inside Docker (default empty)
set -u
PROC=${PROC_ROOT:-/proc}
ROOT=${ROOT_PREFIX:-}
TIMEOUT=${TIMEOUT:-5}
EXPECTED=${EXPECTED:-}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
hr() { printf '\n=== %s ===\n' "$*"; }

# PID 1's mount table = the host's view (/proc/mounts would be our own namespace)
MOUNTS="$PROC/1/mounts"; [ -r "$MOUNTS" ] || MOUNTS="$PROC/mounts"

fsclass() {
  case "$1" in
    nfs|nfs4)                                                         echo nfs ;;
    dpc|lustre|gpfs|beegfs|wekafs|ceph|cephfs|panfs|pvfs2|orangefs|glusterfs|fuse.glusterfs) echo parallel ;;
    cifs|smb3|smbfs)                                                  echo smb ;;
    fuse|fuse.*)                                                      echo fuse ;;
    xfs|ext2|ext3|ext4|btrfs|zfs|vfat|f2fs)                           echo local ;;
    tmpfs|devtmpfs|overlay|cgroup|cgroup2|proc|sysfs|none|devpts|securityfs|pstore|efivarfs|autofs|hugetlbfs|mqueue|debugfs|tracefs|fusectl|configfs|ramfs|squashfs|bpf|rpc_pipefs|binfmt_misc|nsfs|selinuxfs|nfsd) echo skip ;;
    *)                                                                echo other ;;
  esac
}

hr "Node"
echo "host:        $(hostname)"
echo "mount table: $MOUNTS"
[ -n "$ROOT" ] && echo "root prefix: $ROOT"

# ---- 1. Mounts ---------------------------------------------------------------
hr "1. Filesystems (virtual ones skipped)"
: > "$TMP/mounts.tsv"   # mp fstype class source server export ro vers
while read -r src mp fstype opts _; do
  mp=$(printf '%b' "${mp//\\040/ }")
  cls=$(fsclass "$fstype"); [ "$cls" = skip ] && continue
  grep -q "^$mp	" "$TMP/mounts.tsv" 2>/dev/null && continue   # first entry wins
  server=-; export_=$src
  case "$src" in *:/*) server=${src%%:/*}; export_=/${src#*:/};; esac
  ro=rw; case ",$opts," in *,ro,*) ro=ro;; esac
  vers=$(printf '%s' "$opts" | tr ',' '\n' | sed -n 's/^vers=//p' | head -1)
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$mp" "$fstype" "$cls" "$src" "$server" "$export_" "$ro" "${vers:--}" >> "$TMP/mounts.tsv"
done < "$MOUNTS"
awk -F'\t' 'BEGIN{printf "%-18s %-6s %-9s %-28s %-24s %-3s %s\n","MOUNTPOINT","FSTYPE","CLASS","SERVER","EXPORT/DEVICE","RW","NFS_VERS"}
            {printf "%-18s %-6s %-9s %-28s %-24s %-3s %s\n",$1,$2,$3,$5,$6,$7,$8}' "$TMP/mounts.tsv"

# ---- 2. Health: statfs with timeout -----------------------------------------
hr "2. Health check (df with ${TIMEOUT}s timeout; a hung NFS/dpc mount shows HUNG)"
: > "$TMP/health.tsv"   # mp status secs size used avail ipct
while IFS=$'\t' read -r mp fstype cls _; do
  out="$TMP/df.$RANDOM"
  ( df -P -k "$ROOT$mp" 2>/dev/null | tail -1 > "$out"; df -P -i "$ROOT$mp" 2>/dev/null | tail -1 > "$out.i" ) </dev/null >/dev/null 2>&1 &
  pid=$!; ticks=0; max=$((TIMEOUT*10))
  while kill -0 "$pid" 2>/dev/null && [ "$ticks" -lt "$max" ]; do sleep 0.1; ticks=$((ticks+1)); done
  secs=$(awk -v t="$ticks" 'BEGIN{printf "%.1f", t/10}')
  if kill -0 "$pid" 2>/dev/null; then
    disown "$pid" 2>/dev/null   # can't kill a process stuck on a dead mount; leave it
    printf '%s\tHUNG\t>%s\t-\t-\t-\t-\n' "$mp" "$TIMEOUT" >> "$TMP/health.tsv"; continue
  fi
  if [ ! -s "$out" ]; then
    printf '%s\tERROR\t%s\t-\t-\t-\t-\n' "$mp" "$secs" >> "$TMP/health.tsv"; continue
  fi
  read -r _ size used avail _ < "$out"
  ipct=$(awk '{print $5}' "$out.i" 2>/dev/null); [ -z "$ipct" ] && ipct=-
  printf '%s\tOK\t%s\t%s\t%s\t%s\t%s\n' "$mp" "$secs" "$size" "$used" "$avail" "$ipct" >> "$TMP/health.tsv"
done < "$TMP/mounts.tsv"

h() { awk -v k="$1" 'BEGIN{ if(k=="-"){print "-";exit} split("K M G T P E",u," "); v=k; i=1; while(v>=1024 && i<6){v/=1024;i++} printf "%.1f%s", v, u[i] }'; }
printf '%-18s %-9s %-6s %-6s %-9s %-9s %-9s %-5s %s\n' MOUNTPOINT CLASS STATUS SECS SIZE USED AVAIL USE% INODE%
while IFS=$'\t' read -r mp st secs size used avail ipct; do
  cls=$(awk -F'\t' -v m="$mp" '$1==m{print $3}' "$TMP/mounts.tsv")
  pct=-; [ "$size" != - ] && [ "$size" -gt 0 ] && pct=$(awk -v u="$used" -v s="$size" 'BEGIN{printf "%d%%", u*100/s}')
  printf '%-18s %-9s %-6s %-6s %-9s %-9s %-9s %-5s %s\n' "$mp" "$cls" "$st" "$secs" "$(h "$size")" "$(h "$used")" "$(h "$avail")" "$pct" "$ipct"
done < "$TMP/health.tsv"

# ---- 3. Expected mounts ------------------------------------------------------
hr "3. Expected mounts"
if [ -z "$EXPECTED" ]; then
  echo "(set EXPECTED=/scratch,/nfs/home/svu,... to check)"
else
  for mp in $(printf '%s' "$EXPECTED" | tr ',' ' '); do
    line=$(awk -F'\t' -v m="$mp" '$1==m' "$TMP/mounts.tsv")
    if [ -n "$line" ]; then printf '%-18s MOUNTED  (%s)\n' "$mp" "$(printf '%s' "$line" | cut -f2)"
    else printf '%-18s MISSING\n' "$mp"; echo "$mp" >> "$TMP/missing"; fi
  done
fi

# ---- 4. NFS client stats -----------------------------------------------------
hr "4. NFS client stats ($PROC/1/mountstats)"
# Mounts with the same device id (mountinfo field 3) share one NFS superblock,
# and the kernel keeps ONE set of counters for them. Group by device id so
# nothing is counted twice.
MI="$PROC/1/mountinfo"
[ -r "$MI" ] && awk '{print $5"\t"$3}' "$MI" > "$TMP/devmap.tsv" || : > "$TMP/devmap.tsv"
: > "$TMP/nfs.prom"
if [ -r "$PROC/1/mountstats" ]; then
  awk -F'\t' -v P="$TMP/nfs.prom" '
    FNR==1 { f=FILENAME; sub(/.*\//,"",f) }
    f=="devmap.tsv" { dev[$1]=$2; next }
    f=="mounts.tsv" { srv[$1]=$5; next }
    f=="mountstats" {
      split($0, a, /[ \t]+/)
      if ($0 ~ /^device /) { mp=a[5]; nfs=(a[8] ~ /^nfs/); d=""
        if (nfs) { d=(mp in dev)?dev[mp]:mp; if (!(d in mps)) { nd++; D[nd]=d; server[d]=srv[mp] }
                   if (index("," mps[d] ",", "," mp ",")==0) mps[d]=mps[d] (mps[d]?",":"") mp }
        next }
      if (!nfs || d=="" || seen[d,mp]) next
      # per-op line: "  OP: ops trans timeouts bytes_sent bytes_recv queue rtt execute [errors]"
      n=0; for (i=1;i<=length(a);i++) if (a[i]!="") b[++n]=a[i]
      if (n>=9 && b[1] ~ /^[A-Z_0-9]+:$/ && b[2] ~ /^[0-9]+$/) {
        op=substr(b[1],1,length(b[1])-1)
        if (owner[d]=="") owner[d]=mp
        if (owner[d]!=mp) next          # same superblock already counted via another mountpoint
        if (b[2]+0==0) next
        if (!((d,op) in O)) ol[d]=ol[d] op " "
        O[d,op]+=b[2]; T[d,op]+=b[3]-b[2]; TO[d,op]+=b[4]; R[d,op]+=b[8]; E[d,op]+=b[9]
        ops[d]+=b[2]; tr[d]+=b[3]-b[2]; tmo[d]+=b[4]; rtt[d]+=b[8]; exe[d]+=b[9]
      }
    }
    END {
      printf "%-8s %-26s %-28s %-10s %-8s %-8s %-11s %s\n","DEV","SERVER","MOUNTPOINTS","RPC_OPS","RETRANS","TIMEOUT","AVG_RTT_ms","AVG_EXEC_ms"
      for (i=1;i<=nd;i++) { d=D[i]
        printf "%-8s %-26s %-28s %-10d %-8d %-8d %-11.2f %.2f\n", d, server[d], mps[d], ops[d], tr[d], tmo[d], (ops[d]?rtt[d]/ops[d]:0), (ops[d]?exe[d]/ops[d]:0) }
      print "\n(averages are since mount time; the exporter exposes counters so Grafana can rate() them)"
      for (i=1;i<=nd;i++) { d=D[i]; if (!ops[d]) continue
        printf "\nTop operations for %s (%s):\n", mps[d], d
        printf "  %-14s %-10s %-6s %-11s %s\n","OP","OPS","%OPS","AVG_RTT_ms","AVG_EXEC_ms"
        cmd="sort -t\"\t\" -k2,2nr | head -8"
        k=split(ol[d], L, " ")
        for (j=1;j<=k;j++) { op=L[j]; if (op=="") continue
          printf "%s\t%d\t%.1f%%\t%.2f\t%.2f\n", op, O[d,op], O[d,op]*100/ops[d], R[d,op]/O[d,op], E[d,op]/O[d,op] | cmd }
        close(cmd)
        for (j=1;j<=k;j++) { op=L[j]; if (op=="") continue
          lb=sprintf("server=\"%s\",nfs_dev=\"%s\",mountpoints=\"%s\",op=\"%s\"", server[d], d, mps[d], op)
          printf "hpc_nfs_rpc_ops_total{%s} %d\nhpc_nfs_rpc_retransmissions_total{%s} %d\nhpc_nfs_rpc_timeouts_total{%s} %d\nhpc_nfs_rpc_rtt_milliseconds_total{%s} %d\nhpc_nfs_rpc_execute_milliseconds_total{%s} %d\n", lb,O[d,op], lb,T[d,op], lb,TO[d,op], lb,R[d,op], lb,E[d,op] > P }
      }
    }' "$TMP/devmap.tsv" "$TMP/mounts.tsv" "$PROC/1/mountstats" | awk -F'\t' 'NF==5{printf "  %-14s %-10s %-6s %-11s %s\n",$1,$2,$3,$4,$5; next}{print}'
else
  echo "(not readable)"
fi

# ---- 5. Metrics preview ------------------------------------------------------
hr "5. Metrics preview"
while IFS=$'\t' read -r mp fstype cls src server export_ ro vers; do
  dv=$(awk -F'\t' -v m="$mp" '$1==m{print $2; exit}' "$TMP/devmap.tsv"); [ -z "$dv" ] && dv=-
  printf 'hpc_fs_info{mountpoint="%s",fstype="%s",mount_type="%s",server="%s",export="%s",nfs_vers="%s",dev="%s"} 1\n' "$mp" "$fstype" "$cls" "$server" "$export_" "$vers" "$dv"
done < "$TMP/mounts.tsv"
while IFS=$'\t' read -r mp st secs size used avail ipct; do
  cls=$(awk -F'\t' -v m="$mp" '$1==m{print $3}' "$TMP/mounts.tsv")
  ro=$(awk -F'\t' -v m="$mp" '$1==m{print ($7=="ro")?1:0}' "$TMP/mounts.tsv")
  printf 'hpc_fs_responsive{mountpoint="%s",mount_type="%s"} %d\n' "$mp" "$cls" "$([ "$st" = OK ] && echo 1 || echo 0)"
  printf 'hpc_fs_readonly{mountpoint="%s",mount_type="%s"} %s\n' "$mp" "$cls" "$ro"
  [ "$st" = OK ] && printf 'hpc_disk_used_bytes{mountpoint="%s",mount_type="%s"} %s\n' "$mp" "$cls" "$((used*1024))"
done < "$TMP/health.tsv"
if [ -n "$EXPECTED" ]; then
  for mp in $(printf '%s' "$EXPECTED" | tr ',' ' '); do
    v=1; grep -qx "$mp" "$TMP/missing" 2>/dev/null && v=0
    printf 'hpc_fs_mounted{mountpoint="%s"} %d\n' "$mp" "$v"
  done
fi
[ -s "$TMP/nfs.prom" ] && cat "$TMP/nfs.prom"
echo "done."
