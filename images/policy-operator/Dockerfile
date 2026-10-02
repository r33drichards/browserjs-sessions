# syntax=docker/dockerfile:1
# The policy operator: SessionPolicy resources into the one bundle the
# shared OPA serves (docs/policy-operator.md).
#
# Build context: the REPOSITORY ROOT, not this directory, because the
# contract files are copied from docs/contracts/policy/ and exist nowhere
# else:
#
#   docker build -f images/policy-operator/Dockerfile .
#
# Dockerfile.dockerignore, beside this file, takes the place of the root
# .dockerignore for this build and admits only what is copied below.

# OPA 1.9.0, static: the version the opa Deployment runs (deploy.md). The
# two are changed together, with capabilities.json regenerated.
FROM openpolicyagent/opa:1.9.0-static@sha256:60b6af32b58377718546ac7d4634eecbfe50ec36f7d3ca3f8ebf515f9826c2ac AS opa

# python:3.12-slim
FROM python:3.12-slim@sha256:dddfd7e07f9d15aeeca61529320492139d21cac7f0070c00609243e51e4e0016

COPY images/policy-operator/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes --no-deps -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt

COPY --from=opa /opa /usr/local/bin/opa

# The contract files the operator reads, copied and not duplicated.
COPY docs/contracts/policy/capabilities.json \
     docs/contracts/policy/decision-module.rego.tmpl \
     docs/contracts/policy/json-policy.schema.json \
     /contracts/

COPY images/policy-operator/policy_operator /app/policy_operator

# /tmp must be writable: every opa run works in a temporary directory there.
ENV PYTHONPATH=/app \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    POLICY_CONTRACT_DIR=/contracts \
    OPA_BIN=/usr/local/bin/opa

# The same policy code answers at build time: the image does not exist if
# OPA, the contract files and the package do not fit together.
RUN opa version \
    && cd /app && python -c "import json, policy_operator.check as c, policy_operator.config as k; \
v = c.check(k.Config.from_env(), 'json', json.dumps({'version': 1, 'allow': {'operations': ['*']}})); \
assert v.ok, v.errors; print(v.hash)"

USER 65532:65532
WORKDIR /app
EXPOSE 8080 8081
# deploy.md, "Command".
ENTRYPOINT ["kopf", "run", "--standalone", "--namespace=browserjs-sessions", "--liveness=http://0.0.0.0:8081/healthz", "-m", "policy_operator"]
