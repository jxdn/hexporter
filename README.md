# hpc-exporter

Prometheus exporter for HPC node metrics. Exposes CPU, memory, disk, network, GPU, IPMI, temperature, Centrify AD, and PBS scheduler health on `:9100/metrics`.

---

## Quick start

```bash
# 1. Generate go.sum (only needed once)
go mod tidy

# 2. Build and run with Docker Compose
docker compose up -d --build

# 3. Verify
curl -s http://localhost:9100/metrics | head -30
```

---

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: hpc_node
    static_configs:
      - targets: ['<node-ip>:9100']
    scrape_interval: 30s
    scrape_timeout: 15s
```

---

## Environment variables

| Variable          | Default  | Description                                                   |
|-------------------|----------|---------------------------------------------------------------|
| `PORT`            | `9100`   | TCP port to listen on                                         |
| `HOST_PROC`       | `/proc`  | Path to host's `/proc` (set to `/host/proc` in Docker)        |
| `HOST_SYS`        | `/sys`   | Path to host's `/sys` (set to `/host/sys` in Docker)          |
| `IPMI_ENABLED`    | `true`   | Enable IPMI collector (`false` to disable)                    |
| `NVIDIA_ENABLED`  | `true`   | Enable GPU/nvidia-smi collector (`false` to disable)          |
| `CENTRIFY_ENABLED`| `true`   | Enable Centrify AD collector (`false` to disable)             |
| `PBS_ENABLED`     | `true`   | Enable PBS scheduler collector (`false` to disable)           |

---

## Collectors and privilege requirements

| Collector   | Source                          | Privilege required                        |
|-------------|--------------------------------|-------------------------------------------|
| CPU         | `/proc/stat`, `/proc/cpuinfo`  | None                                      |
| Memory      | `/proc/meminfo`                | None                                      |
| Disk        | `/proc/mounts`, `syscall.Statfs`, `/proc/diskstats` | None            |
| Network     | `/proc/net/dev`                | None; `ibstat`/`perfquery` need IB access |
| GPU         | `nvidia-smi`                   | NVIDIA driver access (`/dev/nvidia*`)     |
| IPMI        | `ipmitool dcmi power reading`  | `SYS_RAWIO` capability                   |
| Temperature | `/sys/class/hwmon/`, `/sys/class/thermal/` | None                        |
| Centrify    | `systemctl`, `adinfo`          | None (adinfo may need domain membership)  |
| PBS         | `systemctl`                    | None                                      |
| Health      | `/sys/devices/system/edac/`, `/proc/vmstat` | None; `ras-mc-ctl` for MCE  |

All collectors are **fault-tolerant**: if a source is unavailable the collector logs
the error, emits `hpc_collector_error{collector="<name>"} 1`, and returns without
crashing the exporter.

---

## Centrify / adinfo

`centrifydc` is not available as a standard Debian package.  The container cannot
install it at build time.  To expose the Centrify metrics:

1. Install Centrify DirectControl on the **host**.
2. The `docker-compose.yml` bind-mounts `/usr/sbin/adinfo` and `/usr/sbin/centrifydc`
   into the container read-only.
3. If the binaries live elsewhere on the host, adjust the volume paths accordingly.
4. Set `CENTRIFY_ENABLED=false` if Centrify is not used.

---

## GPU / NVIDIA

The exporter calls `nvidia-smi` inside the container. Options:

- **NVIDIA Container Toolkit** (recommended): uncomment the `deploy.resources` block in
  `docker-compose.yml`.
- **Manual device mount**: add `/dev/nvidia*` and `/dev/nvidiactl` as devices.
- If `nvidia-smi` is not found the GPU collector silently skips all metrics.

---

## Running without Docker

```bash
go build -o hpc-exporter .
sudo ./hpc-exporter          # sudo needed for ipmitool
# or without IPMI:
IPMI_ENABLED=false ./hpc-exporter
```

---

## Key metrics reference

```
hpc_cpu_usage_percent           – overall CPU busy %
hpc_cpu_iowait_percent          – iowait %
hpc_load_average{period}        – 1m / 5m / 15m load averages
hpc_memory_used_bytes           – RSS + kernel allocations
hpc_disk_read_bytes_total       – cumulative bytes read per device
hpc_disk_io_latency_ms          – delta-based average I/O latency
hpc_network_rx_bytes_total      – bytes received per interface
hpc_ib_port_state               – 1=active, 0=down
hpc_gpu_utilization_percent     – GPU compute utilization
hpc_gpu_memory_used_bytes       – GPU VRAM used
hpc_gpu_ecc_errors_uncorrected_total – uncorrected ECC errors (critical)
hpc_ipmi_power_watts            – instantaneous node power draw
hpc_temperature_celsius         – CPU package / core temperatures
hpc_edac_correctable_errors_total   – correctable DRAM errors per MC
hpc_edac_uncorrectable_errors_total – uncorrectable DRAM errors per MC
hpc_oom_kills_total             – OOM kill events since boot
hpc_centrify_service_up         – Centrify agent health
hpc_pbs_mom_up                  – PBS MOM daemon health
hpc_collector_error{collector}  – 1 if a collector encountered an error
```
