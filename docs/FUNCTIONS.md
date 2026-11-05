# hexporter v1.0.0: functions reference and roadmap

This file lists everything hexporter v1 does: each collector, each Go function, every metric and every setting. The last section lists the planned future functions.

---

## 1. Architecture

```
                         ┌──────────────── hexporter (:9009/metrics, scrape every 60 s) ───────────────┐
 host /proc, /sys  ──►   │ cpu · memory · temperature · health · network/IB                             │
 host mount table  ──►   │ disk  (statfs via HOST_ROOT, 5 s timeout per mount)  ·  nfs (mountstats)      │
 nvidia-smi        ──►   │ gpu:  GPU list + fallback values · process list                              │
 dcgmi → nv-hostengine ► │ gpu:  all GPU values, health, profiling pipes (FP64/FP32/FP16/Tensor/INT)     │
 /proc/<pid>/environ,    │ gpu:  GPU → PBS job → user → framework                                        │
   cgroup, status   ──►  │                                                                              │
 systemctl / adinfo ──►  │ pbs · centrify                                                               │
                         └──────────────────────────────────────────────────────────────────────────────┘
```

- All collectors run in parallel on each scrape. Every value is read at scrape time; nothing is averaged inside the exporter.
- **Fault-tolerant:** if a source is missing, that collector logs the error, sets `hpc_collector_error{collector="…"} 1`, and the rest keep working.
- **Node power and SEL** come from the iDRAC exporter. IPMI was removed in v1.

---

## 2. Functions by file

### `main.go`
| Function | What it does |
|---|---|
| `main()` | Reads `PORT` (default 9009) and the `*_ENABLED` switches, registers the collectors and `hpc_exporter_build_info`, and serves `/metrics` and `/`. `/metrics` uses `ContinueOnError`: a bad series is logged and dropped, the rest is still served (no HTTP 500). `-version` prints the version. |
| `hpcExporter.Describe / Collect` | Fans out to all collectors concurrently. |
| `enabled(name)` | true unless the env var is `"false"`. |

### `collectors/paths.go`: shared helpers
| Function | What it does |
|---|---|
| `procPath(p)` | `HOST_PROC` + p (host /proc, `/host/proc` in Docker). |
| `sysPath(p)` | `HOST_SYS` + p. |
| `hostPath(p)` | `HOST_ROOT` + p: where a host path (mountpoint) is visible to the exporter. |
| `runCmd(name, args…)` | Runs a binary with a 10 s timeout and returns stdout. |
| `runCmdTimeout(t, name, args…)` | Same, with a custom timeout. |
| `binaryExists(name)` | Whether the binary is on PATH. |
| `envBool / envDuration / envList` | Read boolean / duration (`5` or `5s`) / comma-list env vars. |

