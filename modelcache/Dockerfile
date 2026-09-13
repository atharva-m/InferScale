ARG PYTHON_IMAGE=python:3.11.13-slim-bookworm@sha256:86adf8dbadc3d6e82ee5dd2c74bec2e1c2467cdad47886280501df722372d2e1
FROM ${PYTHON_IMAGE}

ARG SOURCE_DATE_EPOCH=0
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HF_HUB_DISABLE_TELEMETRY=1

RUN groupadd --gid 65532 inferscale \
    && useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin inferscale
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir --requirement requirements.txt
RUN mkdir /app/modelcache
COPY __init__.py entrypoint.py /app/modelcache/
# /tmp is writable in the hardened prefetch Job; the image root is read-only.
ENV HF_HOME=/tmp/huggingface
USER 65532:65532
ENTRYPOINT ["python", "-m", "modelcache.entrypoint"]
