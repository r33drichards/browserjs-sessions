"""Starting: the list of what exists, the first pass, and kopf's settings."""
import kopf
import pytest
from aiohttp import web

from policy_operator import handlers, kube, server
from policy_operator.check import policy_hash

from conftest import ALLOW_ALL, CONTRACTS, DENY_ALL, free_port, resource


async def test_list_session_policies_follows_continue_tokens(aiohttp_server):
    pages = {None: {"items": [resource("s-aaaaa", "rego", ALLOW_ALL)], "metadata": {"continue": "next"}},
             "next": {"items": [resource("s-bbbbb", "rego", DENY_ALL)], "metadata": {}}}
    seen = []

    async def handle(request):
        seen.append((request.path, request.headers.get("Authorization"), request.query.get("limit")))
        return web.json_response(pages[request.query.get("continue")])

    app = web.Application()
    app.router.add_get("/apis/browserjs.dev/v1alpha1/namespaces/browserjs-sessions/sessionpolicies", handle)
    api = await aiohttp_server(app)
    items = await kube.list_session_policies("browserjs-sessions", f"http://127.0.0.1:{api.port}", "sa-token", False)
    assert [i["metadata"]["name"] for i in items] == ["s-aaaaa", "s-bbbbb"]
    assert seen == [("/apis/browserjs.dev/v1alpha1/namespaces/browserjs-sessions/sessionpolicies", "Bearer sa-token", "500")] * 2


async def test_list_fails_loudly_when_the_api_refuses(aiohttp_server):
    app = web.Application()
    api = await aiohttp_server(app)
    with pytest.raises(Exception):
        await kube.list_session_policies("browserjs-sessions", f"http://127.0.0.1:{api.port}", "t", False)


@pytest.fixture
def environment(cfg, monkeypatch):
    monkeypatch.setenv("OPA_BIN", cfg.opa_bin)
    monkeypatch.setenv("POLICY_CONTRACT_DIR", str(CONTRACTS))
    monkeypatch.setenv("HTTP_PORT", str(free_port()))
    for name, value in (("BUNDLE_TOKEN", "b"), ("OPA_TOKEN", "o"), ("OPERATOR_API_TOKEN", "a")):
        monkeypatch.setenv(name, value)
    monkeypatch.setattr(handlers, "OPERATOR", None)
    monkeypatch.setattr(handlers, "_runner", None)
    return monkeypatch


async def test_startup_serves_then_takes_in_what_exists(environment, aiohttp_client):
    states = []

    async def listing(namespace):
        # By the time the API server is asked, the HTTP API is up and says "not yet".
        states.append((namespace, handlers.OPERATOR.ready, handlers._runner is not None))
        return [resource("s-aaaaa", "rego", ALLOW_ALL)]

    environment.setattr(kube, "list_session_policies", listing)
    settings = kopf.OperatorSettings()
    try:
        await handlers.startup(settings=settings)
        op = handlers.OPERATOR
        assert states == [("browserjs-sessions", False, True)]
        assert op.ready and op.sessions["s-aaaaa"].tenant.hash == policy_hash(ALLOW_ALL)
        assert settings.persistence.finalizer == "browserjs.dev/policy-operator"
        assert isinstance(settings.persistence.diffbase_storage, handlers.SpecDigestDiffBase)
        assert isinstance(settings.persistence.progress_storage, kopf.AnnotationsProgressStorage)
        client = await aiohttp_client(server.make_app(op))
        assert (await client.get("/readyz")).status == 200
    finally:
        await handlers.cleanup()


async def test_startup_fails_and_nothing_is_ready_when_the_list_fails(environment):
    async def listing(namespace):
        raise RuntimeError("the API server is away")

    environment.setattr(kube, "list_session_policies", listing)
    try:
        with pytest.raises(RuntimeError):
            await handlers.startup(settings=kopf.OperatorSettings())
        assert not handlers.OPERATOR.ready and handlers.OPERATOR.publisher.body is None
    finally:
        await handlers.cleanup()


@pytest.mark.parametrize("missing", ["BUNDLE_TOKEN", "OPA_TOKEN", "OPERATOR_API_TOKEN"])
async def test_startup_refuses_to_run_without_a_token(environment, missing):
    environment.delenv(missing)
    with pytest.raises(kopf.PermanentError):
        await handlers.startup(settings=kopf.OperatorSettings())
    assert handlers.OPERATOR is None
