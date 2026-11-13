VERSION ?= 1.0.1
IMAGE   ?= hpc-exporter:$(VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test linux package image image-cross save dashboard rules-test clean

build:            ## local binary for this machine
	go build -ldflags "$(LDFLAGS)" -o hpc-exporter .

test:             ## unit tests
	go vet ./... && go test ./...

linux:            ## static linux/amd64 binary in dist/
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/hpc-exporter-linux-amd64 .

PKG := hpc-exporter-$(VERSION)-linux-amd64

package: linux    ## systemd install: binary + setup-hpc-exporter.sh + verify-metrics.sh (+ test tools)
	rm -rf dist/$(PKG) && mkdir -p dist/$(PKG)/tools
	cp dist/hpc-exporter-linux-amd64 dist/$(PKG)/hpc-exporter
	cp deploy/systemd/setup-hpc-exporter.sh tools/verify-metrics.sh dist/$(PKG)/
	cp tools/test-*.sh dist/$(PKG)/tools/
	cp docs/NODE-TEST.md dist/$(PKG)/
	chmod 755 dist/$(PKG)/hpc-exporter dist/$(PKG)/*.sh dist/$(PKG)/tools/*.sh
	COPYFILE_DISABLE=1 tar -C dist -czf dist/$(PKG).tar.gz $(PKG)
	@echo "on the node, as root: bash setup-hpc-exporter.sh install"

image:            ## multi-stage build (x86_64 build host, or BuildKit/buildx)
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE) .

image-cross: linux ## Apple Silicon / no buildx: compile locally, package runtime image
	docker build --platform linux/amd64 -f Dockerfile.runtime -t $(IMAGE) .

save:             ## image tarball to copy to nodes without a registry
	docker save $(IMAGE) | gzip > dist/hpc-exporter-$(VERSION)-amd64.tar.gz
	@echo "on the node: gunzip -c hpc-exporter-$(VERSION)-amd64.tar.gz | docker load"

PROMTOOL ?= docker run --rm -v $(CURDIR)/deploy/prometheus:/w -w /w --entrypoint promtool prom/prometheus

dashboard:        ## regenerate the Grafana dashboard and the recording rules it needs
	python3 deploy/grafana/gen_dashboard.py > deploy/grafana/hexporter-gpu.json
	python3 deploy/grafana/gen_dashboard.py rules > deploy/prometheus/hexporter-recording.yml

rules-test:       ## promtool: check alert + recording rules, run the recording-rule unit tests
	$(PROMTOOL) check rules hexporter-recording.yml hexporter-alerts.yml
	$(PROMTOOL) test rules tests/hexporter-recording.test.yml

clean:
	rm -rf hpc-exporter dist/
