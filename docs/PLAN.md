# hexporter: GPU → job → user (revised plan)

## Scope

- Each scrape, the exporter reads the PBS jobs **currently running on the node** and works out which GPU belongs to which job.
- It exports GPU utilization together with that mapping: GPU, job ID, user, queue, job name, and the **framework** running on the GPU (vllm, sglang, triton, …).
- **Idle detection is done in Grafana, not in the exporter.** A GPU counts as idle when it stays at 0% for 60 minutes.
- The exporter keeps running in **Docker**.
- **Out of scope for now:** PBS hooks, per-job history, NVML, dashboards and alerts.

## Scrape interval: 60 s for everything

```yaml
scrape_configs:
  - job_name: hpc_node
    scrape_interval: 60s
    scrape_timeout: 30s        # well above the 5 s per-mount statfs timeout
    static_configs:
      - targets: ['<node>:9009']
```

- Every collector gives a **real-time read** at scrape time. There is no sampling or averaging inside the exporter.
- **GPU utilization** is what `nvidia-smi` reports at that moment, so you get one point per minute per GPU. The Grafana idle rule `max_over_time(...[60m]) == 0` checks 60 points: any nonzero reading in the hour means the GPU isn't idle.
- **Counters** (NFS RPC, disk I/O, ECC): use `rate(...[5m])` or a longer window. That's 5 points at 60 s; never use a window shorter than `[2m]`.
- Job, framework and filesystem `*_info` series are rebuilt every scrape. A job that ends disappears within one scrape, and Prometheus marks it stale.
- The README's example config (30 s / 15 s) gets updated to 60 s / 30 s.

## How a GPU is mapped to a job (no hook)

Every PBS job process has the PBS variables in its environment. The exporter reads these from `/proc` on the host:

```
PBS_JOBID             12345.hopper-pbs
PBS_O_LOGNAME / USER  alice
PBS_QUEUE             gpu
PBS_JOBNAME           train_llm
CUDA_VISIBLE_DEVICES  GPU-3f2a...,GPU-9c1b...   (or 0,1)
```

Steps on each scrape:

1. **Find the jobs on the node.** Scan `/proc/*/environ` and group processes by `PBS_JOBID`. For each job, keep one process plus the variables above. Cache the result per job ID, so later scrapes only need to look at new PIDs.
2. **Find which GPUs the job holds.** Read the job's `CUDA_VISIBLE_DEVICES`. If it lists UUIDs, use them as they are. If it lists indices, convert them with the `index→uuid` table from `nvidia-smi --query-gpu`.
   - This step is what lets an **idle** GPU still show its job. With no process on the GPU it reads 0%, but it stays labeled with the job that holds it.
3. **Cross-check with running processes** using `nvidia-smi --query-compute-apps=gpu_uuid,pid`:
   - Match each PID to a job, trying in order: its own `PBS_JOBID` → its PBS cgroup (`/proc/<pid>/cgroup`, e.g. `0::/pbs_jobs.service/jobs/9025.1/1` → job `9025.*`) → a parent process that has `PBS_JOBID`.
   - **Seen on borealis-36:** vLLM workers (`VLLM::Worker_TP*`) have no PBS variables in their environment, so the cgroup step is required. The cgroup file is world-readable, so this step needs no root.
   - This catches jobs where `CUDA_VISIBLE_DEVICES` is not set (no cgroup hook, so all GPUs are visible). Those jobs are mapped to the GPUs where their processes actually run.
   - If none of the three match, the process is running outside PBS. Label it `jobid="none"`.
4. **Detect the framework** for each GPU process. Match the nvidia-smi `process_name` plus `/proc/<pid>/cmdline` against known patterns. If that finds nothing (e.g. a generic `python` multiprocessing child), check up to 5 parent processes. A GPU the job holds idle takes the job's framework.
5. **Merge** the two sources into one GPU → job list. With MPS or shared GPUs, one GPU can belong to more than one job.