### `collectors/gpu.go`: GPU collector
| Function | What it does |
|---|---|
| `NewGPUCollector()` | Builds one descriptor per metric from the field table. Reads `DCGM_ENABLED`, `GPU_JOB_MAPPING`, `GPU_PROCESS_METRICS`. |
| `Collect()` | 1) nvidia-smi GPU list + fallback values → 2) DCGM values override them → 3) emit value metrics → 4) process list → 5) job mapping → `hpc_gpu_job_info`, `hpc_gpu_process_memory_used_bytes`. |
| `querySMI()` | `nvidia-smi --query-gpu=index,uuid,name,…`. Tries `clocks_event_reasons.active` first and falls back to `clocks_throttle_reasons.active` on older drivers. |
| `parseSMIGPUs(out, n)` | Parses that CSV. `[N/A]` values are skipped. |
| `queryApps() / parseApps()` | `nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_memory`. |
| `mapGPUs(gpus, apps, scan)` | Merges **allocation** (job's `CUDA_VISIBLE_DEVICES`, which also catches idle GPUs) with **running processes** into GPU→job assignments. Sets `source` = env / proc / both. An idle GPU of a job takes the job's framework. |
| `gpuAssignment.source()` | env / proc / both. |
| `addUnique`, `splitCSV` | Small helpers. |

### `collectors/dcgm.go`: DCGM reader
| Function | What it does |
|---|---|
| `gpuFields` (table) | 41 DCGM fields → metric, label, unit scale. Covers basics, health and profiling (see §3). |
| `dcgmReader.ensureInit()` | Once per run (retried every 5 min on failure): `dcgmi dmon -l` resolves field IDs **by name**, and `dcgmi profile -l -i 0` lists the profiling fields the GPU supports. |
| `resolveFields(fields, names, profOK)` | Picks the ID for each field (by name, else the default) and drops unsupported profiling fields. |
| `dcgmReader.read()` | One `dcgmi dmon -e <ids> -c 2 -d 1000` per scrape. If it fails (e.g. profiling paused for Nsight), retries with device fields only. |
| `dmonRead(fields)` | Runs dmon and scales the values. |
| `parseDmonList / parseProfileList / parseDmonRows` | Parse dcgmi output; keep the last sample per GPU. |
| `normDCGMName / dcgmNameKeys` | Name normalisation for ID lookup. |
| `parseNum(s)` | Number parser: `N/A` fails, hex `0x…` supported. |

### `collectors/jobmap.go`: PBS job discovery
| Function | What it does |
|---|---|
| `scanJobs()` | Walks `/proc`, reads each environ, and collects jobs (`PBS_JOBID`, user, queue, job name, `CUDA_VISIBLE_DEVICES`), plus each job's PBS cgroup token. Counts unreadable processes. |
| `jobScan.jobForPID(pid)` | Finds a process's job: **environ** → **PBS cgroup** (`/pbs_jobs.service/jobs/9025.1/` → job 9025) → **parent processes** (up to 64 levels). Returns how it matched. |
| `userForPID(pid)` | `USER` from environ, else `uid:<n>`. |
| `readEnviron(pid)` | Parses `/proc/<pid>/environ`. |
| `cgroupToken(pid)` | PBS job token from `/proc/<pid>/cgroup`. |
| `procStatusField / procPPID / procUID / procComm / procCmdline` | Read `/proc/<pid>/status` and `/proc/<pid>/cmdline`. |
| `visibleDevices(cvd, byIndex, known)` | `CUDA_VISIBLE_DEVICES` → GPU UUIDs (accepts UUIDs or indices). |
| `firstNonEmpty`, `baseName` | Small helpers. |

### `collectors/framework.go`: framework detection
| Function | What it does |
|---|---|
| `frameworkRules` (table) | vllm, sglang, tgi, triton, tensorrt-llm, lmdeploy, ollama, llama.cpp, deepspeed-mii, ray-serve, jupyter, training. First match wins. |
| `classifyFramework(text)` | Matches text against the rules, else `other`. |
| `frameworkForPID(pid, smiName)` | Checks the nvidia-smi name, process name and cmdline, then up to 5 parents (for generic `python` spawn children). |

### `collectors/mounts.go`: host mount table
| Function | What it does |
|---|---|
| `fsClass(fstype)` | `local / nfs / parallel / smb / fuse / other / skip`. **dpc**, lustre, gpfs, beegfs, weka, ceph… count as parallel. `FS_PARALLEL_TYPES` adds more. |
| `readHostMounts()` | Reads `HOST_PROC/1/mounts`, the **host's** view, not the container's. Adds device IDs from `1/mountinfo`. |
| `parseMounts(r, devs)` | Skips virtual filesystems. A real mount beats an autofs placeholder. Splits `server:/export`, and reads `ro` and `vers=`. |
| `parseMountInfoDevs(r)` | mountpoint → `major:minor`. NFS mounts with the same device share one superblock. |
| `unescapeMount(s)` | Decodes `\040` (space) and similar. |

### `collectors/disk.go`: filesystems and block devices
| Function | What it does |
|---|---|
| `NewDiskCollector()` | Reads `FS_STAT_TIMEOUT` (default 5 s) and `EXPECTED_MOUNTS`. |
| `statWithTimeout(path)` | statfs in a goroutine with a timeout. A hung NFS/dpc mount gives `hpc_fs_responsive 0` instead of freezing the scrape. No new statfs is started on a path whose previous call is still stuck. |
| `Collect()` | Runs statfs on all mounts in parallel → size, inode and `hpc_fs_*` metrics. Checks expected mounts. Block I/O from `/proc/diskstats`. |
| `parseDiskStats()`, `isPartition(dev)` | Block-device counters for whole disks only. |

### `collectors/nfsstats.go`: NFS client
| Function | What it does |
|---|---|
| `Collect()` | Emits per-RPC-operation counters for each NFS superblock. |
| `parseMountStats(r, devs, servers)` | Parses `HOST_PROC/1/mountstats` and **groups mountpoints that share a superblock**, so `/nfs/home/svu` + `/app1` are counted once. Retransmissions = trans − ops. |

### `collectors/health.go`
| Function | What it does |
|---|---|
| `Collect()` | Uptime, EDAC CE/UE per memory controller, OOM kills, MCE (rasdaemon). |
| `readUptime / readOOMKills / readMCECount / readUintSysFile` | Readers. |

### Unchanged collectors
| File | Functions |
|---|---|
| `cpu.go` | `readCPUStat`, `countProcesses`, `countCPUCores`, `collectSysfsFreqs`, `collectCPUInfoFreqs`, `readLoadAvg` |
| `memory.go` | `parseMemInfo` |
| `network.go` | `parseNetDev`, `ifaceType`, `collectIBStat`, `collectPerfQuery`, `ibCADevices` |
| `temperature.go` | `collectHwmon`, `hwmonDevice` (v1: `device` label = PCI address or platform device, so same-named chips such as several `mlx5` NICs or one `coretemp` per socket stay apart), `collectThermalZones`, `readSysFile`, `sanitizeLabel` |
| `pbs.go` | `serviceActive` (pbs unit), `processRunning` (v1: `pbs_mom` process; PBS Pro has no pbs_mom unit) |
| `centrify.go` | Centrify service; `parseAdinfo` (v1: plain `adinfo`: joined domain, zone, `CentrifyDC mode: connected` = DC reachable; no `--test` port probe) |

---

## 3. Metrics

### GPU values (labels `gpu_index, gpu_name, gpu_uuid` + extra where shown). Source: DCGM, with nvidia-smi as fallback
| Metric | Extra label | Notes |
|---|---|---|
| `hpc_gpu_utilization_percent` | | |
| `hpc_gpu_memory_copy_utilization_percent` | | |
| `hpc_gpu_memory_{total,free,used}_bytes` | | |
| `hpc_gpu_temperature_celsius`, `hpc_gpu_memory_temperature_celsius` | | HBM temp: DCGM only |
| `hpc_gpu_power_draw_watts` | | |
| `hpc_gpu_energy_joules_total` | | DCGM only |
| `hpc_gpu_clock_sm_mhz`, `hpc_gpu_clock_mem_mhz` | | |
| `hpc_gpu_throttle_reason` | `reason="active_mask"` | bitmask |
| `hpc_gpu_ecc_errors_{corrected,uncorrected}_total` | | |
| `hpc_gpu_power_violation_seconds_total`, `hpc_gpu_thermal_violation_seconds_total` | | DCGM only. DCGM 4.5 reports these in ns (not the documented µs); checked against nvidia-smi on borealis-36 |
| `hpc_gpu_last_xid` | | DCGM only; 0 = no XID since nv-hostengine started (DCGM shows N/A) |
| `hpc_gpu_remapped_rows` | `type=correctable\|uncorrectable` | DCGM only |
| `hpc_gpu_row_remap_failure` | | DCGM only |
| `hpc_gpu_pcie_replay_total` | | DCGM only |
| `hpc_gpu_nvlink_errors_total` | `type=crc_flit\|crc_data\|replay\|recovery` | DCGM only. N/A on B200 with DCGM 4.5 (series absent) |
| `hpc_gpu_gr_engine_active_ratio`, `hpc_gpu_sm_active_ratio`, `hpc_gpu_sm_occupancy_ratio`, `hpc_gpu_dram_active_ratio` | | DCGM profiling, 0–1 |
| `hpc_gpu_pipe_active_ratio` | `pipe=fp64\|fp32\|fp16\|int\|tensor\|tensor_hmma\|tensor_imma\|tensor_dfma` | DCGM profiling, 0–1 |
| `hpc_gpu_{pcie,nvlink}_{tx,rx}_bytes_per_second` | | DCGM profiling |

### GPU jobs and processes
| Metric | Labels |
|---|---|
| `hpc_gpu_job_info` (1) | `gpu_index, gpu_uuid, jobid, user, queue, job_name, framework, source` |
| `hpc_gpu_process_memory_used_bytes` | `gpu_index, gpu_uuid, pid, process_name, framework, jobid, user` |
| `hpc_gpu_jobmap_unreadable_processes` | (above 0 = missing root/SYS_PTRACE) |
| `hpc_gpu_dcgm_up` | 1 = DCGM, 0 = nvidia-smi fallback |

### Filesystems
| Metric | Labels |
|---|---|
| `hpc_disk_{total,used,free}_bytes`, `hpc_disk_inodes_{total,used,free}` | `mountpoint, device, fstype, mount_type` |
| `hpc_fs_info` (1) | `mountpoint, fstype, mount_type, server, export, nfs_vers, dev` |
| `hpc_fs_responsive` | `mountpoint, mount_type` |
| `hpc_fs_stat_duration_seconds` | `mountpoint` |
| `hpc_fs_readonly` | `mountpoint, mount_type` |
| `hpc_fs_mounted` | `mountpoint` (EXPECTED_MOUNTS only) |
| `hpc_nfs_rpc_{ops,retransmissions,timeouts}_total`, `hpc_nfs_rpc_{rtt,execute}_milliseconds_total` | `server, nfs_dev, mountpoints, op` |
| `hpc_disk_{read,write}_{iops,bytes}_total`, `hpc_disk_io_queue_depth`, `hpc_disk_io_latency_ms` | `device` |

### Node (unchanged from before v1)
`hpc_cpu_usage_percent`, `hpc_cpu_iowait_percent`, `hpc_cpu_steal_percent`, `hpc_cpu_cores_total`, `hpc_cpu_frequency_mhz`, `hpc_load_average{period}`, `hpc_processes_total`, `hpc_memory_*_bytes`, `hpc_swap_*_bytes`, `hpc_network_*_total`, `hpc_ib_port_*`, `hpc_temperature_celsius{sensor,chip,device,type}`, `hpc_thermal_zone_celsius`, `hpc_edac_*_errors_total`, `hpc_oom_kills_total`, `hpc_mce_errors_total`, `hpc_system_uptime_seconds`, `hpc_centrify_*`, `hpc_pbs_service_up`, `hpc_pbs_mom_up`, `hpc_collector_error{collector}`, `hpc_exporter_build_info{version}`.

**Removed in v1:** `hpc_ipmi_power_watts`, `hpc_ipmi_sel_entries_total` (use the iDRAC exporter).

---

## 4. Configuration

| Env var | Default | Purpose |
|---|---|---|
| `PORT` | `9009` | Listen port |
| `HOST_PROC` / `HOST_SYS` / `HOST_ROOT` | `/proc` / `/sys` / `` | Host paths (`/host/proc`, `/host/sys`, `/host/root` in Docker) |
| `NVIDIA_ENABLED` | `true` | GPU collector |
| `DCGM_ENABLED` | `true` | Use DCGM (falls back to nvidia-smi if it's down) |
| `GPU_JOB_MAPPING` | `true` | GPU → job → user mapping (needs root + SYS_PTRACE) |
| `GPU_PROCESS_METRICS` | `true` | Per-process GPU memory series |
| `NFS_ENABLED` | `true` | NFS RPC counters |
| `FS_STAT_TIMEOUT` | `5s` | statfs timeout per mount |
| `EXPECTED_MOUNTS` | – | e.g. `/scratch,/nfs/home/svu,/app1,/hpctmp[,/scratch_aisg]` |
| `FS_PARALLEL_TYPES` | – | Extra fstypes to treat as parallel |
| `EXTRA_PATH` | – | Extra folders (colon-separated) to find `dcgmi`, `nvidia-smi`, `adinfo` in. Always searched after PATH: `/cm/local/apps/cuda-dcgm/current/bin`, `/usr/local/dcgm/bin`, `/opt/pbs/bin`, `/usr/sbin`, `/sbin` |
| `CENTRIFY_ENABLED`, `PBS_ENABLED` | `true` | Optional collectors |

---

## 5. Build and run

```bash
make test        # unit tests
make package     # bare metal: dist/hpc-exporter-1.0.0-linux-amd64.tar.gz (binary + setup-hpc-exporter.sh)
make image-cross && make save   # Docker image tarball (Apple Silicon build host)

# on the node (root), with hpc-exporter next to the script
bash setup-hpc-exporter.sh install     # or: status | uninstall
```

`deploy/systemd/setup-hpc-exporter.sh` holds the systemd unit and the default settings:

| Installs | |
|---|---|
| `/usr/local/bin/hpc-exporter` | copied and checked with `-version` before the old one is replaced |
| `/etc/hpc-exporter/hpc-exporter.env` | settings (section 4); first install only, takes `PORT`, `EXPECTED_MOUNTS`, `FS_STAT_TIMEOUT` from the command line; on upgrade the new defaults go to `.env.new` |
| `/etc/systemd/system/hpc-exporter.service` | root, `Nice=10`, idle I/O, `MemoryMax=512M`, `TasksMax=256`, `NoNewPrivileges`; no private mount namespace, so host NFS/dpc/autofs mounts are seen as they are; PATH includes `/opt/pbs/bin` |

Prometheus: `scrape_interval: 60s`, `scrape_timeout: 30s`. A scrape takes about 2–3 s with DCGM, which waits for 2 samples 1 s apart.

The **node** needs the NVIDIA driver (`nvidia-smi`), and DCGM 4.x with `nv-hostengine` running (`nvidia-dcgm`, or `cuda-dcgm` on Bright) for DCGM metrics. It runs as root. With Docker it also needs the NVIDIA Container Toolkit; the container runs as root with `SYS_PTRACE`, `pid: host` and `network_mode: host`, and all host mounts are read-only.

Test scripts (read-only, run on a node before deploying) are in `tools/`: `test-gpu-job-map.sh`, `test-shared-fs.sh`, `test-dcgm.sh`.

---

## 6. Future functions (roadmap)

| # | Function | Why | Effort |
|---|---|---|---|
| 1 | **PBS job details via `qstat -f -F json`** (cached 60 s): `project`, `walltime`, `ngpus`, `start_time` | Group usage by project (e.g. `CFP05-SF-000`) and check walltime | S |
| 2 | **Grafana dashboards + alert rules + recording rules** in the repo (`deploy/`) | GPU per job/user, idle 60 min, XID/ECC, hung mounts, NFS latency | M |
| 3 | **Per-job GPU summary at job end** (avg util, peak mem, GPU-hours, energy, main pipe) → CSV/DB | History beyond Prometheus retention; usage reports | M |
| 4 | **go-dcgm / NVML bindings** instead of exec | One call, no 2 s dmon wait, per-process SM utilization | M (cgo) |
| 5 | **MIG support**: `gpu_instance_id` / `mig_uuid` labels | If any node partitions B200s | S |
| 6 | **Framework rules from a file** (`GPU_FRAMEWORK_RULES`) | Add new inference engines without a rebuild | S |
| 7 | **dpc client stats** (Huawei OceanStor DPC tools), like NFS mountstats | Latency/throughput for `/scratch` | M |
| 8 | **Optional PBS hook** writing a GPU→job map file | Allocation info without root/SYS_PTRACE | S |
| 9 | **Lustre/GPFS/BeeGFS client stats** | Other clusters | M |
| 10 | **Process GPU utilization** (`nvmlDeviceGetProcessUtilization`) | Who is busy when several jobs share a GPU | S (with #4) |
| 11 | **Multi-node job view**: label jobs spanning nodes (`exec_host`) | Whole-job efficiency for TP/PP across nodes | S (with #1) |
| 12 | **Self-metrics**: per-collector scrape duration | Spot slow collectors (dcgmi, hung NFS) | S |
| 13 | **arm64 (Grace) image** | GB200 nodes | S |
| 14 | **TLS/basic auth on /metrics** (exporter-toolkit) | If the network isn't trusted | S |
