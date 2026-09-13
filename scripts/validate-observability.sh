#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "${repo_root}" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
for path in sorted((root / "observability" / "dashboards").glob("*.json")):
    payload = json.loads(path.read_text(encoding="utf-8"))
    assert payload.get("uid") and payload.get("panels"), path
    deployed = root / "deploy" / "base" / "monitoring" / "dashboards" / path.name
    assert payload == json.loads(deployed.read_text(encoding="utf-8")), f"dashboard drift: {path.name}"
    print(f"valid JSON: {path.relative_to(root)}")
try:
    import yaml
except ImportError:
    print("PyYAML unavailable; YAML syntax check skipped")
else:
    for path in sorted((root / "observability").rglob("*.yaml")) + sorted((root / "observability").rglob("*.yml")):
        yaml.safe_load(path.read_text(encoding="utf-8"))
        print(f"valid YAML: {path.relative_to(root)}")
    pairs = {
        "observability/prometheus/prometheus.yml": "deploy/base/monitoring/prometheus.yml",
        "observability/alerts/inferscale.rules.yaml": "deploy/base/monitoring/alerts.yml",
        "observability/otel/collector.yaml": "deploy/base/monitoring/otel-collector.yaml",
        "observability/otel/tempo.yaml": "deploy/base/monitoring/tempo.yaml",
    }
    for source, deployed in pairs.items():
        source_bytes = (root / source).read_bytes()
        deployed_bytes = (root / deployed).read_bytes()
        assert source_bytes == deployed_bytes, f"observability deployment drift: {source}"

    prometheus = yaml.safe_load((root / "observability/prometheus/prometheus.yml").read_text(encoding="utf-8"))
    jobs = {item["job_name"]: item for item in prometheus["scrape_configs"]}
    controller = jobs["inferscale-controller"]
    assert controller.get("honor_labels") is True
    controller_targets = {item.get("target_label") for item in controller["relabel_configs"]}
    assert {"scrape_namespace", "scrape_pod"} <= controller_targets
    assert not {"namespace", "pod"} & controller_targets
    assert any(item.get("action") == "drop" and item.get("regex") == "inferscale-controller"
               for item in jobs["annotated-pods"]["relabel_configs"]), "controller would be scraped twice"
    assert "annotated-services" in jobs
    annotated_targets = {
        item.get("target_label") for item in jobs["annotated-pods"]["relabel_configs"]
    }
    managed_targets = {
        item.get("target_label") for item in jobs["inferscale-managed-services"]["relabel_configs"]
    }
    assert {"namespace", "pod", "service", "tenant", "deployment", "revision", "backend"} <= annotated_targets
    assert {"namespace", "pod", "service", "tenant", "deployment", "revision", "backend"} <= managed_targets
    service_rule = next(
        item
        for item in jobs["inferscale-managed-services"]["relabel_configs"]
        if item.get("target_label") == "service"
    )
    assert service_rule["source_labels"] == ["__meta_kubernetes_service_name"]

    dashboards = {path.stem for path in (root / "observability/dashboards").glob("*.json")}
    assert {"overview", "deployment", "router", "runtime", "gpu", "rollout"} <= dashboards
PY

if command -v promtool >/dev/null 2>&1; then
  promtool check rules "${repo_root}/observability/alerts/inferscale.rules.yaml"
  promtool check config "${repo_root}/observability/prometheus/prometheus.yml"
else
  echo "promtool unavailable; Prometheus semantic validation skipped"
fi
