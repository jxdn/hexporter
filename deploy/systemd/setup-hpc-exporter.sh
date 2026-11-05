#!/usr/bin/env bash
# setup-hpc-exporter.sh: install hexporter (hpc-exporter) as a systemd service.
#
# Put this script and the hpc-exporter binary in the same folder, then as root:
#   bash setup-hpc-exporter.sh install     # install + enable + start (default)
#   bash setup-hpc-exporter.sh status      # service state + quick metrics check
#   bash setup-hpc-exporter.sh uninstall   # stop + remove (keeps /etc/hpc-exporter)
#
# Node-specific settings can be given on the command line the first time:
#   PORT=9019 EXPECTED_MOUNTS=/scratch,/scratch_aisg,/nfs/home/svu,/app1,/hpctmp \
#     bash setup-hpc-exporter.sh install
# Later, edit /etc/hpc-exporter/hpc-exporter.env and: systemctl restart hpc-exporter
#
# Installs:
#   /usr/local/bin/hpc-exporter
#   /etc/hpc-exporter/hpc-exporter.env      (kept if it already exists)
#   /etc/systemd/system/hpc-exporter.service
set -eu

NAME=hpc-exporter
BIN=/usr/local/bin/hpc-exporter
CONF_DIR=/etc/hpc-exporter
CONF=$CONF_DIR/hpc-exporter.env
UNIT=/etc/systemd/system/hpc-exporter.service
HERE=$(cd "$(dirname "$0")" && pwd)

