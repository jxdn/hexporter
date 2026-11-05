#!/usr/bin/env bash
# test-gpu-job-map.sh — run on a GPU node to see which PBS job / user is on each GPU.
# Read-only: only reads /proc and runs nvidia-smi (and qstat if present).
#
#   sudo bash test-gpu-job-map.sh            # full report
#   sudo bash test-gpu-job-map.sh > out.txt  # save and send back
#
# Root is needed to read other users' /proc/<pid>/environ.
set -u
PROC=${PROC_ROOT:-/proc}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

hr() { printf '\n=== %s ===\n' "$*"; }

hr "Node"
echo "host:    $(hostname)"
echo "running: $(id -un) (uid $(id -u))"
[ "$(id -u)" -ne 0 ] && echo "WARNING: not root — other users' job environments will be unreadable. Re-run with sudo."
command -v nvidia-smi >/dev/null || { echo "ERROR: nvidia-smi not found"; exit 1; }
nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1 | sed 's/^/driver:  /'

# ---- 1. GPUs -----------------------------------------------------------------
hr "1. GPUs (nvidia-smi --query-gpu)"
nvidia-smi --query-gpu=index,uuid,name,utilization.gpu,memory.used,utilization.memory,power.draw,power.limit \
  --format=csv,noheader,nounits | sed 's/, */\t/g' > "$TMP/gpus.tsv"
awk -F'\t' 'BEGIN{printf "%-4s %-42s %-5s %-9s %-8s %-8s %-8s %s\n","IDX","UUID","UTIL%","MEMBW%","MEM_MiB","POWER_W","LIMIT_W","NAME"}
            {printf "%-4s %-42s %-5s %-9s %-8s %-8s %-8s %s\n",$1,$2,$4,$6,$5,$7,$8,$3}' "$TMP/gpus.tsv"
echo "(UTIL% = time a kernel was running in the last sample period; MEMBW% = memory bandwidth busy)"

# ---- 2. Processes on GPUs ----------------------------------------------------
hr "2. Processes on GPUs (nvidia-smi --query-compute-apps)"
nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_memory \
  --format=csv,noheader,nounits 2>/dev/null | sed 's/, */\t/g' | grep -v '^$' > "$TMP/apps.tsv"
if [ -s "$TMP/apps.tsv" ]; then
  awk -F'\t' 'BEGIN{printf "%-42s %-8s %-8s %s\n","GPU_UUID","PID","MEM_MiB","PROCESS"}
              {printf "%-42s %-8s %-8s %s\n",$1,$2,$4,$3}' "$TMP/apps.tsv"
else
  echo "(no compute processes reported)"
fi

