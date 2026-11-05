#!/usr/bin/env bash
# test-dcgm.sh — read ALL GPU metrics from DCGM (replacing nvidia-smi values):
#   basic (util, memory, temp, power, clocks), health (XID, ECC, row remap, PCIe/NVLink errors,
#   power/thermal violation), energy, and profiling pipes (FP64/FP32/FP16/Tensor/INT, SM, DRAM).
# Read-only.  Usage:  bash test-dcgm.sh
set -u
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
hr() { printf '\n=== %s ===\n' "$*"; }

# DCGM field name                               default_id  hexporter metric                           extra label        scale
cat > "$TMP/fields.txt" <<'EOF'
DCGM_FI_DEV_GPU_UTIL                            203  hpc_gpu_utilization_percent                  -                  1
DCGM_FI_DEV_MEM_COPY_UTIL                       204  hpc_gpu_memory_copy_utilization_percent      -                  1
DCGM_FI_DEV_FB_TOTAL                            250  hpc_gpu_memory_total_bytes                   -                  1048576
DCGM_FI_DEV_FB_FREE                             251  hpc_gpu_memory_free_bytes                    -                  1048576
DCGM_FI_DEV_FB_USED                             252  hpc_gpu_memory_used_bytes                    -                  1048576
DCGM_FI_DEV_GPU_TEMP                            150  hpc_gpu_temperature_celsius                  -                  1
DCGM_FI_DEV_MEMORY_TEMP                         140  hpc_gpu_memory_temperature_celsius           -                  1
DCGM_FI_DEV_POWER_USAGE                         155  hpc_gpu_power_draw_watts                     -                  1
DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION            156  hpc_gpu_energy_joules_total                  -                  0.001
DCGM_FI_DEV_SM_CLOCK                            100  hpc_gpu_clock_sm_mhz                         -                  1
DCGM_FI_DEV_MEM_CLOCK                           101  hpc_gpu_clock_mem_mhz                        -                  1
DCGM_FI_DEV_CLOCKS_EVENT_REASONS                112  hpc_gpu_throttle_reason                      reason=active_mask 1
DCGM_FI_DEV_POWER_VIOLATION                     240  hpc_gpu_power_violation_seconds_total        -                  0.000001
DCGM_FI_DEV_THERMAL_VIOLATION                   241  hpc_gpu_thermal_violation_seconds_total      -                  0.000001
DCGM_FI_DEV_XID_ERRORS                          230  hpc_gpu_last_xid                             -                  1
DCGM_FI_DEV_ECC_SBE_VOL_TOTAL                   310  hpc_gpu_ecc_errors_corrected_total           -                  1
DCGM_FI_DEV_ECC_DBE_VOL_TOTAL                   311  hpc_gpu_ecc_errors_uncorrected_total         -                  1
DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS           394  hpc_gpu_remapped_rows                        type=correctable   1
DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS         393  hpc_gpu_remapped_rows                        type=uncorrectable 1
DCGM_FI_DEV_ROW_REMAP_FAILURE                   395  hpc_gpu_row_remap_failure                    -                  1
DCGM_FI_DEV_PCIE_REPLAY_COUNTER                 202  hpc_gpu_pcie_replay_total                    -                  1
DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL   409  hpc_gpu_nvlink_errors_total                  type=crc_flit      1
DCGM_FI_DEV_NVLINK_CRC_DATA_ERROR_COUNT_TOTAL   419  hpc_gpu_nvlink_errors_total                  type=crc_data      1
DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_TOTAL     429  hpc_gpu_nvlink_errors_total                  type=replay        1
DCGM_FI_DEV_NVLINK_RECOVERY_ERROR_COUNT_TOTAL   439  hpc_gpu_nvlink_errors_total                  type=recovery      1
DCGM_FI_PROF_GR_ENGINE_ACTIVE                   1001 hpc_gpu_gr_engine_active_ratio               -                  1
DCGM_FI_PROF_SM_ACTIVE                          1002 hpc_gpu_sm_active_ratio                      -                  1
DCGM_FI_PROF_SM_OCCUPANCY                       1003 hpc_gpu_sm_occupancy_ratio                   -                  1
DCGM_FI_PROF_PIPE_TENSOR_ACTIVE                 1004 hpc_gpu_pipe_active_ratio                    pipe=tensor        1
DCGM_FI_PROF_DRAM_ACTIVE                        1005 hpc_gpu_dram_active_ratio                    -                  1
DCGM_FI_PROF_PIPE_FP64_ACTIVE                   1006 hpc_gpu_pipe_active_ratio                    pipe=fp64          1
DCGM_FI_PROF_PIPE_FP32_ACTIVE                   1007 hpc_gpu_pipe_active_ratio                    pipe=fp32          1
DCGM_FI_PROF_PIPE_FP16_ACTIVE                   1008 hpc_gpu_pipe_active_ratio                    pipe=fp16          1
DCGM_FI_PROF_PCIE_TX_BYTES                      1009 hpc_gpu_pcie_tx_bytes_per_second             -                  1
DCGM_FI_PROF_PCIE_RX_BYTES                      1010 hpc_gpu_pcie_rx_bytes_per_second             -                  1
DCGM_FI_PROF_NVLINK_TX_BYTES                    1011 hpc_gpu_nvlink_tx_bytes_per_second           -                  1
DCGM_FI_PROF_NVLINK_RX_BYTES                    1012 hpc_gpu_nvlink_rx_bytes_per_second           -                  1
DCGM_FI_PROF_PIPE_TENSOR_IMMA_ACTIVE            1013 hpc_gpu_pipe_active_ratio                    pipe=tensor_imma   1
DCGM_FI_PROF_PIPE_TENSOR_HMMA_ACTIVE            1014 hpc_gpu_pipe_active_ratio                    pipe=tensor_hmma   1
DCGM_FI_PROF_PIPE_TENSOR_DFMA_ACTIVE            1015 hpc_gpu_pipe_active_ratio                    pipe=tensor_dfma   1
DCGM_FI_PROF_PIPE_INT_ACTIVE                    1016 hpc_gpu_pipe_active_ratio                    pipe=int           1
EOF

