# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.22 AS builder

WORKDIR /build

# Cache dependency downloads separately from source changes.
COPY go.mod .
ENV GOFLAGS="-mod=mod"
ENV GONOSUMDB="*"
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o hpc-exporter .

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM debian:bookworm-slim

# ipmitool  – IPMI power/SEL (needs SYS_RAWIO cap)
# lm-sensors – hwmon sensor support
# infiniband-diags – ibstat, perfquery for IB metrics
# Note: centrifydc/adinfo are NOT packageable; bind-mount from the host instead.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ipmitool \
        lm-sensors \
        infiniband-diags \
    && rm -rf /var/lib/apt/lists/*

# Run as non-root.  Collectors that require elevated access:
#   ipmitool    → cap_add: SYS_RAWIO  (set in docker-compose)
#   /proc, /sys → mounted read-only from host
#   nvidia-smi  → NOT installed here.  The NVIDIA Container Toolkit (runtime: nvidia
#                 in docker-compose) bind-mounts the host driver + nvidia-smi binary
#                 into the container automatically.  No image changes needed.
RUN useradd -r -u 65534 -g nogroup -s /bin/false hpc-exporter
USER hpc-exporter

COPY --from=builder /build/hpc-exporter /usr/local/bin/hpc-exporter

EXPOSE 9009

ENTRYPOINT ["/usr/local/bin/hpc-exporter"]