# ---- 3. Scan /proc for PBS jobs ---------------------------------------------
hr "3. PBS jobs found in $PROC/*/environ"
denied=0; scanned=0
: > "$TMP/procs.tsv"   # pid jobid user queue jobname cvd cgroup-token
# PBS cgroup token, e.g. "0::/pbs_jobs.service/jobs/9025.1/1" -> "9025.1"
cgtoken() { sed -n 's#.*pbs_jobs\.service/jobs/\([^/]*\).*#\1#p' "$PROC/$1/cgroup" 2>/dev/null | head -1; }
for d in "$PROC"/[0-9]*; do
  pid=${d##*/}; scanned=$((scanned+1))
  env_file="$d/environ"
  if ! [ -r "$env_file" ] || ! cat "$env_file" >/dev/null 2>&1; then
    denied=$((denied+1)); continue
  fi
  grep -qa 'PBS_JOBID=' "$env_file" 2>/dev/null || continue
  cg=$(cgtoken "$pid")
  tr '\0' '\n' < "$env_file" 2>/dev/null | awk -F= -v pid="$pid" -v cg="${cg:--}" '
    /^PBS_JOBID=/{j=substr($0,11)} /^USER=/{u=substr($0,6)} /^LOGNAME=/{l=substr($0,9)}
    /^PBS_O_LOGNAME=/{o=substr($0,15)} /^PBS_QUEUE=/{q=substr($0,11)}
    /^PBS_JOBNAME=/{n=substr($0,13)} /^CUDA_VISIBLE_DEVICES=/{c=substr($0,22)}
    END{ if(u=="")u=l; if(u=="")u=o; if(c=="")c="-";
         printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",pid,j,u,q,n,c,cg }' >> "$TMP/procs.tsv"
done
echo "scanned $scanned processes, $denied unreadable"

# one row per job (first process that has a CUDA_VISIBLE_DEVICES wins)
awk -F'\t' '{ if(!($2 in seen) || (cvd[$2]=="-" && $6!="-")){seen[$2]=1;u[$2]=$3;q[$2]=$4;n[$2]=$5;cvd[$2]=$6}
              pids[$2]=pids[$2] (pids[$2]?",":"") $1 }
     END{ for(j in seen) printf "%s\t%s\t%s\t%s\t%s\t%s\n",j,u[j],q[j],n[j],cvd[j],pids[j] }' \
  "$TMP/procs.tsv" | sort > "$TMP/jobs.tsv"
if [ -s "$TMP/jobs.tsv" ]; then
  awk -F'\t' 'BEGIN{printf "%-22s %-12s %-12s %-18s %-30s %s\n","JOBID","USER","QUEUE","JOBNAME","CUDA_VISIBLE_DEVICES","PIDS"}
              {p=$6; if(length(p)>40)p=substr(p,1,40)"…"; printf "%-22s %-12s %-12s %-18s %-30s %s\n",$1,$2,$3,$4,$5,p}' "$TMP/jobs.tsv"
else
  echo "(no PBS job processes found)"
fi

# ---- 4. Raw details per job (for checking the mapping) ----------------------
hr "4. Per-job raw details"
while IFS=$'\t' read -r jid user q name cvd pids; do
  pid=${pids%%,*}
  echo "--- $jid  (sample pid $pid)"
  echo "  CUDA_VISIBLE_DEVICES = $cvd"
  case "$cvd" in
    -) echo "  format: NOT SET (no GPU pinning by PBS for this job)";;
    GPU-*|MIG-*) echo "  format: UUID";;
    *) echo "  format: INDEX";;
  esac
  [ -r "$PROC/$pid/cgroup" ] && grep -iE 'pbs|jobid' "$PROC/$pid/cgroup" | head -3 | sed 's/^/  cgroup: /'
  if command -v qstat >/dev/null 2>&1; then
    timeout 10 qstat -f "$jid" 2>/dev/null | grep -E 'euser|exec_host|exec_vnode|ngpus|project' | sed 's/^ */  qstat: /'
  fi
done < "$TMP/jobs.tsv"

# ---- 5. GPU -> job -> user -> framework ------------------------------------
hr "5. GPU -> job -> user -> framework"

# classify TEXT -> inference/training framework (order matters)
classify() {
  local t; t=$(printf '%s' "$1" | tr 'A-Z' 'a-z')
  case "$t" in
    *vllm*|*rayworkerwrapper*)                                   echo vllm ;;
    *sglang*)                                                    echo sglang ;;
    *text-generation-launcher*|*text-generation-server*|*text_generation_server*) echo tgi ;;
    *tritonserver*)                                              echo triton ;;
    *trtllm*|*tensorrt_llm*|*tensorrt-llm*)                      echo tensorrt-llm ;;
    *lmdeploy*)                                                  echo lmdeploy ;;
    *ollama*)                                                    echo ollama ;;
    *llama-server*|*llama_cpp*|*llama.cpp*)                      echo llama.cpp ;;
    *deepspeed.mii*|*mii.serve*)                                 echo deepspeed-mii ;;
    *ray::serve*|*"serve run"*)                                  echo ray-serve ;;
    *jupyter*|*ipykernel*)                                       echo jupyter ;;
    *torchrun*|*deepspeed*|*"accelerate launch"*|*torch.distributed*|*megatron*) echo training ;;
    *)                                                           echo other ;;
  esac
}
cmdline() { tr '\0' ' ' < "$PROC/$1/cmdline" 2>/dev/null | cut -c1-300; }
pcomm()   { awk '/^Name:/{print $2; exit}' "$PROC/$1/status" 2>/dev/null; }
ppid()    { awk '/^PPid:/{print $2; exit}' "$PROC/$1/status" 2>/dev/null; }