Why use the job environment instead of `qstat`: it needs no PBS client in the container, no extra load on the PBS server, and it gives the AD user name directly. With this approach, Centrify name lookup inside the container doesn't matter.

> Optional later: run `qstat -f -F json` (cached for 60 s) to add `project` and `walltime`. That needs the PBS client mounted into the container. It is left out of v1.

## Metrics

Existing GPU metrics are unchanged; they keep the `gpu_index`, `gpu_name`, `gpu_uuid` labels. Two new metrics:

```
hpc_gpu_job_info{gpu_uuid, gpu_index, jobid, user, queue, job_name, framework, source} 1
#   framework = vllm | sglang | tgi | triton | tensorrt-llm | lmdeploy | ollama | llama.cpp
#             | deepspeed-mii | ray-serve | jupyter | training | other | none
#               (comma-joined if one GPU runs more than one, e.g. "vllm,other")
#   source    = "env" (CUDA_VISIBLE_DEVICES) | "proc" (running process) | "both"

hpc_gpu_process_memory_used_bytes{gpu_uuid, gpu_index, pid, process_name, framework, jobid, user}
#   one series per GPU process; process_name is what nvidia-smi reports (e.g. VLLM::Worker_TP3)
```

Framework detection rules (case-insensitive, first match wins, checked against process name + cmdline, then parents):

| framework | matches |
|---|---|
| vllm | `vllm`, `RayWorkerWrapper` |
| sglang | `sglang` |
| tgi | `text-generation-launcher`, `text-generation-server` |
| triton | `tritonserver` |
| tensorrt-llm | `trtllm`, `tensorrt_llm` |
| lmdeploy | `lmdeploy` |
| ollama | `ollama` |
| llama.cpp | `llama-server`, `llama_cpp`, `llama.cpp` |
| deepspeed-mii | `deepspeed.mii`, `mii.serve` |
| ray-serve | `ray::serve`, `serve run` |
| jupyter | `jupyter`, `ipykernel` |
| training | `torchrun`, `deepspeed`, `accelerate launch`, `torch.distributed`, `megatron` |

The full cmdline is **not** a label (too many unique values); it's only used for matching.

- A GPU that no job holds has no `hpc_gpu_job_info` series. That is how Grafana can tell a free GPU from a GPU that a job holds but leaves idle.
- New series are created only when a job starts (about one per GPU per job), so cardinality stays low.

### Grafana queries (for when you build the dashboards)

```promql
# Utilization with job/user labels
hpc_gpu_utilization_percent
  * on(gpu_uuid) group_left(jobid, user, queue, job_name) hpc_gpu_job_info

# Idle: allocated to a job but 0% for the last 60 minutes
max_over_time(hpc_gpu_utilization_percent[60m])
  * on(gpu_uuid) group_left(jobid, user, queue, job_name) hpc_gpu_job_info
  == 0

# Free GPUs (no job)
hpc_gpu_utilization_percent unless on(gpu_uuid) hpc_gpu_job_info

# GPUs per inference framework across the cluster
count by (framework) (hpc_gpu_job_info{framework!~"none|other|training|jupyter"})

# Idle inference servers (model loaded, no traffic for 60 min)
max_over_time(hpc_gpu_utilization_percent[60m])
  * on(gpu_uuid) group_left(jobid, user, framework) hpc_gpu_job_info{framework=~"vllm|sglang|tgi|triton|tensorrt-llm|lmdeploy|ollama|llama.cpp"}
  == 0
```

## Code changes