# ---- 1. DCGM present? --------------------------------------------------------
hr "1. DCGM"
echo "host: $(hostname)"
if ! command -v dcgmi >/dev/null; then
  echo "ERROR: dcgmi not found. Install DCGM 4.x (datacenter-gpu-manager-4-*) and start nvidia-dcgm."
  exit 1
fi
dcgmi --version 2>&1 | grep -iE 'version' | head -2
if command -v systemctl >/dev/null; then
  for u in nvidia-dcgm dcgm; do st=$(systemctl is-active "$u" 2>/dev/null) && { echo "service $u: $st"; break; }; done
fi
if ! dcgmi discovery -l > "$TMP/disc.txt" 2>&1; then
  echo "ERROR: dcgmi cannot reach nv-hostengine:"; sed 's/^/  /' "$TMP/disc.txt"; exit 1
fi
grep -E 'GPUs? found' "$TMP/disc.txt" | sed 's/^/  /'
# index -> uuid. DCGM discovery shows it too, but nvidia-smi is simpler and is kept for the process list anyway.
nvidia-smi --query-gpu=index,uuid --format=csv,noheader 2>/dev/null | sed 's/, */\t/' > "$TMP/uuids.tsv"

# ---- 2. Resolve field IDs from this DCGM version ----------------------------
hr "2. Field IDs (resolved by name from 'dcgmi dmon -l'; prof fields checked with 'dcgmi profile -l -i 0')"
norm() { tr 'A-Z' 'a-z' | sed -e 's/^dcgm_fi_dev_//' -e 's/^dcgm_fi_//' -e 's/^dev_//'; }
dcgmi dmon -l 2>/dev/null | awk '{ id=$NF; if (id ~ /^[0-9]+$/) print $1"\t"id }' | while IFS=$'\t' read -r n id; do
  printf '%s\t%s\n' "$(printf '%s' "$n" | norm)" "$id"; done > "$TMP/dmonl.tsv"
dcgmi profile -l -i 0 2>/dev/null | grep -oE '\b1[0-9]{3}\b' | sort -u > "$TMP/profok.txt"
: > "$TMP/use.tsv"     # id metric label scale name
printf '  %-46s %-6s %-6s %s\n' FIELD DEFAULT USED STATUS
while read -r name defid metric label scale; do
  key=$(printf '%s' "$name" | norm)
  found=$(awk -F'\t' -v k="$key" '$1==k{print $2; exit}' "$TMP/dmonl.tsv")
  id=${found:-$defid}; st=ok
  if [ -s "$TMP/dmonl.tsv" ] && [ -z "$found" ]; then st="not listed by dmon -l (trying default id)"; fi
  [ -n "$found" ] && [ "$found" != "$defid" ] && st="ID DIFFERS from default"
  if [ "$id" -ge 1000 ] && [ -s "$TMP/profok.txt" ] && ! grep -qx "$id" "$TMP/profok.txt"; then
    printf '  %-46s %-6s %-6s %s\n' "$name" "$defid" "-" "profiling field not supported on this GPU — skipped"; continue
  fi
  printf '  %-46s %-6s %-6s %s\n' "$name" "$defid" "$id" "$st"
  printf '%s\t%s\t%s\t%s\t%s\n' "$id" "$metric" "$label" "$scale" "$name" >> "$TMP/use.tsv"
