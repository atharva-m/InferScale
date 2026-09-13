SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

GO ?= go
PYTHON ?= python3
KUBECTL ?= kubectl
CONTROLLER_GEN ?= $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0
BENCH_PYTHONPATH := benchmarks/runner
CRD_FILE := platform.inferscale.io_inferencedeployments.yaml

.PHONY: help dev-up dev-status dev-down baseline-up baseline-down baseline-run generate verify-generated fmt lint test test-go test-python test-shell e2e-local e2e-local-live e2e-local-gpu conformance-static release-check verify-manifests migrate build clean

help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "InferScale targets:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

dev-up: ## Start the local non-GPU k3d control-plane stack.
	./scripts/dev/up.sh

dev-status: ## Check local infrastructure and service health.
	./scripts/dev/status.sh

dev-down: ## Stop the local stack without deleting benchmark data.
	./scripts/dev/down.sh

baseline-up: ## Start direct local vLLM outside Kubernetes.
	./scripts/baseline/up.sh

baseline-down: ## Stop direct local vLLM.
	./scripts/baseline/down.sh

baseline-run: ## Run the non-publishable local baseline scenario.
	PYTHONPATH=$(BENCH_PYTHONPATH) $(PYTHON) -m inferscale_bench run \
		--scenario benchmarks/scenarios/local-smoke.yaml \
		--allow-unauthenticated

generate: ## Regenerate checked-in API/CRD/SQL artifacts.
	$(GO) run ./hack/openapiviews
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/platform/v1alpha1/...
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths=./api/platform/v1alpha1/... output:crd:artifacts:config=api/crds
	cp api/crds/$(CRD_FILE) deploy/base/inferscale/crd/$(CRD_FILE)
	$(CONTROLLER_GEN) rbac:roleName=inferscale-controller paths=./internal/controller/deployment/... output:rbac:artifacts:config=deploy/base/inferscale/generated
	cp observability/prometheus/prometheus.yml deploy/base/monitoring/prometheus.yml
	cp observability/alerts/inferscale.rules.yaml deploy/base/monitoring/alerts.yml
	cp observability/otel/collector.yaml deploy/base/monitoring/otel-collector.yaml
	cp observability/otel/tempo.yaml deploy/base/monitoring/tempo.yaml
	cp observability/dashboards/*.json deploy/base/monitoring/dashboards/
	$(GO) generate ./...

verify-generated: ## Fail when checked-in generated artifacts are stale.
	./hack/verify-generated.sh

fmt: ## Format Go and Python sources.
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')
	$(PYTHON) -m ruff format benchmarks modelcache runtime/trtllm

lint: ## Run static checks without rewriting files.
	$(GO) vet ./...
	$(PYTHON) -m ruff check benchmarks modelcache runtime/trtllm
	$(PYTHON) -m mypy benchmarks modelcache runtime/trtllm/metrics_exporter.py runtime/trtllm/test_metrics_exporter.py

test: test-go test-python test-shell verify-manifests ## Run all hardware-independent tests.

test-go: ## Run Go unit and integration tests.
	$(GO) test -race ./...

test-python: ## Run Python unit tests.
	$(PYTHON) -m pytest benchmarks modelcache runtime/trtllm/test_metrics_exporter.py

test-shell: ## Validate runtime entrypoints and migration gates.
	bash tests/shell/runtime_entrypoints_test.sh
	bash tests/shell/migration_gate_test.sh
	bash tests/shell/benchmark_runner_image_test.sh
	bash tests/shell/control_plane_image_test.sh
	bash tests/shell/local_fake_runtime_test.sh
	bash tests/shell/live_local_stack_test.sh
	bash tests/shell/live_local_gpu_test.sh
	bash tests/shell/remote_release_images_test.sh
	bash tests/shell/platform_dependencies_test.sh

verify-manifests: ## Render every checked-in Kustomize overlay.
	@for overlay in deploy/overlays/*; do $(KUBECTL) kustomize "$$overlay" >/dev/null; done
	./scripts/validate-manifests.sh
	./scripts/validate-observability.sh

e2e-local: ## Run the deterministic in-process control-plane vertical slice.
	$(GO) test -tags=e2e ./tests/e2e/local/...

e2e-local-live: ## Bootstrap k3d and run the live CPU-only local-stack gate.
	./scripts/e2e/live-local-stack.sh

e2e-local-gpu: ## Run the real GPU smoke against an explicitly selected local cluster.
	./scripts/e2e/live-local-gpu.sh

conformance-static: ## Verify rendered Gateway/llm-d contracts without claiming live conformance.
	$(GO) test ./tests/conformance/gateway/...

release-check: verify-generated test e2e-local conformance-static ## Enforce all release gates, including signed-off live conformance evidence.
	$(GO) run ./hack/conformancegate --evidence tests/conformance/gateway/evidence.json

migrate: ## Apply PostgreSQL migrations.
	$(GO) run ./cmd/migrate up

build: ## Build all Go binaries.
	$(GO) build ./cmd/...

clean: ## Remove local build/test outputs only.
	find . -type d \( -name '__pycache__' -o -name '.pytest_cache' -o -name '.mypy_cache' -o -name '.ruff_cache' \) -prune -exec rm -r {} +
	rm -rf bin coverage
