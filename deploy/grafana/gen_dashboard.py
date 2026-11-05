#!/usr/bin/env python3
"""Generate hexporter-gpu.json, the Grafana dashboard for hexporter v1.

    python3 deploy/grafana/gen_dashboard.py > deploy/grafana/hexporter-gpu.json

Idle detection: a GPU is "idle" when a job holds it and its utilization has
stayed at or below $idle_threshold %. "Idle for" is the shorter of
  - the time since the GPU was last busy (utilization > $idle_threshold), and
  - the time since the job got the GPU,
so a job that just started and is still loading is not reported as idle.
Lookback is 24 h: "idle for" tops out at about 24 h.
"""
import json

DS = {"type": "prometheus", "uid": "${datasource}"}
INST = 'instance=~"$instance"'
UTIL = f'hpc_gpu_utilization_percent{{{INST}}}'
JOBALL = f'hpc_gpu_job_info{{{INST}}}'
JOBPBS = f'hpc_gpu_job_info{{{INST}, jobid!="none"}}'
# one row per (GPU, job), filtered by user; drops "source" so it can't split series
JOB = f'max without (source) (hpc_gpu_job_info{{{INST}, user=~"$user"}})'
JL = "jobid, user, queue, job_name"

# seconds since the GPU was last busy; 1e9 if not busy in the last 24 h
SINCE_BUSY = (
    f'(min by (instance, gpu_uuid) (time() - max_over_time(timestamp({UTIL} > $idle_threshold)[24h:1m]))'
    f' or (count by (instance, gpu_uuid) ({UTIL}) * 0 + 1e9))'
)
# seconds since the job got the GPU (within 24 h)
HELD = f'(time() - min_over_time(timestamp(count by (instance, gpu_uuid, jobid) ({JOBALL}))[24h:1m]))'
# min(HELD, SINCE_BUSY) = (a + b - |a - b|) / 2, labels (instance, gpu_uuid, jobid)
IDLE_RAW = (
    f'(({HELD} + on (instance, gpu_uuid) group_left () {SINCE_BUSY})'
    f' - abs({HELD} - on (instance, gpu_uuid) group_left () {SINCE_BUSY})) / 2'
)
# with job labels (gpu_index, user, queue, job_name, framework)
IDLE = f'({JOB} * on (instance, gpu_uuid, jobid) group_left () ({IDLE_RAW}))'
IDLE_OVER = f'({IDLE} >= $idle_minutes * 60)'


def on_jobs(metric):
    """metric per GPU -> one row per (GPU, job) with the job labels."""
    return f'{JOB} * on (instance, gpu_uuid) group_left () {metric}'


_id = [0]


def nid():
    _id[0] += 1
    return _id[0]


def tgt(expr, legend="", ref="A", instant=False, table=False):
    t = {"datasource": DS, "refId": ref, "expr": expr, "legendFormat": legend or "__auto",
         "range": not instant, "instant": instant}
    if table:
        t["format"] = "table"
    return t


def thresholds(*steps):
    s = [{"color": steps[0], "value": None}]
    for v, c in zip(steps[1::2], steps[2::2]):
        s.append({"color": c, "value": v})
    return {"mode": "absolute", "steps": s}


def stat(title, expr, x, y, w=4, h=4, unit="none", th=None, desc="", decimals=None):
    d = {"unit": unit, "thresholds": th or thresholds("green"), "color": {"mode": "thresholds"}}
    if decimals is not None:
        d["decimals"] = decimals
    return {"id": nid(), "type": "stat", "title": title, "description": desc, "datasource": DS,
            "gridPos": {"x": x, "y": y, "w": w, "h": h},
            "targets": [tgt(expr, instant=True)],
            "fieldConfig": {"defaults": d, "overrides": []},
            "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                        "colorMode": "background", "graphMode": "none", "textMode": "value",
                        "justifyMode": "center"}}