done < "$TMP/fields.txt"
ids=$(cut -f1 "$TMP/use.tsv" | paste -sd, -)

# ---- 3. Read once ------------------------------------------------------------
hr "3. Read (dcgmi dmon -e <${ids//,/ } > -c 2 -d 1000, last sample)"
if ! dcgmi dmon -e "$ids" -c 2 -d 1000 > "$TMP/dmon.txt" 2>&1; then
  echo "ERROR: dcgmi dmon failed:"; sed 's/^/  /' "$TMP/dmon.txt" | head -5
  echo "Fix: remove the field IDs reported as bad, or check if profiling is paused (Nsight running)."
  exit 1
fi
awk '$1=="GPU"{last[$2]=$0} END{for(g in last) print last[g]}' "$TMP/dmon.txt" | sort -k2,2n > "$TMP/last.txt"
# long form: gpu  id  value
awk -v ids="$ids" 'BEGIN{n=split(ids,I,",")} {for(i=1;i<=n;i++) print $2"\t"I[i]"\t"$(i+2)}' "$TMP/last.txt" > "$TMP/vals.tsv"
echo "read $(wc -l < "$TMP/last.txt" | tr -d ' ') GPUs x $(awk -F, '{print NF}' <<< "$ids") fields"

# ---- 4. Summary --------------------------------------------------------------
awk -F'\t' '
  FILENAME ~ /use.tsv$/  { nm[$1]=$5; next }
  FILENAME ~ /vals.tsv$/ { g[$1]=1; x=$3; if (x ~ /^-?[0-9.eE+]+$/) v[$1, nm[$2]]=x+0; else na[$1, nm[$2]]=1; next }
  function val(gp, f) { return ((gp, f) in v) ? v[gp, f] : "" }
  function show(gp, f, fmt) { return ((gp, f) in v) ? sprintf(fmt, v[gp, f]) : "N/A" }
  END {
    print "\n=== 4a. GPU basics (replaces nvidia-smi) ==="
    printf "%-4s %-6s %-7s %-10s %-10s %-6s %-7s %-8s %-8s %-8s %s\n","GPU","UTIL%","SM_ACT","MEM_USED","MEM_TOTAL","TEMP","HBM_T","POWER_W","SM_MHZ","ENERGY","WORKLOAD"
    for (gp in g) ord[++ng]=gp+0; asort_n(ord, ng)
    for (i=1;i<=ng;i++) { gp=ord[i]
      nice["fp64"]="FP64"; nice["fp32"]="FP32"; nice["fp16"]="FP16"; nice["int"]="INT"
      best=""; bv=0.05
      split("DCGM_FI_PROF_PIPE_FP64_ACTIVE:FP64 DCGM_FI_PROF_PIPE_FP32_ACTIVE:FP32 DCGM_FI_PROF_PIPE_FP16_ACTIVE:FP16 DCGM_FI_PROF_PIPE_INT_ACTIVE:INT DCGM_FI_PROF_PIPE_TENSOR_HMMA_ACTIVE:TENSOR-HMMA(FP16/BF16) DCGM_FI_PROF_PIPE_TENSOR_IMMA_ACTIVE:TENSOR-IMMA(INT8/INT4) DCGM_FI_PROF_PIPE_TENSOR_DFMA_ACTIVE:TENSOR-DFMA(FP64)", P, " ")
      for (k in P) { split(P[k], q, ":"); x=val(gp, q[1]); if (x!="" && x>bv) { bv=x; best=q[2] } }
      ge=val(gp,"DCGM_FI_PROF_GR_ENGINE_ACTIVE"); sa=val(gp,"DCGM_FI_PROF_SM_ACTIVE")
      if (best!="") w=sprintf("%s %.0f%%", best, bv*100)
      else if (ge!="" && sa!="" && ge>0.5 && sa<0.1) w=sprintf("UNDERUSED (engine %.0f%%, SMs %.0f%%)", ge*100, sa*100)
      else if (val(gp,"DCGM_FI_DEV_GPU_UTIL")=="") w="no data"
      else w="idle"
      printf "%-4s %-6s %-7s %-10s %-10s %-6s %-7s %-8s %-8s %-8s %s\n", gp,
        show(gp,"DCGM_FI_DEV_GPU_UTIL","%d"), show(gp,"DCGM_FI_PROF_SM_ACTIVE","%.2f"),
        show(gp,"DCGM_FI_DEV_FB_USED","%.0fMiB"), show(gp,"DCGM_FI_DEV_FB_TOTAL","%.0fMiB"),
        show(gp,"DCGM_FI_DEV_GPU_TEMP","%dC"), show(gp,"DCGM_FI_DEV_MEMORY_TEMP","%dC"),
        show(gp,"DCGM_FI_DEV_POWER_USAGE","%.0f"), show(gp,"DCGM_FI_DEV_SM_CLOCK","%d"),
        ((gp,"DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION") in v) ? sprintf("%.0fkWh", v[gp,"DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION"]/3.6e9) : "N/A", w }

    print "\n=== 4b. GPU health ==="
    printf "%-4s %-8s %-8s %-8s %-9s %-9s %-10s %-11s %-12s %-12s %s\n","GPU","LAST_XID","ECC_SBE","ECC_DBE","REMAP_COR","REMAP_UNC","REMAP_FAIL","PCIE_REPLAY","NVLINK_ERRS","PWR_VIOL_s","THERM_VIOL_s"
    for (i=1;i<=ng;i++) { gp=ord[i]; nv=""; s=0; any=0
      split("CRC_FLIT CRC_DATA REPLAY RECOVERY", L, " ")
      for (k in L) { x=val(gp, "DCGM_FI_DEV_NVLINK_" L[k] "_ERROR_COUNT_TOTAL"); if (x!="") { s+=x; any=1 } }
      nv = any ? s : "N/A"
      pv=val(gp,"DCGM_FI_DEV_POWER_VIOLATION"); tv=val(gp,"DCGM_FI_DEV_THERMAL_VIOLATION")
      flag=""
      if (val(gp,"DCGM_FI_DEV_XID_ERRORS")+0>0) flag=flag " XID"
      if (val(gp,"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL")+0>0) flag=flag " DBE"
      if (val(gp,"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS")+0>0 || val(gp,"DCGM_FI_DEV_ROW_REMAP_FAILURE")+0>0) flag=flag " REMAP"
      printf "%-4s %-8s %-8s %-8s %-9s %-9s %-10s %-11s %-12s %-12s %-12s %s\n", gp,
        show(gp,"DCGM_FI_DEV_XID_ERRORS","%d"), show(gp,"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL","%d"), show(gp,"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL","%d"),
        show(gp,"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS","%d"), show(gp,"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS","%d"), show(gp,"DCGM_FI_DEV_ROW_REMAP_FAILURE","%d"),
        show(gp,"DCGM_FI_DEV_PCIE_REPLAY_COUNTER","%d"), nv,
        (pv!="" ? sprintf("%.1f", pv/1e6) : "N/A"), (tv!="" ? sprintf("%.1f", tv/1e6) : "N/A"), (flag!="" ? "<- CHECK:" flag : "") }
    print "(LAST_XID = code of the most recent XID error, 0 = none. ECC counts are since driver load.)"
  }
  function asort_n(a, n,   i, j, t) { for (i=2;i<=n;i++) { t=a[i]; for (j=i-1;j>=1 && a[j]>t;j--) a[j+1]=a[j]; a[j+1]=t } }