| File | Change |
|---|---|
| `collectors/jobmap.go` (new) | Scan `/proc` for `PBS_JOBID`, parse the environment, cache per job ID and drop jobs that have ended, and parse `CUDA_VISIBLE_DEVICES` whether it holds indices or UUIDs. Match GPU PIDs by environ → cgroup → parent. |
| `collectors/framework.go` (new) | Framework rules table + classification from process name, cmdline and parents. Rules configurable via `GPU_FRAMEWORK_RULES` (file) so new frameworks can be added without a rebuild. |
| `collectors/gpu.go` | Keep the `index→uuid` table. Add the `--query-compute-apps` call. Merge both sources and emit `hpc_gpu_job_info` and `hpc_gpu_process_memory_used_bytes`. Try `clocks_event_reasons.active` first, then fall back to `clocks_throttle_reasons.active`. |
| `collectors/jobmap_test.go` (new) | Tests against a fake `HOST_PROC` tree: a job with UUIDs, a job with indices, a job with no `CUDA_VISIBLE_DEVICES`, a process outside PBS, a job that has ended, a worker matched only by cgroup / parent, and one fixture per framework. |
| `docker-compose.yml` | Add `user: "0"` and `cap_add: [SYS_PTRACE]`. Both are needed because reading another user's `/proc/<pid>/environ` takes root plus ptrace. Keep `pid: host` and the read-only mounts. |
| `Dockerfile` | Drop the `USER hpc-exporter` line, or make it overridable. |
| `README.md` | Fix the port (`9009`). Document `hpc_gpu_job_info`, the new privileges, and the queries above. |

New env var: `GPU_JOB_MAPPING` (default `true`), so the feature can be turned off on nodes where the extra privileges aren't allowed.

## Check on one Hopper GPU node before coding

```bash
# 1. Does the job environ have what we need? (run as root, pick a PID from a running GPU job)
tr '\0' '\n' < /proc/<pid>/environ | grep -E 'PBS_JOBID|PBS_O_LOGNAME|PBS_QUEUE|PBS_JOBNAME|CUDA_VISIBLE_DEVICES'

# 2. Are PIDs from nvidia-smi host PIDs that match /proc?
nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name --format=csv
```

What to look for:

- **Command 1:** whether `CUDA_VISIBLE_DEVICES` holds **indices or UUIDs**. If it is missing, the PBS cgroup hook isn't assigning GPUs. Mapping still works through running processes, but an idle GPU won't show which job holds it.
- **Command 2:** whether the PIDs it prints exist under `/proc` on the host. They should, because the container uses `pid: host`.

## Risks

- **Privileges:** the container needs root and `SYS_PTRACE`. It only reads `/proc`, and all mounts stay read-only. If this is not allowed, `GPU_JOB_MAPPING=false` turns the feature off.
- **Scan cost:** scanning `/proc` touches a few thousand PIDs per scrape. That is cheap, and the per-PID cache keeps it well under 100 ms.
- **MPS:** processes appear as `nvidia-cuda-mps-server`, so the process cross-check finds no job. Mapping still works through `CUDA_VISIBLE_DEVICES`.

---

# Part 2: Shared filesystems (NFS, dpc, …)

## Problems in today's `collectors/disk.go`

| # | Problem | Effect on borealis-36 |
|---|---|---|
| 1 | Only `nfs*` counts as shared; everything else is `mount_type="local"`. | `/scratch` (dpc, 3.9 PB) is labeled `local`. |
| 2 | Reads `procPath("mounts")` = `/host/proc/mounts`. That symlinks to `self/mounts`, which is the **container's** mount namespace. | In Docker it doesn't see the host's `/scratch`, `/app1`, `/hpctmp` or `/nfs/home/svu` at all. |
| 3 | `statfs(mountpoint)` runs on the container path. | Should be `/host/root` + mountpoint (compose already mounts `/` at `/host/root` with `rslave`). |
| 4 | `statfs` has no timeout. | One hung NFS/dpc mount freezes the **whole** scrape, and Prometheus loses every metric for the node. |
| 5 | No idea which mounts *should* be there. | A node that boots without `/scratch_aisg` looks healthy. |

## Design