def ts(title, targets, x, y, w=12, h=8, unit="none", desc="", stack=False, maxv=None, minv=None,
       legend_table=False):
    d = {"unit": unit, "custom": {"drawStyle": "line", "lineWidth": 1, "fillOpacity": 10,
                                  "showPoints": "never", "spanNulls": False,
                                  "stacking": {"mode": "normal" if stack else "none", "group": "A"}},
         "color": {"mode": "palette-classic"}}
    if maxv is not None:
        d["max"] = maxv
    if minv is not None:
        d["min"] = minv
    leg = {"displayMode": "table" if legend_table else "list", "placement": "bottom", "showLegend": True}
    if legend_table:
        leg["calcs"] = ["mean", "max", "lastNotNull"]
    return {"id": nid(), "type": "timeseries", "title": title, "description": desc, "datasource": DS,
            "gridPos": {"x": x, "y": y, "w": w, "h": h}, "targets": targets,
            "fieldConfig": {"defaults": d, "overrides": []},
            "options": {"legend": leg, "tooltip": {"mode": "multi", "sort": "desc"}}}


def table(title, targets, x, y, w=24, h=8, rename=None, hide=(), overrides=(), desc="", sort=None,
          merge=True, order=()):
    excl = {"Time": True, "job": True, "gpu_uuid": True, "cluster": True, "__name__": True}
    for hcol in hide:
        excl[hcol] = True
    tr = []
    if merge and len(targets) > 1:
        tr.append({"id": "merge", "options": {}})
    tr.append({"id": "organize", "options": {"excludeByName": excl, "renameByName": rename or {},
                                              "indexByName": _index(order, rename or {})}})
    opts = {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}}
    if sort:
        opts["sortBy"] = [{"displayName": sort[0], "desc": sort[1]}]
    return {"id": nid(), "type": "table", "title": title, "description": desc, "datasource": DS,
            "gridPos": {"x": x, "y": y, "w": w, "h": h}, "targets": targets,
            "transformations": tr,
            "fieldConfig": {"defaults": {"custom": {"align": "auto", "filterable": True},
                                         "thresholds": thresholds("green")},
                            "overrides": list(overrides)},
            "options": opts}


def _index(order, rename):
    """Column order by display name -> organize indexByName, which uses the original field names."""
    back = {v: k for k, v in rename.items()}
    return {back.get(c, c): i for i, c in enumerate(order)}


def ov(name, unit=None, color_bg=None, th=None, decimals=None, width=None, mappings=None):
    props = []
    if unit:
        props.append({"id": "unit", "value": unit})
    if decimals is not None:
        props.append({"id": "decimals", "value": decimals})
    if th:
        props.append({"id": "thresholds", "value": th})
    if color_bg:
        props.append({"id": "custom.cellOptions", "value": {"type": color_bg}})
    if width:
        props.append({"id": "custom.width", "value": width})
    if mappings:
        props.append({"id": "mappings", "value": mappings})
    return {"matcher": {"id": "byName", "options": name}, "properties": props}


def row(title, y, collapsed=False, panels=None):
    r = {"id": nid(), "type": "row", "title": title, "collapsed": collapsed,
         "gridPos": {"x": 0, "y": y, "w": 24, "h": 1}, "panels": []}
    if collapsed:
        r["panels"] = panels or []
    return r


IDLE_TH = thresholds("green", 1800, "orange", 3600, "red")
P = []
y = 0