' "$TMP/use.tsv" "$TMP/vals.tsv"

# ---- 5. Metrics preview ------------------------------------------------------
hr "5. Metrics preview (labels: gpu_index, gpu_uuid; plus pipe/type/reason where shown)"
awk -F'\t' '
  FILENAME ~ /uuids.tsv$/ { uuid[$1]=$2; next }
  FILENAME ~ /use.tsv$/   { m[$1]=$2; lb[$1]=$3; sc[$1]=$4; next }
  FILENAME ~ /vals.tsv$/  { x=$3; if (x !~ /^-?[0-9.eE+]+$/) { na++; next }
    l=sprintf("gpu_index=\"%s\",gpu_uuid=\"%s\"", $1, (($1 in uuid)?uuid[$1]:"-"))
    if (lb[$2]!="-") { split(lb[$2], kv, "="); l=l sprintf(",%s=\"%s\"", kv[1], kv[2]) }
    y=x*sc[$2]; printf "%s{%s} " (y==int(y) ? "%.0f" : "%.10g") "\n", m[$2], l, y }
  END { printf "hpc_gpu_dcgm_up 1\n# %d values were N/A (not emitted)\n", na }' "$TMP/uuids.tsv" "$TMP/use.tsv" "$TMP/vals.tsv" > "$TMP/prom.txt"
lines=$(wc -l < "$TMP/prom.txt" | tr -d ' ')
if [ "${FULL:-0}" = 1 ]; then cat "$TMP/prom.txt"; else
  grep 'gpu_index="0"' "$TMP/prom.txt"; tail -2 "$TMP/prom.txt"
  echo "# showing GPU 0 only; $lines lines in total. Run with FULL=1 to print all."
fi
echo "done."
