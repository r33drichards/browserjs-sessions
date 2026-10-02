"""The kopf handlers. `kopf run -m policy_operator` imports them.

Each is a thin layer over operator.py and status.py, so that they can be
called as functions in tests.
"""
from __future__ import annotations

import copy
import hashlib
import logging

import kopf

from . import kube, server, status as st
from .config import GROUP, PLURAL, VERSION, Config
from .operator import Operator

log = logging.getLogger("policy_operator")
RESOURCE = (GROUP, VERSION, PLURAL)

# How long a reconcile waits for the replicas to load what it published
# before leaving it to the timer. They long-poll: it takes milliseconds.
LOADED_WAIT_SECONDS = 5.0
LOADED_INTERVAL_SECONDS = 5.0

# Set at startup; tests set them directly.
OPERATOR: Operator | None = None
_runner = None


class SpecDigestDiffBase(kopf.AnnotationsDiffBaseStorage):
    """kopf remembers the last spec it handled in an annotation, to tell
    what changed. spec.source can be 64 KiB, and more once JSON-escaped: with
    the source itself in the annotation a large policy could pass the limit
    on annotations, and its resource could never be handled. Its digest
    tells a change just as well.
    """

    def build(self, *, body, extra_fields=None):
        essence = copy.deepcopy(dict(super().build(body=body, extra_fields=extra_fields)))
        spec = essence.get("spec")
        if isinstance(spec, dict) and isinstance(spec.get("source"), str):
            spec["source"] = "sha256:" + hashlib.sha256(spec["source"].encode("utf-8")).hexdigest()
        return essence


def ready_addresses(slice_body: dict, default_port: int) -> list[str]:
    """host:port of every ready endpoint of an EndpointSlice."""
    ports = [p.get("port") for p in slice_body.get("ports") or [] if p.get("port")]
    port = ports[0] if ports else default_port
    v6 = slice_body.get("addressType") == "IPv6"
    out = []
    for endpoint in slice_body.get("endpoints") or []:
        # The API: an absent `ready` means ready.
        if (endpoint.get("conditions") or {}).get("ready") is False:
            continue
        for address in endpoint.get("addresses") or []:
            out.append(f"[{address}]:{port}" if v6 else f"{address}:{port}")
    return out


def addresses_of(index) -> list[str]:
    """Every address in the EndpointSlice index, whatever slice it is from."""
    out: list[str] = []
    for key in list(index or {}):
        for addresses in index[key]:
            out += addresses
    return sorted(set(out))


@kopf.on.startup()
async def startup(settings: kopf.OperatorSettings, **_):
    global OPERATOR, _runner
    cfg = Config.from_env()
    for name in ("bundle_token", "opa_token", "api_token"):
        if not getattr(cfg, name):
            raise kopf.PermanentError(f"{name} is not set (the Secret policy-tokens, deploy.md)")
    settings.persistence.finalizer = f"{GROUP}/policy-operator"
    settings.persistence.progress_storage = kopf.AnnotationsProgressStorage(prefix=f"policy-operator.{GROUP}")
    settings.persistence.diffbase_storage = SpecDigestDiffBase(prefix=f"policy-operator.{GROUP}")
    settings.posting.level = logging.WARNING
    # kopf logs every handler call of every resource at INFO, the timer's too.
    logging.getLogger("kopf.objects").setLevel(logging.WARNING)
    # kopf would otherwise list and watch the cluster's namespaces, which the
    # operator's Role does not allow (deploy.md): kopf 1.44 retries that 403
    # nine times, for over a minute, before it handles anything. The
    # namespace is the one named on the command line; the resources are
    # looked up once, at start.
    settings.scanning.disabled = True
    OPERATOR = Operator(cfg)
    # Listening before the first pass: OPA is answered 503, and keeps what it has.
    _runner = await server.start(OPERATOR)
    await OPERATOR.first_pass(await kube.list_session_policies(cfg.namespace))


@kopf.on.cleanup()
async def cleanup(**_):
    if _runner is not None:
        await server.stop(_runner, OPERATOR)
    if OPERATOR is not None:
        await OPERATOR.close()


@kopf.index("discovery.k8s.io", "v1", "endpointslices",
            labels={"kubernetes.io/service-name": Config.from_env().opa_service})
def opa_endpoints(name, body, **_):
    return {name: ready_addresses(dict(body), Config.from_env().opa_port)}


@kopf.on.resume(*RESOURCE)
@kopf.on.create(*RESOURCE)
@kopf.on.update(*RESOURCE, field="spec")
async def reconcile(name, spec, status, meta, patch, opa_endpoints=None, **_):
    op = OPERATOR
    generation = meta.get("generation", 0)
    outcome = await op.reconcile(name, dict(spec), dict(status))
    policy_hash = outcome.tenant.hash if outcome.tenant else None
    loaded = await op.wait_loaded(name, policy_hash, lambda: addresses_of(opa_endpoints), LOADED_WAIT_SECONDS)
    patch.status.update(st.after_reconcile(dict(status), generation, outcome, loaded))
    if not outcome.validation.ok:
        log.info("%s: generation %s does not compile: %s", name, generation, outcome.validation.errors[:1])


@kopf.timer(*RESOURCE, interval=LOADED_INTERVAL_SECONDS, initial_delay=LOADED_INTERVAL_SECONDS)
async def loaded(name, status, meta, patch, opa_endpoints=None, **_):
    """New replicas, restarted replicas, a bundle that took longer than the
    reconcile waited: Loaded and Ready follow what the replicas serve."""
    op = OPERATOR
    generation = meta.get("generation", 0)
    if status.get("observedGeneration") != generation:
        return  # the reconcile of this generation has not written yet
    found = await op.loaded(name, status.get("hash"), addresses_of(opa_endpoints), max_age=1.0)
    change = st.after_loaded_check(dict(status), generation, found)
    if change:
        patch.status.update(change)


@kopf.on.delete(*RESOURCE)
async def delete(name, **_):
    await OPERATOR.remove(name)


@kopf.on.event(*RESOURCE)
async def gone(event, name, **_):
    # A resource that left without its finalizer being honoured (removed by
    # hand, say) is still out of the bundle.
    if event.get("type") == "DELETED" and OPERATOR is not None:
        await OPERATOR.remove(name)