# ---------------------------------------------------------------- idle
P.append(row("Idle detection", y)); y += 1
P += [
    stat("Allocated", f'count(count by (instance, gpu_uuid) ({JOB})) or vector(0)', 0, y,
         th=thresholds("blue"), desc="GPUs held by a job (PBS, or a process outside PBS)"),
    stat("Idle", f'count({IDLE_OVER}) or vector(0)', 4, y,
         th=thresholds("green", 1, "red"),
         desc="Held by a job, utilization ≤ $idle_threshold % for at least $idle_minutes min"),
    stat("Idle share", f'(count({IDLE_OVER}) or vector(0)) / count(count by (instance, gpu_uuid) ({JOB}))',
         8, y, unit="percentunit", th=thresholds("green", 0.1, "orange", 0.25, "red"), decimals=0,
         desc="Idle allocated GPUs / allocated GPUs"),
    stat("Free (no job)",
         f'count(count by (instance, gpu_uuid) ({UTIL}) unless on (instance, gpu_uuid) {JOBALL}) or vector(0)',
         12, y, th=thresholds("text"), desc="GPUs no job or process is using"),
    stat("GPU-h held",
         f'sum(sum_over_time((count by (instance, gpu_uuid) ({JOB}) > bool 0)[$__range:1m])) / 60',
         16, y, unit="none", decimals=1, th=thresholds("blue"),
         desc="GPU-hours held by jobs in the selected time range (1-minute resolution)"),
    stat("GPU-h idle",
         f'sum(sum_over_time((({UTIL} <= bool $idle_threshold) * on (instance, gpu_uuid) group_left () '
         f'(count by (instance, gpu_uuid) ({JOB}) > bool 0))[$__range:1m])) / 60',
         20, y, unit="none", decimals=1, th=thresholds("green", 1, "orange", 24, "red"),
         desc="GPU-hours held by jobs while utilization was ≤ $idle_threshold % (minute by minute)"),
]
y += 4
P.append(table(
    "Jobs: GPUs held and idle",
    [tgt(f'count by ({JL}) ({JOB})', ref="A", instant=True, table=True),
     tgt(f'count by ({JL}) ({IDLE_OVER}) or count by ({JL}) ({JOB}) * 0', ref="B", instant=True, table=True),
     tgt(f'min by ({JL}) ({IDLE})', ref="C", instant=True, table=True),
     tgt(f'avg by ({JL}) ({on_jobs(f"avg_over_time({UTIL}[${{idle_minutes}}m])")})', ref="D",
         instant=True, table=True),
     tgt(f'sum by ({JL}) ({on_jobs(f"hpc_gpu_memory_used_bytes{{{INST}}}")})', ref="E",
         instant=True, table=True),
     tgt(f'sum by ({JL}) ({on_jobs(f"hpc_gpu_power_draw_watts{{{INST}}}")})', ref="F",
         instant=True, table=True)],
    0, y, w=24, h=7,
    rename={"jobid": "Job", "user": "User", "queue": "Queue", "job_name": "Job name",
            "Value #A": "GPUs", "Value #B": "Idle GPUs", "Value #C": "All GPUs idle for",
            "Value #D": "Avg util (window)", "Value #E": "GPU memory", "Value #F": "Power"},
    overrides=[ov("Idle GPUs", th=thresholds("green", 1, "red"), color_bg="color-background"),
               ov("All GPUs idle for", unit="dtdurations", th=IDLE_TH, color_bg="color-background"),
               ov("Avg util (window)", unit="percent", decimals=1),
               ov("GPU memory", unit="bytes", decimals=0),
               ov("Power", unit="watt", decimals=0)],
    sort=("All GPUs idle for", True),
    order=("Job", "User", "GPUs", "Idle GPUs", "All GPUs idle for", "Avg util (window)", "GPU memory", "Power",
           "Queue", "Job name"),
    desc="All GPUs idle for = how long every GPU of the job has been idle. Avg util over the last $idle_minutes min."))
y += 7
P.append(table(
    "Idle GPUs: held by a job, ≤ $idle_threshold % for ≥ $idle_minutes min",
    [tgt(IDLE_OVER, instant=True, table=True)], 0, y, w=10, h=10,
    rename={"instance": "Node", "gpu_index": "GPU", "jobid": "Job", "user": "User", "queue": "Queue",
            "job_name": "Job name", "framework": "Framework", "Value": "Idle for"},
    overrides=[ov("Idle for", unit="dtdurations", th=IDLE_TH, color_bg="color-background")],
    sort=("Idle for", True),
    order=("Node", "GPU", "Idle for", "Job", "User", "Framework", "Queue", "Job name"),
    desc="Idle for = shorter of (time since last busy) and (time since the job got the GPU); max ~24 h."))
