# syntax=docker/dockerfile:1
# The billing operator: the observer that sends usage to Metronome
# (docs/billing-operator.md).
#
# Build context: the REPOSITORY ROOT, as for images/policy-operator:
#
#   docker build -f images/billing-operator/Dockerfile .
#
# Dockerfile.dockerignore, beside this file, takes the place of the root
# .dockerignore for this build and admits only what is copied below. The
# build also works with this directory as its context
# (docker build images/billing-operator): nothing outside it is copied.

# python:3.12-slim, the policy operator's base.
FROM python:3.12-slim@sha256:dddfd7e07f9d15aeeca61529320492139d21cac7f0070c00609243e51e4e0016

# From either context: images/billing-operator/ under the root, or the
# directory itself.
COPY . /src
RUN set -eu; \
    src=/src; if [ -d /src/images/billing-operator ]; then src=/src/images/billing-operator; fi; \
    pip install --no-cache-dir --require-hashes --no-deps -r "$src/requirements.txt"; \
    mkdir -p /app && cp -R "$src/billing_operator" /app/billing_operator; \
    chmod -R a+rX /app; \
    rm -rf /src

# USER: kopf asks getpass.getuser() for its identity, and uid 65532 has no
# passwd entry here; without a name in the environment it dies at start.
# The switch (BILLING) is not set here: it is the Deployment's, and unset
# the operator is idle. Neither is Metronome's token: it is the Secret's.
ENV PYTHONPATH=/app \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    USER=billing-operator

# The same code answers at build time: the image does not exist if the
# package does not import, or an hour awake is not counted as 3600 seconds.
RUN cd /app && python -m billing_operator selfcheck

USER 65532:65532
WORKDIR /app
EXPOSE 8081
# deploy.md, "Workloads".
ENTRYPOINT ["kopf", "run", "--standalone", "--namespace=browserjs-sessions", "--liveness=http://0.0.0.0:8081/healthz", "-m", "billing_operator"]