die() { echo "ERROR: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root"
command -v systemctl >/dev/null || die "systemd not found"

write_env() {
  cat > "$1" <<EOF
# hexporter settings. Restart after changes: systemctl restart hpc-exporter
PORT=${PORT:-9009}

# Optional collectors
NVIDIA_ENABLED=true
NFS_ENABLED=true
PBS_ENABLED=true
CENTRIFY_ENABLED=true

# GPU: DCGM values + pipes (falls back to nvidia-smi), GPU -> PBS job -> user -> framework
DCGM_ENABLED=true
GPU_JOB_MAPPING=true
GPU_PROCESS_METRICS=true

# Filesystems: hung-mount timeout, and mounts that must exist (hpc_fs_mounted=0 if missing)
FS_STAT_TIMEOUT=${FS_STAT_TIMEOUT:-5s}
EXPECTED_MOUNTS=${EXPECTED_MOUNTS:-/scratch,/nfs/home/svu,/app1,/hpctmp}
#FS_PARALLEL_TYPES=dpc,lustre

# Extra folders to find dcgmi/nvidia-smi/adinfo in (colon-separated)
#EXTRA_PATH=/opt/nvidia/dcgm/bin
EOF
}

write_unit() {
  cat > "$1" <<'EOF'
[Unit]
Description=hexporter (hpc-exporter): Prometheus exporter for HPC GPU nodes
Documentation=https://github.com/jxdn/hexporter
# DCGM is optional: without it the exporter uses nvidia-smi and retries DCGM every 5 min.
After=network-online.target remote-fs.target nvidia-dcgm.service cuda-dcgm.service pbs.service
Wants=network-online.target

[Service]
Type=simple
# root: reads every job's /proc/<pid>/environ (PBS job, queue, name,
# CUDA_VISIBLE_DEVICES) and the node's mount statistics. Read-only.
User=root
EnvironmentFile=-/etc/hpc-exporter/hpc-exporter.env
# dcgmi (Bright: /cm/local/apps/cuda-dcgm/current/bin), nvidia-smi, ibstat, adinfo, ras-mc-ctl
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/pbs/bin:/cm/local/apps/cuda-dcgm/current/bin
ExecStart=/usr/local/bin/hpc-exporter
Restart=on-failure
RestartSec=10s
# A statfs stuck on a hung NFS mount cannot be killed; don't wait forever on stop.
TimeoutStopSec=20s

# Low priority, bounded memory and threads (each hung statfs holds one thread).
Nice=10
IOSchedulingClass=idle
MemoryMax=512M
TasksMax=256

# Hardening that keeps the host mount namespace. ProtectSystem/ProtectHome/
# PrivateTmp are left out on purpose: they create a private mount namespace,
# and the exporter must see and statfs the node's NFS/dpc/autofs mounts as they are.
NoNewPrivileges=yes
LockPersonality=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
EOF
}

conf_port() {
  local p=""
  [ -f "$CONF" ] && p=$(sed -n 's/^PORT=["]*\([0-9]*\).*/\1/p' "$CONF" | tail -1)
  echo "${p:-${PORT:-9009}}"
}

check() {
  local port url tmp n
  port=$(conf_port); url="http://localhost:$port/metrics"
  tmp=$(mktemp)
  if ! command -v curl >/dev/null; then echo "curl not found; skip metrics check"; rm -f "$tmp"; return 0; fi
  # Retry only while the port is not open yet (code 000); one scrape takes ~3 s.
  local code=000 t0 t1
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    t0=$(date +%s)
    code=$(curl -s -o "$tmp" -w '%{http_code}' --max-time 30 "$url" || true)
    t1=$(date +%s)
    [ "$code" != 000 ] && break
    sleep 1
  done
  if [ "$code" = 000 ]; then
    rm -f "$tmp"
    echo "no answer from $url; check: journalctl -u $NAME -n 50"
    return 1
  fi
  if [ "$code" != 200 ]; then
    echo "HTTP $code from $url:"
    head -c 1500 "$tmp" | sed 's/^/  /'; echo
    rm -f "$tmp"
    echo "the service keeps running; logs: journalctl -u $NAME -n 50"
    return 1
  fi
  echo "metrics at $url (scrape $((t1 - t0)) s):"
  grep '^hpc_exporter_build_info' "$tmp" | sed 's/^/  /'
  n=$(grep -c '^hpc_gpu_utilization_percent' "$tmp" || true); echo "  GPUs:            $n"
  echo "  DCGM up:         $(grep '^hpc_gpu_dcgm_up' "$tmp" | awk '{print $NF}')"
  n=$(grep -c '^hpc_gpu_job_info' "$tmp" || true);           echo "  GPU->job rows:   $n"
  echo "  unreadable proc: $(grep '^hpc_gpu_jobmap_unreadable_processes' "$tmp" | awk '{print $NF}')"
  n=$(grep '^hpc_fs_info' "$tmp" | grep -cE 'mount_type="(nfs|parallel)"' || true); echo "  shared fs:       $n"
  grep -E '^hpc_collector_error' "$tmp" | awk '$NF!=0' | sed 's/^/  ERROR /'
  grep -E '^hpc_fs_mounted' "$tmp" | awk '$NF==0' | sed 's/^/  MISSING /'
  rm -f "$tmp"
  [ -f "$HERE/verify-metrics.sh" ] && echo "full check: bash $HERE/verify-metrics.sh $url"
  return 0
}

install_svc() {
  [ -r "$HERE/hpc-exporter" ] || die "$HERE/hpc-exporter not found or not readable by root (NFS root squash? chmod 755 it, or copy both files to /tmp)"
  local port ver
  port=$(conf_port)
  if ! systemctl is-active -q "$NAME" && ss -ltn 2>/dev/null | grep -q ":$port "; then
    die "port $port is in use; rerun with PORT=<free port> bash $0 install"
  fi

  # Copy to local disk first and check it runs there (home may be noexec NFS).
  install -m 0755 "$HERE/hpc-exporter" "$BIN.new"
  ver=$("$BIN.new" -version 2>&1) || { rm -f "$BIN.new"; die "binary does not run on this node: $ver"; }
  echo "installing $ver"

  if systemctl is-active -q "$NAME"; then systemctl stop "$NAME"; fi
  mv -f "$BIN.new" "$BIN"
  install -d -m 0755 "$CONF_DIR"
  if [ -f "$CONF" ]; then
    write_env "$CONF.new"; chmod 0644 "$CONF.new"
    echo "kept $CONF (defaults for this version in $CONF.new)"
  else
    write_env "$CONF"; chmod 0644 "$CONF"
    echo "wrote $CONF"
  fi
  write_unit "$UNIT"; chmod 0644 "$UNIT"
  echo "wrote $UNIT"
  systemctl daemon-reload
  systemctl enable "$NAME" >/dev/null 2>&1
  systemctl restart "$NAME"
  sleep 2
  systemctl is-active -q "$NAME" || { journalctl -u "$NAME" -n 20 --no-pager; die "service did not start"; }
  echo "service $NAME: active, enabled at boot"
  echo
  check
}

uninstall_svc() {
  systemctl disable --now "$NAME" 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  systemctl daemon-reload
  echo "removed $NAME service and $BIN; settings kept in $CONF_DIR"
}

status_svc() {
  systemctl --no-pager --lines=5 status "$NAME" || true
  echo
  check
}

case "${1:-install}" in
  install)   install_svc ;;
  uninstall) uninstall_svc ;;
  status)    status_svc ;;
  *) echo "usage: bash $0 [install|status|uninstall]" >&2; exit 2 ;;
esac