STATE = (f'({UTIL} > bool $idle_threshold) * 2 + on (instance, gpu_uuid) group_left () '
         f'(clamp_max(count by (instance, gpu_uuid) ({JOBPBS}), 1) or count by (instance, gpu_uuid) ({UTIL}) * 0)')
st = {"id": nid(), "type": "state-timeline", "title": "GPU state over time", "datasource": DS,
      "description": "Free = no PBS job · Idle = PBS job holds it at ≤ $idle_threshold % · "
                     "Busy outside PBS = work without a PBS job · Busy = PBS job computing",
      "gridPos": {"x": 10, "y": y, "w": 14, "h": 10},
      "targets": [tgt(STATE, legend="{{instance}} GPU{{gpu_index}}")],
      "fieldConfig": {"defaults": {"color": {"mode": "fixed", "fixedColor": "text"},
                                   "custom": {"fillOpacity": 80, "lineWidth": 0},
                                   "mappings": [{"type": "value", "options": {
                                       "0": {"text": "Free", "color": "#4a4a4a", "index": 0},
                                       "1": {"text": "Idle (job)", "color": "red", "index": 1},
                                       "2": {"text": "Busy outside PBS", "color": "purple", "index": 2},
                                       "3": {"text": "Busy", "color": "green", "index": 3}}}]},
                      "overrides": []},
      "options": {"mergeValues": True, "showValue": "never", "alignValue": "left", "rowHeight": 0.85,
                  "legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"},
                  "tooltip": {"mode": "single"}}}
P.append(st); y += 10

# ---------------------------------------------------------------- who uses
P.append(row("Who uses the GPUs", y)); y += 1
P.append(table(
    "GPU → job → user (now)",
    [tgt(on_jobs(UTIL), ref="A", instant=True, table=True),
     tgt(on_jobs(f'hpc_gpu_sm_active_ratio{{{INST}}}'), ref="B", instant=True, table=True),
     tgt(on_jobs(f'hpc_gpu_pipe_active_ratio{{{INST}, pipe="tensor"}}'), ref="C", instant=True, table=True),
     tgt(on_jobs(f'hpc_gpu_memory_used_bytes{{{INST}}}'), ref="D", instant=True, table=True),
     tgt(on_jobs(f'hpc_gpu_power_draw_watts{{{INST}}}'), ref="E", instant=True, table=True),
     tgt(IDLE, ref="F", instant=True, table=True)],
    0, y, w=24, h=9,
    rename={"instance": "Node", "gpu_index": "GPU", "jobid": "Job", "user": "User", "queue": "Queue",
            "job_name": "Job name", "framework": "Framework", "Value #A": "Util", "Value #B": "SM active",
            "Value #C": "Tensor", "Value #D": "Memory", "Value #E": "Power", "Value #F": "Idle for"},
    overrides=[ov("Util", unit="percent", decimals=0, color_bg="gauge", th=thresholds("red", 5, "orange", 30, "green")),
               ov("SM active", unit="percentunit", decimals=0),
               ov("Tensor", unit="percentunit", decimals=0),
               ov("Memory", unit="bytes", decimals=1),
               ov("Power", unit="watt", decimals=0),
               ov("Idle for", unit="dtdurations", th=IDLE_TH, color_bg="color-text")],
    sort=("Node", False),
    order=("Node", "GPU", "Job", "User", "Framework", "Util", "SM active", "Tensor", "Memory", "Power",
           "Idle for", "Queue", "Job name")))
