ARG PYTHON_IMAGE=python:3.11.13-slim-bookworm@sha256:86adf8dbadc3d6e82ee5dd2c74bec2e1c2467cdad47886280501df722372d2e1
FROM ${PYTHON_IMAGE}

ARG SOURCE_DATE_EPOCH=0
ARG INFERSCALE_GIT_COMMIT=unknown
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    INFERSCALE_RUNNER_GIT_COMMIT=${INFERSCALE_GIT_COMMIT}

RUN groupadd --gid 65532 inferscale \
    && useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin inferscale

WORKDIR /opt/inferscale-bench
COPY benchmarks/runner/inferscale_bench ./inferscale_bench
RUN pip install --no-cache-dir 'PyYAML==6.0.2' 'guidellm[recommended]==0.7.0'

# Native behavioral configurations may refer to the checked-in workloads.
# Publishable GuideLLM runs use its exact synthetic generator and never accept
# caller-supplied prompt files or download prompts at run time.
COPY benchmarks/workloads /workloads

USER 65532:65532
ENTRYPOINT ["python", "-m", "inferscale_bench"]
