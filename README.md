# hexporter (hpc-exporter) v1.0.0

Prometheus exporter for HPC GPU nodes running PBS. On `:9009/metrics` it exposes:

- **GPU → PBS job → user → framework**: which job and user hold each GPU, and what runs on it (vLLM, SGLang, Triton, Ollama, training…). This includes GPUs a job holds while idle, and work started outside PBS.
- **GPU metrics from DCGM** (falls back to nvidia-smi): utilization, memory, power, energy, clocks, XID, ECC, HBM row remapping, PCIe/NVLink errors, power/thermal throttling.
- **DCGM pipe activity**: FP64 / FP32 / FP16 / INT / Tensor Cores (HMMA, IMMA, DFMA), SM active/occupancy, DRAM, PCIe/NVLink bandwidth.
- **Shared filesystems**: NFS, dpc (`/scratch`), Lustre, GPFS… Responsive/hung check with a timeout, read-only, expected mounts, and NFS RPC counters per operation.
- **Node**: CPU, memory, disk I/O, network/InfiniBand, temperatures, EDAC/MCE/OOM, uptime, PBS mom, Centrify.

Node power/SEL is **not** included; use the iDRAC exporter.

All functions, metrics, settings and the roadmap: **[docs/FUNCTIONS.md](docs/FUNCTIONS.md)**. Design notes: [docs/PLAN.md](docs/PLAN.md).

---

## Quick start

```bash
make test            # unit tests
make image-cross     # Apple Silicon / no buildx: compile locally + package amd64 image
# or, on an x86_64 build host:  make image
make package         # dist/hpc-exporter-1.0.0-linux-amd64.tar.gz: binary + setup-hpc-exporter.sh (systemd, no Docker)
make save            # dist/hpc-exporter-1.0.0-amd64.tar.gz: Docker image for nodes without a registry
```

On a GPU node, **systemd (no Docker)**: put `hpc-exporter` and [setup-hpc-exporter.sh](deploy/systemd/setup-hpc-exporter.sh) in one folder, then as root:

```bash
bash setup-hpc-exporter.sh install     # /usr/local/bin + /etc/hpc-exporter/hpc-exporter.env + hpc-exporter.service
bash verify-metrics.sh                 # tools/verify-metrics.sh
```

Or **Docker**:

```bash
gunzip -c hpc-exporter-1.0.0-amd64.tar.gz | docker load
docker compose up -d
```

Step by step: [docs/NODE-TEST.md](docs/NODE-TEST.md).

## Prometheus

```yaml
scrape_configs:
  - job_name: hpc_node
    scrape_interval: 60s
    scrape_timeout: 30s
    static_configs:
      - targets: ['<node>:9009']
```

## Node requirements

| Need | For |
|---|---|
| `nvidia-smi` (NVIDIA driver) | GPU list and processes |
| DCGM 4.x (`dcgmi`), `nv-hostengine` running (`nvidia-dcgm`, or `cuda-dcgm` on Bright) | DCGM values, health and pipe metrics (without it: nvidia-smi fallback, `hpc_gpu_dcgm_up 0`) |
| root (the systemd service runs as root) | Reading every PBS job's environment (job, queue, name, `CUDA_VISIBLE_DEVICES`) and the node's mount statistics |

The exporter only reads. Docker needs more: NVIDIA Container Toolkit, root + `SYS_PTRACE`, `pid: host`, and `/` at `/host/root`; see [docker-compose.yml](docker-compose.yml).

## Main settings

| Variable | Default | |
|---|---|---|
| `DCGM_ENABLED` | `true` | DCGM as GPU source |
| `GPU_JOB_MAPPING` | `true` | GPU → job → user → framework |
| `GPU_PROCESS_METRICS` | `true` | Per-process GPU memory |
| `NFS_ENABLED` | `true` | NFS RPC counters |
| `PORT` | `9009` | Listen port |
| `EXPECTED_MOUNTS` | – | e.g. `/scratch,/nfs/home/svu,/app1,/hpctmp` (`hpc_fs_mounted 0` if missing) |
| `FS_STAT_TIMEOUT` | `5s` | Hung-mount timeout |

Full list: [docs/FUNCTIONS.md §4](docs/FUNCTIONS.md#4-configuration).

## Example queries

```promql
# Utilization per job / user / framework
hpc_gpu_utilization_percent * on(gpu_uuid) group_left(jobid, user, framework) hpc_gpu_job_info
# GPU held by a job but idle for 60 min
max_over_time(hpc_gpu_utilization_percent[60m]) * on(gpu_uuid) group_left(jobid, user) hpc_gpu_job_info == 0
# What each GPU computes with
topk by (gpu_uuid) (1, hpc_gpu_pipe_active_ratio{pipe!="tensor"})
# Hung shared filesystem
hpc_fs_responsive{mount_type=~"nfs|parallel"} == 0
# New XID error
changes(hpc_gpu_last_xid[5m]) > 0
```

## Check a node before deploying

Read-only scripts in [tools/](tools/):

```bash
sudo bash tools/test-gpu-job-map.sh   # GPU → job → user → framework
bash tools/test-shared-fs.sh          # mounts, hung check, NFS stats
bash tools/test-dcgm.sh               # DCGM fields, values, health
```