y += 9
P += [
    ts("GPU utilization by job", [tgt(on_jobs(UTIL), "{{instance}} GPU{{gpu_index}} · {{jobid}} ({{user}}, {{framework}})")],
       0, y, unit="percent", minv=0, maxv=100),
    ts("Average GPU utilization by user", [tgt(f'avg by (user) ({on_jobs(UTIL)})', "{{user}}")],
       12, y, unit="percent", minv=0, maxv=100),
]
y += 8
P += [
    {"id": nid(), "type": "bargauge", "title": "GPUs per user (now)", "datasource": DS,
     "gridPos": {"x": 0, "y": y, "w": 8, "h": 7},
     "targets": [tgt(f'count by (user) (count by (instance, gpu_uuid, user) ({JOB}))', "{{user}}", instant=True)],
     "fieldConfig": {"defaults": {"unit": "none", "min": 0, "thresholds": thresholds("blue"),
                                  "color": {"mode": "thresholds"}}, "overrides": []},
     "options": {"orientation": "horizontal", "displayMode": "basic", "showUnfilled": True,
                 "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}}},
    {"id": nid(), "type": "piechart", "title": "GPUs by framework (now)", "datasource": DS,
     "gridPos": {"x": 8, "y": y, "w": 8, "h": 7},
     "targets": [tgt(f'count by (framework) (count by (instance, gpu_uuid, framework) ({JOB}))', "{{framework}}", instant=True)],
     "fieldConfig": {"defaults": {"unit": "none", "color": {"mode": "palette-classic"}}, "overrides": []},
     "options": {"pieType": "donut", "legend": {"showLegend": True, "displayMode": "table", "placement": "right",
                                                "values": ["value"]},
                 "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}}},
    {"id": nid(), "type": "bargauge", "title": "GPUs per queue (now)", "datasource": DS,
     "gridPos": {"x": 16, "y": y, "w": 8, "h": 7},
     "targets": [tgt(f'count by (queue) (count by (instance, gpu_uuid, queue) ({JOB}))', "{{queue}}", instant=True)],
     "fieldConfig": {"defaults": {"unit": "none", "min": 0, "thresholds": thresholds("purple"),
                                  "color": {"mode": "thresholds"}}, "overrides": []},
     "options": {"orientation": "horizontal", "displayMode": "basic", "showUnfilled": True,
                 "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}}},
]
y += 7

# ---------------------------------------------------------------- compute (DCGM)
P.append(row("What the GPUs compute (DCGM)", y)); y += 1
PIPES = 'pipe=~"tensor|fp64|fp32|fp16|int"'
P += [
    ts("Compute pipes by job", [tgt(
        f'avg by (jobid, pipe) (hpc_gpu_pipe_active_ratio{{{INST}, {PIPES}}} * on (instance, gpu_uuid) '
        f'group_left (jobid) max by (instance, gpu_uuid, jobid) (hpc_gpu_job_info{{{INST}, user=~"$user"}}))',
        "{{jobid}} · {{pipe}}")], 0, y, unit="percentunit", minv=0, maxv=1,
       desc="Share of time each pipe was busy (DCGM profiling). Tensor ≈ matrix math (AI); fp64 ≈ HPC/scientific"),
    ts("SM active per GPU", [tgt(f'hpc_gpu_sm_active_ratio{{{INST}}}', "{{instance}} GPU{{gpu_index}}")],
       12, y, unit="percentunit", minv=0, maxv=1,
       desc="Fraction of SMs with work; more accurate than utilization, which only says a kernel was running"),
]
y += 8
P += [
    ts("GPU memory used", [tgt(on_jobs(f'hpc_gpu_memory_used_bytes{{{INST}}}'),
                               "{{instance}} GPU{{gpu_index}} · {{jobid}}")], 0, y, w=8, unit="bytes", minv=0,
       desc="High memory with 0 % utilization = model loaded but no requests"),
    ts("Power draw", [tgt(f'hpc_gpu_power_draw_watts{{{INST}}}', "{{instance}} GPU{{gpu_index}}")],
       8, y, w=8, unit="watt", minv=0),
    ts("DRAM (HBM) active", [tgt(f'hpc_gpu_dram_active_ratio{{{INST}}}', "{{instance}} GPU{{gpu_index}}")],
       16, y, w=8, unit="percentunit", minv=0, maxv=1),
]
y += 8
P += [
    ts("NVLink traffic per node", [
        tgt(f'sum by (instance) (hpc_gpu_nvlink_tx_bytes_per_second{{{INST}}})', "{{instance}} tx", "A"),
        tgt(f'sum by (instance) (hpc_gpu_nvlink_rx_bytes_per_second{{{INST}}})', "{{instance}} rx", "B")],
       0, y, unit="Bps", minv=0),
    ts("PCIe traffic per node", [
        tgt(f'sum by (instance) (hpc_gpu_pcie_tx_bytes_per_second{{{INST}}})', "{{instance}} tx", "A"),
        tgt(f'sum by (instance) (hpc_gpu_pcie_rx_bytes_per_second{{{INST}}})', "{{instance}} rx", "B")],
       12, y, unit="Bps", minv=0),
]
y += 8

# ---------------------------------------------------------------- health
P.append(row("GPU health", y)); y += 1
P += [
    stat("DCGM up", f'sum(hpc_gpu_dcgm_up{{{INST}}}) / count(hpc_gpu_dcgm_up{{{INST}}})', 0, y,
         unit="percentunit", decimals=0, th=thresholds("red", 1, "green"),
         desc="Share of nodes reading DCGM; below 100 % = nvidia-smi fallback somewhere"),
    stat("XID errors", f'count(hpc_gpu_last_xid{{{INST}}} != 0) or vector(0)', 4, y,
         th=thresholds("green", 1, "red"), desc="Last XID error code is not 0 (since nv-hostengine started)"),
    stat("ECC uncorr.", f'sum(increase(hpc_gpu_ecc_errors_uncorrected_total{{{INST}}}[$__range])) or vector(0)',
         8, y, decimals=0, th=thresholds("green", 1, "red")),
    stat("Remap failures", f'sum(hpc_gpu_row_remap_failure{{{INST}}}) or vector(0)', 12, y,
         th=thresholds("green", 1, "red"), desc="GPU needs a reset or RMA"),
    stat("Too hot now", f'count(rate(hpc_gpu_thermal_violation_seconds_total{{{INST}}}[5m]) > 0) or vector(0)',
         16, y, th=thresholds("green", 1, "red"), desc="GPUs slowed down because they are too hot"),
    stat("Unreadable", f'sum(hpc_gpu_jobmap_unreadable_processes{{{INST}}}) or vector(0)', 20, y,
         th=thresholds("green", 1, "orange"), desc="Above 0: exporter not root, job mapping incomplete"),
]
y += 4
P += [
    ts("GPU / HBM temperature (max per node)", [
        tgt(f'max by (instance) (hpc_gpu_temperature_celsius{{{INST}}})', "{{instance}} GPU", "A"),
        tgt(f'max by (instance) (hpc_gpu_memory_temperature_celsius{{{INST}}})', "{{instance}} HBM", "B")],
       0, y, w=8, unit="celsius"),
    ts("Power-capped time", [tgt(f'rate(hpc_gpu_power_violation_seconds_total{{{INST}}}[5m])',
                                 "{{instance}} GPU{{gpu_index}}")], 8, y, w=8, unit="percentunit", minv=0, maxv=1,
       desc="Fraction of time clocks were lowered to stay under the power limit. Normal under heavy load"),
    ts("Thermal-throttled time", [tgt(f'rate(hpc_gpu_thermal_violation_seconds_total{{{INST}}}[5m])',
                                      "{{instance}} GPU{{gpu_index}}")], 16, y, w=8, unit="percentunit", minv=0, maxv=1,
       desc="Fraction of time clocks were lowered because the GPU was too hot. Should be 0"),
]
y += 8

# ---------------------------------------------------------------- filesystems
P.append(row("Shared filesystems", y)); y += 1
SH = f'{INST}, mount_type=~"nfs|parallel|smb|fuse"'
P += [
    stat("Hung mounts", f'count(hpc_fs_responsive{{{SH}}} == 0) or vector(0)', 0, y,
         th=thresholds("green", 1, "red"), desc="statfs did not answer within FS_STAT_TIMEOUT"),
    stat("Missing mounts", f'count(hpc_fs_mounted{{{INST}}} == 0) or vector(0)', 4, y,
         th=thresholds("green", 1, "red")),
    stat("Read-only", f'count(hpc_fs_readonly{{{SH}}} == 1) or vector(0)', 8, y,
         th=thresholds("green", 1, "orange")),
]
P.append(table("Shared mounts (now)", [
    tgt(f'max by (instance, mountpoint, fstype, mount_type, server) (hpc_fs_info{{{SH}}})', ref="A", instant=True, table=True),
    tgt(f'max by (instance, mountpoint) (hpc_fs_responsive{{{SH}}})', ref="B", instant=True, table=True),
    tgt(f'max by (instance, mountpoint) (hpc_fs_stat_duration_seconds{{{INST}}}) and on (instance, mountpoint) hpc_fs_responsive{{{SH}}}',
        ref="C", instant=True, table=True),
    tgt(f'max by (instance, mountpoint) (hpc_disk_used_bytes{{{INST}}} / hpc_disk_total_bytes{{{INST}}}) and on (instance, mountpoint) hpc_fs_responsive{{{SH}}}',
        ref="D", instant=True, table=True)],
    12, y, w=12, h=8,
    rename={"instance": "Node", "mountpoint": "Mount", "fstype": "FS", "mount_type": "Type", "server": "Server",
            "Value #B": "Responding", "Value #C": "statfs time", "Value #D": "Used"},
    hide=("Value #A",),
    order=("Node", "Mount", "FS", "Type", "Server", "Responding", "statfs time", "Used"),
    overrides=[ov("Responding", color_bg="color-background", th=thresholds("red", 1, "green"),
                  mappings=[{"type": "value", "options": {"0": {"text": "HUNG"}, "1": {"text": "ok"}}}]),
               ov("statfs time", unit="s", decimals=3),
               ov("Used", unit="percentunit", decimals=0, color_bg="gauge", th=thresholds("green", 0.8, "orange", 0.9, "red"))],
    sort=("Mount", False)))
P.append(ts("NFS average round-trip time", [tgt(
    f'sum by (instance, mountpoints) (rate(hpc_nfs_rpc_rtt_milliseconds_total{{{INST}}}[5m])) / '
    f'sum by (instance, mountpoints) (rate(hpc_nfs_rpc_ops_total{{{INST}}}[5m]))', "{{instance}} {{mountpoints}}")],
    0, y + 4, w=12, h=4, unit="ms", minv=0))
y += 8
P += [
    ts("statfs time per mount", [tgt(f'hpc_fs_stat_duration_seconds{{{INST}}} and on (instance, mountpoint) hpc_fs_responsive{{{SH}}}',
                                     "{{instance}} {{mountpoint}}")], 0, y, unit="s", minv=0,
       desc="Time for a df-style call on each shared mount; a jump means the server is slow"),
    ts("NFS operations and retransmissions", [
        tgt(f'sum by (instance, mountpoints) (rate(hpc_nfs_rpc_ops_total{{{INST}}}[5m]))', "{{instance}} {{mountpoints}} ops/s", "A"),
        tgt(f'sum by (instance, mountpoints) (rate(hpc_nfs_rpc_retransmissions_total{{{INST}}}[5m]))', "{{instance}} {{mountpoints}} retrans/s", "B")],
       12, y, unit="ops"),
]
y += 8

# ---------------------------------------------------------------- node (collapsed)
node = [
    ts("CPU usage", [tgt(f'hpc_cpu_usage_percent{{{INST}}}', "{{instance}}")], 0, y + 1, w=8, unit="percent", minv=0, maxv=100),
    ts("Memory used", [tgt(f'hpc_memory_used_bytes{{{INST}}} / hpc_memory_total_bytes{{{INST}}}', "{{instance}}")],
       8, y + 1, w=8, unit="percentunit", minv=0, maxv=1),
    ts("Load (1 min)", [tgt(f'hpc_load_average{{{INST}, period="1m"}}', "{{instance}}")], 16, y + 1, w=8),
    stat("PBS mom down", f'count(hpc_pbs_mom_up{{{INST}}} == 0) or vector(0)', 0, y + 9,
         th=thresholds("green", 1, "red")),
    stat("Centrify DC down", f'count(hpc_centrify_dc_reachable{{{INST}}} == 0) or vector(0)', 4, y + 9,
         th=thresholds("green", 1, "red")),
    stat("Collector errors", f'sum(hpc_collector_error{{{INST}}}) or vector(0)', 8, y + 9,
         th=thresholds("green", 1, "orange")),
    stat("OOM kills (range)", f'sum(increase(hpc_oom_kills_total{{{INST}}}[$__range])) or vector(0)', 12, y + 9,
         decimals=0, th=thresholds("green", 1, "orange")),
    stat("Scrape time", f'max(scrape_duration_seconds{{job=~"$exporter_job", {INST}}})', 16, y + 9,
         unit="s", decimals=1, th=thresholds("green", 15, "orange", 25, "red"),
         desc="Prometheus scrape_timeout is 30 s"),
    stat("Exporter down", f'count(up{{job=~"$exporter_job", {INST}}} == 0) or vector(0)', 20, y + 9,
         th=thresholds("green", 1, "red")),
]
P.append(row("Node", y, collapsed=True, panels=node))


def var_query(name, label, query, multi=True, include_all=True):
    return {"type": "query", "name": name, "label": label, "datasource": DS,
            "query": {"query": query, "refId": "PrometheusVariableQueryEditor-VariableQuery"},
            "definition": query, "refresh": 2, "multi": multi, "includeAll": include_all,
            "allValue": ".*", "sort": 1, "current": {"selected": True, "text": ["All"], "value": ["$__all"]}}


def var_custom(name, label, values, default):
    opts = [{"text": v, "value": v, "selected": v == default} for v in values]
    return {"type": "custom", "name": name, "label": label, "query": ",".join(values), "options": opts,
            "current": {"selected": True, "text": default, "value": default}, "multi": False,
            "includeAll": False}


dash = {
    "title": "hexporter: GPU jobs & idle detection",
    "uid": "hexporter-gpu",
    "description": "hexporter v1: GPU -> PBS job -> user, idle detection, DCGM pipes, health, shared filesystems",
    "tags": ["hexporter", "gpu", "pbs", "hpc"],
    "timezone": "browser",
    "editable": True,
    "graphTooltip": 1,
    "refresh": "1m",
    "schemaVersion": 39,
    "time": {"from": "now-6h", "to": "now"},
    "templating": {"list": [
        {"type": "datasource", "name": "datasource", "label": "Prometheus", "query": "prometheus",
         "current": {}, "hide": 0},
        {"type": "custom", "name": "exporter_job", "label": "Prometheus job", "query": "hexporter",
         "current": {"text": "hexporter", "value": "hexporter"}, "hide": 2,
         "options": [{"text": "hexporter", "value": "hexporter", "selected": True}]},
        var_query("instance", "Node", "label_values(hpc_gpu_utilization_percent, instance)"),
        var_query("user", "User", 'label_values(hpc_gpu_job_info{instance=~"$instance"}, user)'),
        var_custom("idle_minutes", "Idle after (min)", ["30", "60", "120", "240", "480"], "60"),
        var_custom("idle_threshold", "Idle if util ≤ (%)", ["0", "1", "5", "10"], "0"),
    ]},
    "annotations": {"list": [{"builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"},
                              "enable": True, "hide": True, "iconColor": "rgba(0, 211, 255, 1)",
                              "name": "Annotations & Alerts", "type": "dashboard"}]},
    "panels": P,
}

print(json.dumps(dash, indent=2, ensure_ascii=False))
