# Build for the GPU nodes (x86_64):
#   docker build --platform linux/amd64 -t hpc-exporter:1.0.0 .
# With BuildKit/buildx the builder stage runs natively and cross-compiles;
# the classic builder falls back to the defaults below.
ARG BUILDPLATFORM=linux/amd64

# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.22 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=1.0.0

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o hpc-exporter .

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM debian:bookworm-slim
ARG TARGETARCH=amd64
# DCGM client (dcgmi). Talks to the host's nv-hostengine on localhost:5555
# (network_mode: host). Keep the major version equal to the host's DCGM.
ARG INSTALL_DCGM=true
ARG DCGM_PACKAGE=datacenter-gpu-manager-4-core

# lm-sensors       – hwmon sensor support
# infiniband-diags – ibstat, perfquery for IB metrics
# centrifydc/adinfo are not packageable; bind-mount them from the host.
# nvidia-smi is not installed here: the NVIDIA Container Toolkit mounts it.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl lm-sensors infiniband-diags \
 && if [ "$INSTALL_DCGM" = "true" ]; then \
        case "$TARGETARCH" in arm64) repo=sbsa ;; *) repo=x86_64 ;; esac; \
        curl -fsSL -o /tmp/cuda-keyring.deb \
          "https://developer.download.nvidia.com/compute/cuda/repos/debian12/${repo}/cuda-keyring_1.1-1_all.deb" \
     && dpkg -i /tmp/cuda-keyring.deb && rm /tmp/cuda-keyring.deb \
     && apt-get update && apt-get install -y --no-install-recommends "$DCGM_PACKAGE"; \
    fi \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/hpc-exporter /usr/local/bin/hpc-exporter

# Runs as root: reading other users' /proc/<pid>/environ (PBS job mapping)
# needs root + CAP_SYS_PTRACE. All host mounts are read-only (docker-compose).
EXPOSE 9009
ENTRYPOINT ["/usr/local/bin/hpc-exporter"]