# Match every GPU process to a job: 1) its own environ  2) its PBS cgroup  3) a parent process
awk -F'\t' '$7!="-"{print $7"\t"$2}' "$TMP/procs.tsv" | sort -u > "$TMP/cgmap.tsv"
: > "$TMP/appjobs.tsv"   # pid jobid user how framework fw_from cmdline
while IFS=$'\t' read -r uuid pid pname mem; do
  job=$(awk -F'\t' -v p="$pid" '$1==p{print $2; exit}' "$TMP/procs.tsv"); how=environ
  if [ -z "$job" ]; then
    tok=$(cgtoken "$pid")
    if [ -n "$tok" ]; then
      job=$(awk -F'\t' -v t="$tok" '$1==t{print $2; exit}' "$TMP/cgmap.tsv")
      # token not seen on a job process: match on the job number (e.g. 9025.1 -> 9025.*)
      [ -z "$job" ] && job=$(awk -F'\t' -v n="${tok%%.*}" 'index($1,n".")==1||index($1,n"[")==1{print $1; exit}' "$TMP/jobs.tsv")
      how="cgroup($tok)"
    fi
  fi
  if [ -z "$job" ]; then
    cur=$pid
    for _ in $(seq 1 64); do
      cur=$(ppid "$cur")
      { [ -z "$cur" ] || [ "$cur" -le 1 ]; } && break
      job=$(awk -F'\t' -v p="$cur" '$1==p{print $2; exit}' "$TMP/procs.tsv")
      [ -n "$job" ] && { how="parent($cur)"; break; }
    done
  fi
  [ -z "$job" ] && { job=none; how=unmatched; }
  u=$(tr '\0' '\n' < "$PROC/$pid/environ" 2>/dev/null | sed -n 's/^USER=//p' | head -1)
  [ -z "$u" ] && u="uid:$(awk '/^Uid:/{print $2}' "$PROC/$pid/status" 2>/dev/null)"

  # framework: process name + cmdline; if unknown, look at up to 5 parents
  cl=$(cmdline "$pid")
  fw=$(classify "$pname $(pcomm "$pid") $cl"); fwfrom=self
  cur=$pid
  for _ in 1 2 3 4 5; do
    [ "$fw" != other ] && break
    cur=$(ppid "$cur"); { [ -z "$cur" ] || [ "$cur" -le 1 ]; } && break
    fw=$(classify "$(pcomm "$cur") $(cmdline "$cur")"); fwfrom="parent($cur)"
  done
  [ "$fw" = other ] && fwfrom=-
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$pid" "$job" "$u" "$how" "$fw" "$fwfrom" "${cl:--}" >> "$TMP/appjobs.tsv"
done < "$TMP/apps.tsv"

if [ -s "$TMP/appjobs.tsv" ]; then
  echo "GPU processes:"
  awk -F'\t' 'BEGIN{printf "  %-9s %-20s %-10s %-16s %-13s %-14s %s\n","PID","JOBID","USER","MATCHED_BY","FRAMEWORK","FW_FROM","CMDLINE"}
              {c=$7; if(length(c)>70)c=substr(c,1,70)"…"; printf "  %-9s %-20s %-10s %-16s %-13s %-14s %s\n",$1,$2,$3,$4,$5,$6,c}' "$TMP/appjobs.tsv"
  echo
fi