- **Mount table:** read `HOST_PROC/1/mounts` (PID 1 = the host's view). Keep the existing skip list, and let a real mount win over an `autofs` placeholder at the same path.
- **Classification (`mount_type`):**

| mount_type | fstypes |
|---|---|
| `nfs` | nfs, nfs4 |
| `parallel` | **dpc**, lustre, gpfs, beegfs, wekafs, ceph/cephfs, panfs, pvfs2/orangefs, glusterfs |
| `smb` | cifs, smb3 |
| `fuse` | fuse.* |
| `local` | xfs, ext4, btrfs, zfs, vfat, … |
| `other` | anything unknown (still exported, so new types aren't hidden) |

  Extra types can be added with `FS_PARALLEL_TYPES=dpc,lustre,…` without a rebuild.
- **Timeout:** each `statfs` runs in its own goroutine with `FS_STAT_TIMEOUT` (default 5 s). If the timeout passes, emit `hpc_fs_responsive=0` and skip the size metrics for that mount. Don't start a new `statfs` on that mount while the old one is still stuck, so stuck goroutines don't pile up.
- **Never trigger automounts:** only `statfs` paths that are already mounted (taken from the mount table).
- **Expected mounts (optional):** `EXPECTED_MOUNTS=/scratch,/nfs/home/svu,/app1,/hpctmp`. Nodes that also mount `/scratch_aisg` add it in their own compose env file. Even without this setting, Grafana can catch a mount that disappears (query below).
- **NFS client stats:** parse `HOST_PROC/1/mountstats` for RPC ops, retransmissions, timeouts and average RTT per mount. dpc has no `mountstats`; it only gets size and responsiveness.

## Metrics

Existing `hpc_disk_*` metrics are unchanged, except `mount_type` now says `parallel` for dpc. New metrics:

```
hpc_fs_info{mountpoint, fstype, mount_type, server, export, nfs_vers, dev} 1
#   dev = major:minor from mountinfo; NFS mounts with the same dev share one superblock
#   server/export split from "host:/path"; dpc → server="-", export="/hpfs-main"
hpc_fs_responsive{mountpoint, mount_type}          # 1 = statfs answered within timeout
hpc_fs_stat_duration_seconds{mountpoint}
hpc_fs_readonly{mountpoint, mount_type}            # 1 if mounted ro (catches NFS falling back to ro)
hpc_fs_mounted{mountpoint}                         # only for EXPECTED_MOUNTS; 0 = missing
# NFS: one set per superblock (not per mountpoint), per RPC op. All are counters.
hpc_nfs_rpc_ops_total{server, nfs_dev, mountpoints, op}
hpc_nfs_rpc_retransmissions_total{server, nfs_dev, mountpoints, op}
hpc_nfs_rpc_timeouts_total{server, nfs_dev, mountpoints, op}
hpc_nfs_rpc_rtt_milliseconds_total{server, nfs_dev, mountpoints, op}      # network + server time
hpc_nfs_rpc_execute_milliseconds_total{server, nfs_dev, mountpoints, op}  # rtt + client queueing
```

**Seen on borealis-36:** `/nfs/home/svu` and `/app1` (both Isilon `/ifs` on hpcnas100gsto) reported identical counters. The likely cause is one shared NFS superblock, which the device IDs in `/proc/1/mountinfo` will confirm or rule out. Per-mountpoint NFS series would therefore double-count. Group by `mountinfo` device ID and emit one set, with `mountpoints="/nfs/home/svu,/app1"`. Don't export averages: "avg RTT since mount" (29.8 ms on borealis-36) hides current behaviour. Export counters and let Grafana `rate()` them.

### Grafana queries

```promql
# Hung shared filesystem on any node
hpc_fs_responsive{mount_type=~"nfs|parallel"} == 0

# Expected mount missing
hpc_fs_mounted == 0

# Mount disappeared (no config needed): was there 1h ago, gone now
hpc_fs_info offset 1h unless on(instance, mountpoint) hpc_fs_info

# Nodes with /scratch_aisg
count by (instance) (hpc_fs_info{mountpoint="/scratch_aisg"})

# NFS retransmission rate (network or NAS trouble)
sum by (instance, server, mountpoints) (rate(hpc_nfs_rpc_retransmissions_total[5m])) > 0

# Current NFS latency per mount group (ms per RPC, last 5 min)
sum by (instance, server, mountpoints) (rate(hpc_nfs_rpc_rtt_milliseconds_total[5m]))
  / sum by (instance, server, mountpoints) (rate(hpc_nfs_rpc_ops_total[5m]))

# Same, per operation (which op is slow: GETATTR, LOOKUP, READ, WRITE ...)
rate(hpc_nfs_rpc_rtt_milliseconds_total[5m]) / rate(hpc_nfs_rpc_ops_total[5m])

# Client-side queueing (execute much larger than rtt = client is the bottleneck)
rate(hpc_nfs_rpc_execute_milliseconds_total[5m]) - rate(hpc_nfs_rpc_rtt_milliseconds_total[5m])

# Shared FS fill level (one value per filesystem, not per node)
max by (mountpoint) (hpc_disk_used_bytes{mount_type=~"nfs|parallel"} / hpc_disk_total_bytes)
```

## Code changes

| File | Change |
|---|---|
| `collectors/disk.go` | Read `HOST_PROC/1/mounts`. Prefix `statfs` paths with `HOST_ROOT` (new env, `/host/root` in compose). Add the classification table, the per-mount timeout and the `hpc_fs_*` metrics. |
| `collectors/nfsstats.go` (new) | Parse `HOST_PROC/1/mountstats` per RPC op, group mounts by `mountinfo` device ID (shared superblock), and emit `hpc_nfs_rpc_*` counters once per group. |
| `collectors/disk_test.go` (new) | Fixture built from borealis-36's mount table (dpc + 3 NFS + autofs placeholder), a missing expected mount, and a hung mount (fake statfs that blocks). |
| `docker-compose.yml` | Add `HOST_ROOT: /host/root` and `EXPECTED_MOUNTS`. |

Test script: `tools/test-shared-fs.sh` (read-only). Run it on a node to confirm before coding.

---

# Part 3: Remove the IPMI collector (iDRAC exporter covers it)

Power, SEL and other BMC data will come from the iDRAC exporter, so hexporter drops IPMI completely.

| File | Change |
|---|---|
| `collectors/ipmi.go` | Delete. |
| `collectors/health.go` | Take over `hpc_system_uptime_seconds` and `readUptime()`. Uptime comes from `/proc/uptime`, not IPMI, and is needed to spot node reboots. |
| `main.go` | Remove the `IPMI_ENABLED` block and `NewIPMICollector()`. |
| `docker-compose.yml` | Remove `cap_add: SYS_RAWIO` and `IPMI_ENABLED`. (`SYS_PTRACE` from Part 1 stays.) |
| `Dockerfile` | Remove the `ipmitool` package and its comments. |
| `README.md` | Remove IPMI from the description, the env table, the collector table, the "sudo needed for ipmitool" note and the metrics list. |

**Metrics removed:** `hpc_ipmi_power_watts`, `hpc_ipmi_sel_entries_total`, and `hpc_collector_error{collector="ipmi"}`.
**Metrics kept:** `hpc_system_uptime_seconds` (moved to the health collector, same name and value).

In Grafana, node power now comes from the iDRAC exporter. GPU power (`hpc_gpu_power_draw_watts`) still comes from hexporter.

---

# Part 4: DCGM profiling metrics (which units the GPU is using: FP64 / FP32 / FP16 / Tensor Cores)

`nvidia-smi` utilization only says whether *a* kernel was running. It can't tell an FP64 HPC code from BF16 training or INT8 inference, and it reads ~100% even when tiny kernels leave most SMs idle. DCGM profiling fields read the hardware counters per compute pipe.

## Fields

| DCGM field (ID) | hexporter metric | Tells you |
|---|---|---|
| `DCGM_FI_PROF_GR_ENGINE_ACTIVE` (1001) | `hpc_gpu_gr_engine_active_ratio` | Same idea as nvidia-smi utilization |
| `DCGM_FI_PROF_SM_ACTIVE` (1002) | `hpc_gpu_sm_active_ratio` | Share of SMs actually working (**better idle signal**) |
| `DCGM_FI_PROF_SM_OCCUPANCY` (1003) | `hpc_gpu_sm_occupancy_ratio` | How full the active SMs are |
| `DCGM_FI_PROF_DRAM_ACTIVE` (1005) | `hpc_gpu_dram_active_ratio` | HBM bandwidth busy (memory-bound work) |
| `DCGM_FI_PROF_PIPE_TENSOR_ACTIVE` (1004) | `hpc_gpu_pipe_active_ratio{pipe="tensor"}` | Any Tensor Core activity |
| `DCGM_FI_PROF_PIPE_TENSOR_HMMA_ACTIVE` (1014) | `…{pipe="tensor_hmma"}` | Half-precision tensor (FP16/BF16): AI training/inference |
| `DCGM_FI_PROF_PIPE_TENSOR_IMMA_ACTIVE` (1013) | `…{pipe="tensor_imma"}` | Integer tensor (INT8/INT4): quantized inference |
| `DCGM_FI_PROF_PIPE_TENSOR_DFMA_ACTIVE` (1015) | `…{pipe="tensor_dfma"}` | FP64 tensor: HPC double precision |
| `DCGM_FI_PROF_PIPE_FP64_ACTIVE` (1006) | `…{pipe="fp64"}` | FP64 CUDA cores: classic HPC |
| `DCGM_FI_PROF_PIPE_FP32_ACTIVE` (1007) | `…{pipe="fp32"}` | FP32 CUDA cores |
| `DCGM_FI_PROF_PIPE_FP16_ACTIVE` (1008) | `…{pipe="fp16"}` | FP16 CUDA cores (non-tensor) |
| `DCGM_FI_PROF_PIPE_INT_ACTIVE` (1016) | `…{pipe="int"}` | Integer ALU |
| `DCGM_FI_PROF_PCIE_TX/RX_BYTES` (1009/1010) | `hpc_gpu_pcie_{tx,rx}_bytes_per_second` | Host ↔ GPU traffic |
| `DCGM_FI_PROF_NVLINK_TX/RX_BYTES` (1011/1012) | `hpc_gpu_nvlink_{tx,rx}_bytes_per_second` | GPU ↔ GPU traffic (tensor parallel) |

All labels: `gpu_index`, `gpu_uuid`, plus `pipe` where shown. Ratios are 0–1, the fraction of cycles busy. The field IDs above still need checking on the B200 nodes with `dcgmi profile -l -i 0`, which the test script prints. Only fields that `profile -l` reports as supported are read.

## All GPU metrics move from nvidia-smi to DCGM

DCGM also provides every value `nvidia-smi --query-gpu` gave, plus health fields. **Metric names stay the same**, so existing dashboards keep working.

| DCGM field (default ID) | hexporter metric | Note |
|---|---|---|
| `DEV_GPU_UTIL` (203) | `hpc_gpu_utilization_percent` | same as before |
| `DEV_MEM_COPY_UTIL` (204) | `hpc_gpu_memory_copy_utilization_percent` | **new** |
| `DEV_FB_TOTAL / FREE / USED` (250/251/252) | `hpc_gpu_memory_{total,free,used}_bytes` | MiB × 1048576 |
| `DEV_GPU_TEMP` (150) | `hpc_gpu_temperature_celsius` | |
| `DEV_MEMORY_TEMP` (140) | `hpc_gpu_memory_temperature_celsius` | **new**: HBM temperature |
| `DEV_POWER_USAGE` (155) | `hpc_gpu_power_draw_watts` | |
| `DEV_TOTAL_ENERGY_CONSUMPTION` (156) | `hpc_gpu_energy_joules_total` | **new** counter (mJ ÷ 1000), for GPU energy per job |
| `DEV_SM_CLOCK / MEM_CLOCK` (100/101) | `hpc_gpu_clock_sm_mhz`, `hpc_gpu_clock_mem_mhz` | |
| `DEV_CLOCKS_EVENT_REASONS` (112) | `hpc_gpu_throttle_reason{reason="active_mask"}` | bitmask, as before |
| `DEV_POWER_VIOLATION / THERMAL_VIOLATION` (240/241) | `hpc_gpu_{power,thermal}_violation_seconds_total` | **new** (µs ÷ 1e6): time held back by power or heat |
| `DEV_XID_ERRORS` (230) | `hpc_gpu_last_xid` | **new**: code of the last XID (0 = none). Alert on `changes()` |
| `DEV_ECC_SBE/DBE_VOL_TOTAL` (310/311) | `hpc_gpu_ecc_errors_{corrected,uncorrected}_total` | |
| `DEV_CORRECTABLE/UNCORRECTABLE_REMAPPED_ROWS` (394/393) | `hpc_gpu_remapped_rows{type}` | **new**: HBM rows retired |
| `DEV_ROW_REMAP_FAILURE` (395) | `hpc_gpu_row_remap_failure` | **new**: 1 = GPU needs RMA |
| `DEV_PCIE_REPLAY_COUNTER` (202) | `hpc_gpu_pcie_replay_total` | **new**: PCIe link quality |
| `DEV_NVLINK_{CRC_FLIT,CRC_DATA,REPLAY,RECOVERY}_ERROR_COUNT_TOTAL` (409/419/429/439) | `hpc_gpu_nvlink_errors_total{type}` | **new** |
| `hpc_gpu_dcgm_up` | – | **new**: 1 if DCGM answered this scrape |

Plus the profiling fields above (pipes, SM, DRAM, PCIe/NVLink bytes). The default IDs are only a fallback: at start, hexporter resolves every ID **by name** from `dcgmi dmon -l`, so a DCGM version that renumbers fields still works. The test script shows `ID DIFFERS` when that happens.

Fields that come back `N/A` aren't emitted; NVLink error counters may be N/A on some GPUs. They never get exported as 0.

### What still uses nvidia-smi

- **Only** `nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_memory`: the PID list that drives job, user and framework mapping (Part 1). DCGM can't list processes per GPU; it can only give stats for a PID you already know.
- **Fallback:** if `nv-hostengine` is down, hexporter sets `hpc_gpu_dcgm_up 0` and falls back to the old `nvidia-smi --query-gpu` call. Basic utilization, memory, temperature and power keep coming; health and pipe metrics stop until DCGM is back.

### Health queries

```promql
# New XID error in the last 5 min (any code)
changes(hpc_gpu_last_xid[5m]) > 0
# GPU needs RMA / drain the node
hpc_gpu_row_remap_failure == 1 or increase(hpc_gpu_ecc_errors_uncorrected_total[1h]) > 0
# Throttled by power or heat (share of time, last 5 min)
rate(hpc_gpu_power_violation_seconds_total[5m]) > 0.1
rate(hpc_gpu_thermal_violation_seconds_total[5m]) > 0
# Link problems
increase(hpc_gpu_pcie_replay_total[1h]) > 0
increase(hpc_gpu_nvlink_errors_total[1h]) > 0
# GPU energy per job (kWh over the last day)
sum by (jobid, user) (increase(hpc_gpu_energy_joules_total[1d]) * on(gpu_uuid) group_left(jobid, user) hpc_gpu_job_info) / 3.6e6
# DCGM down on a node
hpc_gpu_dcgm_up == 0
```

## How hexporter reads them

- **Requires** DCGM 4.x on the host with `nv-hostengine` running (service `nvidia-dcgm`). Blackwell/B200 needs DCGM 4.
- **v1: run `dcgmi`**, the same approach as the existing `nvidia-smi` calls (no cgo). Each scrape runs **one** `dcgmi dmon -e <all resolved ids> -c 2 -d 1000` for the device, health and profiling fields together, and keeps the last sample per GPU. That takes about 2 s, well inside the 30 s scrape timeout. Values are real-time, a 1 s window at scrape time.
- **Later (optional):** use `github.com/NVIDIA/go-dcgm`, which avoids the exec but needs cgo and `libdcgm`.
- **Container:** `network_mode: host` already lets `dcgmi` in the container reach the host's `nv-hostengine` on `localhost:5555`. Install the `datacenter-gpu-manager-4-core` package in the image, matching the host's DCGM major version.
- **Env:** `DCGM_ENABLED` (default `true`). If `dcgmi` is missing or the hostengine is down, the collector falls back to nvidia-smi. `DCGM_FIELDS` overrides the field list.

## Conflicts to know

- **Nsight Compute / Nsight Systems GPU metrics** need the same hardware counters. While DCGM holds them, user profiling fails. Users, or a PBS prologue for profiling jobs, run `dcgmi profile --pause` and then `--resume`. While paused, the exporter emits nothing for these fields; it never emits wrong zeros.
- Run only **one** DCGM consumer per node. If NVIDIA's `dcgm-exporter` is also deployed, point both at the same hostengine, not embedded mode.

## Grafana queries

```promql
# What each job is actually computing with (busiest pipe per GPU)
topk by (gpu_uuid) (1, hpc_gpu_pipe_active_ratio{pipe!="tensor"})
  * on(gpu_uuid) group_left(jobid, user, framework) hpc_gpu_job_info

# FP64 users (HPC double precision) vs tensor users (AI)
sum by (user) (hpc_gpu_pipe_active_ratio{pipe=~"fp64|tensor_dfma"} * on(gpu_uuid) group_left(user) hpc_gpu_job_info)
sum by (user) (hpc_gpu_pipe_active_ratio{pipe=~"tensor_hmma|tensor_imma"} * on(gpu_uuid) group_left(user) hpc_gpu_job_info)

# "Looks busy but isn't": engine busy, SMs idle (nvidia-smi would show ~100%)
hpc_gpu_gr_engine_active_ratio > 0.5 and hpc_gpu_sm_active_ratio < 0.1

# Stricter idle rule using SM activity instead of utilization
max_over_time(hpc_gpu_sm_active_ratio[60m]) < 0.01
  and on(gpu_uuid) hpc_gpu_job_info
```

## Code changes

| File | Change |
|---|---|
| `collectors/dcgm.go` (new) | Field table (name → metric, labels, scale). At start, resolve IDs by name (`dcgmi dmon -l`) and check which profiling fields the GPU supports (`dcgmi profile -l`). Each scrape, run one `dcgmi dmon`, parse by column position, and map GPU index to UUID. |
| `collectors/gpu.go` | Becomes the GPU collector front-end: DCGM for all values. `nvidia-smi --query-compute-apps` only for the process list. `nvidia-smi --query-gpu` only as the fallback when `hpc_gpu_dcgm_up` is 0. |
| `collectors/dcgm_test.go` (new) | Parse fixtures: normal output, `N/A` rows, a partial field list, renumbered IDs, the "profiling paused" error, and the hostengine-down fallback. |
| `Dockerfile` | Add the NVIDIA repo and `datacenter-gpu-manager-4-core`. |
| `docker-compose.yml` | `DCGM_ENABLED: "true"`. |

Test script: `tools/test-dcgm.sh` (read-only). Run it on borealis-36 to confirm DCGM is installed, the field IDs this DCGM version uses, which profiling fields the B200 supports, and the real values and health state of all 8 GPUs. Set `FULL=1` to print every metric line.
