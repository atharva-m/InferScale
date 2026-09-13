ARG VLLM_IMAGE=vllm/vllm-openai:v0.23.0@sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f
FROM ${VLLM_IMAGE}

COPY --chmod=0555 entrypoint.sh /opt/inferscale/entrypoint.sh
ENTRYPOINT ["/opt/inferscale/entrypoint.sh"]
