ARG TRTLLM_IMAGE=nvcr.io/nvidia/tensorrt-llm/release:1.0.0@sha256:e0d966e2daec1827046dd30c6143b39c947ef06ed3fb9ef9469c83a8643b29d7
FROM ${TRTLLM_IMAGE}

COPY --chmod=0555 entrypoint.sh /opt/inferscale/entrypoint.sh
COPY --chmod=0555 build-engine.sh /opt/inferscale/build-engine
COPY --chmod=0555 metrics_exporter.py /opt/inferscale/metrics_exporter.py
ENTRYPOINT ["/opt/inferscale/entrypoint.sh"]