awk -F'\t' -v M="$TMP/metrics.txt" '
  FNR==1 { f=FILENAME; sub(/.*\//,"",f) }
  f=="gpus.tsv"    { idx[NR]=$1; uuid[NR]=$2; idxof[$2]=$1; gname[$2]=$3; util[$2]=$4; mem[$2]=$5; byidx[$1]=$2; ng=NR; next }
  f=="jobs.tsv"    { jn++; J[jn]=$1; JU[$1]=$2; JQ[$1]=$3; JN[$1]=$4; JC[$1]=$5; next }
  f=="appjobs.tsv" { pjob[$1]=$2; puser[$1]=$3; pfw[$1]=$5; next }
  f=="apps.tsv"    { an++; AU[an]=$1; AP[an]=$2; AN[an]=$3; AM[an]=$4; next }
  END {
    # allocation from CUDA_VISIBLE_DEVICES
    for (i=1;i<=jn;i++) { j=J[i]; if (JC[j]=="-") continue
      n=split(JC[j], t, ","); for (k=1;k<=n;k++) { g=t[k]; gsub(/ /,"",g)
        if (g !~ /^(GPU|MIG)-/) g=byidx[g]; if (g=="" || !(g in util)) continue
        key=g SUBSEP j; if (!(key in src)) { order[g]=order[g] j "|" }; src[key]=src[key] "e" } }
    # running processes
    for (i=1;i<=an;i++) { g=AU[i]; p=AP[i]; j=((p in pjob) && pjob[p]!="none")?pjob[p]:"none"
      if (j=="none") { JU["none:" p]=puser[p]; j="none:" p }
      key=g SUBSEP j; if (!(key in src)) { order[g]=order[g] j "|" }
      src[key]=src[key] "p"; procs[key]=procs[key] (procs[key]?",":"") p "(" AN[i] ")"
      fw=(p in pfw)?pfw[p]:"other"; if (index("," fws[key] ",", "," fw ",")==0) fws[key]=fws[key] (fws[key]?",":"") fw
      if (index("," jobfw[j] ",", "," fw ",")==0) jobfw[j]=jobfw[j] (jobfw[j]?",":"") fw
      jl=(j ~ /^none:/)?"none":j
      plines=plines sprintf("hpc_gpu_process_memory_used_bytes{gpu_index=\"%s\",gpu_uuid=\"%s\",pid=\"%s\",process_name=\"%s\",framework=\"%s\",jobid=\"%s\",user=\"%s\"} %d\n", idxof[g], g, p, AN[i], fw, jl, JU[j], AM[i]*1048576) }

    fmt="%-4s %-5s %-8s %-20s %-10s %-10s %-14s %-13s %-6s %-26s %s\n"
    printf fmt,"GPU","UTIL%","MEM_MiB","JOBID","USER","QUEUE","JOBNAME","FRAMEWORK","SOURCE","PROCESSES","STATUS"
    for (i=1;i<=ng;i++) { g=uuid[i]
      if (order[g]=="") { printf fmt,idx[i],util[g],mem[g],"-","-","-","-","-","-","-","FREE"; free++; continue }
      n=split(order[g], js, "|")
      for (k=1;k<n;k++) { j=js[k]; key=g SUBSEP j; s=src[key]
        so=(s ~ /e/ && s ~ /p/)?"both":(s ~ /e/)?"env":"proc"
        if (j ~ /^none:/) { st="NOT PBS"; jid="none"; notpbs++ } else { jid=j; st=(util[g]+0==0)?"ALLOCATED, 0%":"IN USE"; if(st~/0%/)idle++; else busy++ }
        pr=(key in procs)?procs[key]:"-"; fw=(key in fws)?fws[key]:((j in jobfw)?jobfw[j]:"none")
        printf fmt,idx[i],util[g],mem[g],jid,JU[j],(JQ[j]?JQ[j]:"-"),(JN[j]?JN[j]:"-"),fw,so,pr,st
        printf "hpc_gpu_job_info{gpu_index=\"%s\",gpu_uuid=\"%s\",jobid=\"%s\",user=\"%s\",queue=\"%s\",job_name=\"%s\",framework=\"%s\",source=\"%s\"} 1\n", idx[i], g, jid, JU[j], JQ[j], JN[j], fw, so > M } }
    printf "\nsummary: %d GPUs | in use %d | allocated at 0%% %d | free %d | not-PBS %d\n", ng, busy, idle, free, notpbs
    printf "%s", plines > M
  }' "$TMP/gpus.tsv" "$TMP/jobs.tsv" "$TMP/appjobs.tsv" "$TMP/apps.tsv"

# ---- 6. What this tells us --------------------------------------------------
hr "6. Findings"
uuid_jobs=$(awk -F'\t' '$5 ~ /^(GPU|MIG)-/' "$TMP/jobs.tsv" | wc -l | tr -d ' ')
idx_jobs=$(awk -F'\t' '$5 != "-" && $5 !~ /^(GPU|MIG)-/' "$TMP/jobs.tsv" | wc -l | tr -d ' ')
none_jobs=$(awk -F'\t' '$5 == "-"' "$TMP/jobs.tsv" | wc -l | tr -d ' ')
echo "jobs with CUDA_VISIBLE_DEVICES as UUID:  $uuid_jobs"
echo "jobs with CUDA_VISIBLE_DEVICES as index: $idx_jobs"
echo "jobs without CUDA_VISIBLE_DEVICES:       $none_jobs  (CPU jobs, or PBS not pinning GPUs)"
[ "$denied" -gt 0 ] && [ "$(id -u)" -ne 0 ] && echo "NOTE: $denied processes unreadable — run with sudo for a complete result."

# ---- 7. Metrics preview ------------------------------------------------------
hr "7. Metrics preview"
echo "# existing (already in hexporter today)"
awk -F'\t' '{ l=sprintf("gpu_index=\"%s\",gpu_name=\"%s\",gpu_uuid=\"%s\"",$1,$3,$2)
  printf "hpc_gpu_utilization_percent{%s} %s\n", l, $4
  printf "hpc_gpu_memory_used_bytes{%s} %d\n", l, $5*1048576
  if ($7 ~ /^[0-9.]+$/) printf "hpc_gpu_power_draw_watts{%s} %s\n", l, $7 }' "$TMP/gpus.tsv"
echo
echo "# new"
[ -s "$TMP/metrics.txt" ] && cat "$TMP/metrics.txt" || echo "(none)"

# ---- 8. What Grafana gets after the join ------------------------------------
hr "8. Grafana: utilization per job (hpc_gpu_utilization_percent * on(gpu_uuid) group_left(jobid,user,framework) hpc_gpu_job_info)"
awk -F'\t' '
  FNR==1 { f=FILENAME; sub(/.*\//,"",f) }
  f=="gpus.tsv"    { util[$2]=$4; pw[$2]=$7; next }
  f=="metrics.txt" && /^hpc_gpu_job_info/ {
    match($0,/gpu_uuid="[^"]*"/); g=substr($0,RSTART+10,RLENGTH-11)
    match($0,/gpu_index="[^"]*"/); i=substr($0,RSTART+11,RLENGTH-12)
    match($0,/jobid="[^"]*"/); j=substr($0,RSTART+7,RLENGTH-8)
    match($0,/user="[^"]*"/); u=substr($0,RSTART+6,RLENGTH-7)
    match($0,/framework="[^"]*"/); fw=substr($0,RSTART+11,RLENGTH-12)
    printf "{gpu_index=\"%s\",jobid=\"%s\",user=\"%s\",framework=\"%s\"}  util=%s%%  power=%sW\n", i, j, u, fw, util[g], pw[g] }' \
  "$TMP/gpus.tsv" "$TMP/metrics.txt"
echo "done."
