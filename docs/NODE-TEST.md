# Installing hexporter v1.0.1 on a GPU node (systemd, no Docker)

Three files in one folder (on borealis: `/home/svu/zidni`):

| File | |
|---|---|
| `hpc-exporter` | static linux/amd64 binary, no dependencies |
| `setup-hpc-exporter.sh` | installs, checks and removes the systemd service; contains the unit and the default settings |
| `verify-metrics.sh` | full read-only check of a running exporter (PASS/WARN/FAIL) |

The files are world-readable (`755`), so root can read them from the NFS home even with root squash.

## 1. Copy (from the Mac)

```bash
cd ~/Documents/2026/PersonalProject/hexporter
make linux
scp dist/hpc-exporter-linux-amd64 zidni@borealis.nus.edu.sg:/home/svu/zidni/hpc-exporter
scp deploy/systemd/setup-hpc-exporter.sh tools/verify-metrics.sh zidni@borealis.nus.edu.sg:/home/svu/zidni/
ssh zidni@borealis.nus.edu.sg 'cd /home/svu/zidni && chmod 755 hpc-exporter setup-hpc-exporter.sh verify-metrics.sh'
```

## 2. Install (root, on the GPU node)

```bash
bash /home/svu/zidni/setup-hpc-exporter.sh install
```

The script:
1. Checks it runs as root and that the port (default 9009) is free.
2. Copies the binary to `/usr/local/bin/hpc-exporter` and checks that it runs.
3. Writes `/etc/hpc-exporter/hpc-exporter.env`, but only if it doesn't exist yet.
4. Writes `/etc/systemd/system/hpc-exporter.service`, enables it at boot, and starts it.
5. Scrapes `/metrics` once and prints a short summary: GPUs, DCGM, GPU→job rows, shared filesystems, collector errors, missing mounts.

On a node with other mounts or a different port, set them the first time:

```bash
PORT=9019 EXPECTED_MOUNTS=/scratch,/scratch_aisg,/nfs/home/svu,/app1,/hpctmp \
  bash /home/svu/zidni/setup-hpc-exporter.sh install
```

## 3. Verify

```bash
bash /home/svu/zidni/verify-metrics.sh            # add the URL if not on 9009: http://localhost:9019/metrics
```

Expected on borealis-36 (job 9025, vLLM on 8× B200):

| Check | Expected |
|---|---|
| GPUs | `8 GPUs reporting utilization` |
| DCGM | `DCGM up` (`cuda-dcgm` runs nv-hostengine on borealis; dcgm-exporter shares it, no conflict) |
| Job mapping | `all process environments readable`, 8 rows `9025.vip-ha-pbs zidni large glm53 vllm both`, `one job per GPU` |
| Filesystems | `/nfs/home/svu`, `/app1`, `/hpctmp` (nfs) and `/scratch` (dpc, parallel), all responding; expected mounts present |
| NFS | `NFS stats for: /hpctmp /nfs/home/svu,/app1`, meaning the shared superblock is reported once |
| IPMI | removed |

## 4. Day to day

| Task | Command (root) |
|---|---|
| State + quick check | `bash /home/svu/zidni/setup-hpc-exporter.sh status` |
| Logs | `journalctl -u hpc-exporter -f` |
| Change settings | edit `/etc/hpc-exporter/hpc-exporter.env`, then `systemctl restart hpc-exporter` |
| Upgrade | replace `hpc-exporter` in `/home/svu/zidni`, then `bash setup-hpc-exporter.sh install` (settings are kept; the new defaults go to `hpc-exporter.env.new`) |
| Remove | `bash /home/svu/zidni/setup-hpc-exporter.sh uninstall` (keeps `/etc/hpc-exporter`) |

The service runs as root (to read every job's environment and the mount statistics), at low priority (`Nice=10`, idle I/O), and is capped at 512 MB memory and 256 threads. It only reads `/proc`, `/sys` and the mount table, and calls `nvidia-smi`, `dcgmi`, `ibstat` and `systemctl is-active`. It runs in the host's mount namespace, so NFS, dpc and autofs mounts look exactly as they do on the host.

## 5. More nodes

On each new GPU node run the same install as root (it is safe to rerun). With pdsh on Bright, from a root shell on the head node:

```bash
pdsh -w borealis-[37-40] 'bash /home/svu/zidni/setup-hpc-exporter.sh install' | dshbak -c
```

Nodes with extra mounts (e.g. `/scratch_aisg`) need `EXPECTED_MOUNTS=...` in front, as in step 2. Then add each node to the Prometheus targets file (`/etc/prometheus/targets/hexporter-*.yml`, see `deploy/prometheus/targets/`); Prometheus picks it up within a minute.

## If something fails

| Symptom | Likely cause / fix |
|---|---|
| `not found or not readable by root` | NFS root squash and the files aren't world-readable: `chmod 755` them as zidni, or copy them to `/tmp` |
| `port 9009 is in use` | `PORT=9019 bash setup-hpc-exporter.sh install` |
| `service did not start` | The script prints the last 20 log lines; also `journalctl -u hpc-exporter -n 50` |
| `processes unreadable` > 0 | SELinux or `hidepid` blocking `/proc`: check `cat /proc/<pid>/environ` as root for a job process |
| `DCGM down` | `dcgmi` not found (searched: PATH, `EXTRA_PATH`, `/cm/local/apps/cuda-dcgm/current/bin`, `/usr/local/dcgm/bin`), or `nv-hostengine` not running: `dcgmi discovery -l` must list the GPUs |
| No pipe metrics, DCGM up | Profiling busy (another DCGM exporter or Nsight is profiling): `dcgmi profile -l -i 0` |
| Each GPU shows two rows (job + `none`) | Workers outside the job cgroup: send `cat /proc/<pid>/cgroup` for one worker |
| Mount "not responding" | That mount is hung; `df` on it will hang too. The exporter keeps going (5 s timeout) |
| Scrape > 30 s | `journalctl -u hpc-exporter` and look for "statfs … no answer" |

Cross-check scripts (read-only, in the repo's `tools/`): `test-gpu-job-map.sh`, `test-dcgm.sh`, `test-shared-fs.sh`.

---

### Docker instead

If Docker with the NVIDIA Container Toolkit is available: `gunzip -c hpc-exporter-1.0.1-amd64.tar.gz | docker load`, then `docker compose up -d` with the repo's `docker-compose.yml`. The same `verify-metrics.sh` applies.
