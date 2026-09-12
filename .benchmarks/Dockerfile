# Copyright 2026 Patrick J. Scruggs
# SPDX-License-Identifier: Apache-2.0
# The submission script records the resolved base/image digests and binary hashes.
FROM python:3.12-slim-bookworm
WORKDIR /opt/bench
COPY bin/ bin/
COPY provenance.json run_matrix.py ./
RUN chmod 0555 bin/*
ENV GOMAXPROCS=2 PYTHONUNBUFFERED=1
ENTRYPOINT ["python3", "/opt/bench/run_matrix.py"]
