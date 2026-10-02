"""Starting and stopping, the switch, the watchdog, and the whole operator
under `kopf run` as deploy.md starts it, against the fake API server."""
import asyncio
import json
import logging
import socket
import threading
import time
import urllib.request
from pathlib import Path

import kopf
import pytest
import yaml
from aiohttp import web
from kopf.testing import KopfRunner

from billing_operator import handlers
from billing_operator.kube import Client
from billing_operator.memory import MemoryKube

from billing_operator.sender import FakeMetronome, Metronome

from conftest import NS, sandbox
from fake_api import FakeAPI, FakeIngest

HERE = Path(__file__).resolve().parents[1]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_until(condition, seconds: float = 20.0) -> bool:
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(0.05)
    return False


@pytest.fixture
def environment(monkeypatch, catalogue_path):
    for name in ("BILLING", "TICK", "MAX_GAP", "BILLING_NAMESPACE", "METRONOME_URL", "METRONOME_API_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("BILLING_CATALOGUE", str(catalogue_path))
    monkeypatch.setattr(handlers, "METER", None)
    monkeypatch.setattr(handlers, "_tasks", [])
    return monkeypatch


# --- the switch ---------------------------------------------------------------------

@pytest.mark.parametrize("billing", [None, "off"])
async def test_with_billing_off_the_operator_is_idle(environment, billing, caplog):
    if billing:
        environment.setenv("BILLING", billing)
    environment.setenv("BILLING_CATALOGUE", "/nowhere/catalogue.yaml")  # not even looked at

    def no_client(cfg):
        raise AssertionError("with BILLING off neither the API server nor Metronome is called")

    environment.setattr(handlers, "make_kube", no_client)
    environment.setattr(handlers, "make_sink", no_client)
    settings = kopf.OperatorSettings()
    with caplog.at_level(logging.INFO, logger="billing_operator"):
        await handlers.startup(settings=settings)
    try:
        assert handlers.METER is None and handlers._tasks == []
        assert "the operator is idle" in caplog.text
        assert handlers.probe() == {"metering": False}
        assert settings.scanning.disabled is True
    finally:
        await handlers.cleanup()


@pytest.mark.parametrize("billing", ["meter", "enforce"])
async def test_with_billing_on_it_meters(environment, billing):
    kube, sink = MemoryKube([sandbox("s-aaaaa")]), FakeMetronome()
    environment.setenv("BILLING", billing)
    environment.setenv("METRONOME_API_TOKEN", "made-up-token")
    environment.setenv("TICK", "1")
    environment.setattr(handlers, "make_kube", lambda cfg: kube)
    environment.setattr(handlers, "make_sink", lambda cfg: sink)
    await handlers.startup(settings=kopf.OperatorSettings())
    try:
        for _ in range(100):
            if handlers.METER.passes:
                break
            await asyncio.sleep(0.02)
        assert kube.lists >= 1 and kube.lease is not None
        probe = handlers.probe()
        assert probe["metering"] is True and probe["passes"] >= 1 and probe["lastPass"]
        assert probe["pending"] == 0 and probe["leaseRenewed"] is True
        assert len(handlers._tasks) == 2 and not any(t.done() for t in handlers._tasks)
    finally:
        tasks = list(handlers._tasks)
        await handlers.cleanup()
    assert all(t.done() for t in tasks) and handlers.METER is None


async def test_startup_refuses_a_catalogue_that_does_not_parse(environment, catalogue_path):
    environment.setenv("BILLING", "meter")
    environment.setenv("METRONOME_API_TOKEN", "made-up-token")
    catalogue_path.write_text("version: 9")
    with pytest.raises(kopf.PermanentError, match="BILLING_CATALOGUE"):
        await handlers.startup(settings=kopf.OperatorSettings())
    assert handlers.METER is None and handlers._tasks == []


@pytest.mark.parametrize("env", [{"BILLING": "yes"}, {"BILLING": "meter", "METRONOME_API_TOKEN": "t", "TICK": "200s"},
                                 {"BILLING": "meter"}])   # the last: no Metronome token
async def test_startup_refuses_a_wrong_configuration(environment, env):
    for name, value in env.items():
        environment.setenv(name, value)
    with pytest.raises(kopf.PermanentError):
        await handlers.startup(settings=kopf.OperatorSettings())
    assert handlers.METER is None


def test_the_sink_in_a_pod_is_metronome(environment):
    from billing_operator.config import Config
    sink = handlers.make_sink(Config.from_env({"BILLING": "meter", "METRONOME_API_TOKEN": "made-up-token"}))
    assert isinstance(sink, Metronome) and sink.url == "https://api.metronome.com/v1/ingest"


# --- the watchdog ---------------------------------------------------------------------

def test_stalled(catalogue_file):
    from billing_operator.passes import Observer
    meter = Observer(MemoryKube(), FakeMetronome(), catalogue_file)
    assert not handlers.stalled(meter, 60, now=meter.beat + 299)
    assert handlers.stalled(meter, 60, now=meter.beat + 301)       # five ticks
    assert not handlers.stalled(meter, 1, now=meter.beat + 299)    # never sooner than five minutes
    assert handlers.stalled(meter, 120, now=meter.beat + 601)


async def test_the_watchdog_stops_the_process_when_the_loop_has_ended(catalogue_file):
    from billing_operator.passes import Observer
    meter = Observer(MemoryKube(), FakeMetronome(), catalogue_file)

    async def dies():
        raise RuntimeError("the loop is gone")

    loop_task = asyncio.create_task(dies())
    await asyncio.gather(loop_task, return_exceptions=True)
    stopped = []
    await asyncio.wait_for(handlers.watchdog(meter, 0.01, loop_task, stop=lambda: stopped.append(True)), 5)
    assert stopped == [True]


async def test_the_watchdog_leaves_a_living_loop_alone(catalogue_file):
    from billing_operator.passes import Observer
    meter = Observer(MemoryKube([sandbox("s-aaaaa")]), FakeMetronome(), catalogue_file)
    loop_task = asyncio.create_task(meter.run(0.01))
    stopped = []
    dog = asyncio.create_task(handlers.watchdog(meter, 0.01, loop_task, stop=lambda: stopped.append(True)))
    await asyncio.sleep(0.1)
    for task in (dog, loop_task):
        task.cancel()
    await asyncio.gather(dog, loop_task, return_exceptions=True)
    assert stopped == [] and meter.passes > 1


# --- the image --------------------------------------------------------------------------

def test_the_image_starts_as_deploy_md_says_and_kopf_can_name_its_user():
    dockerfile = (HERE / "Dockerfile").read_text()
    assert ('ENTRYPOINT ["kopf", "run", "--standalone", "--namespace=browserjs-sessions", '
            '"--liveness=http://0.0.0.0:8081/healthz", "-m", "billing_operator"]') in dockerfile
    # kopf calls getpass.getuser(): uid 65532 has no passwd entry in the image,
    # and without a name in the environment the operator dies at start.
    assert "USER 65532:65532" in dockerfile and "USER=billing-operator" in dockerfile
    assert "BILLING=" not in dockerfile  # the switch is the Deployment's, and off by default
    assert "METRONOME" not in dockerfile  # the token is the Secret's, never the image's


def test_requirements_are_locked_with_hashes():
    wanted = [line.strip() for line in (HERE / "requirements.in").read_text().splitlines()
              if line.strip() and not line.startswith("#")]
    locked = (HERE / "requirements.txt").read_text()
    assert wanted and all(f"\n{w.lower()} \\" in "\n" + locked.lower() for w in wanted)
    assert "--hash=sha256:" in locked


# --- under kopf ---------------------------------------------------------------------------

class Cluster:
    """The fake API server and the fake ingest endpoint on a thread of their
    own: kopf brings its own loop."""

    def __init__(self, sandboxes):
        self.api = FakeAPI(sandboxes)
        self.ingest = FakeIngest()
        self.port, self.ingest_port = free_port(), free_port()
        self.loop = asyncio.new_event_loop()
        self.started = threading.Event()
        self.thread = threading.Thread(target=self._serve, daemon=True)

    def _serve(self):
        asyncio.set_event_loop(self.loop)
        for app, port in ((self.api.app(), self.port), (self.ingest.app(), self.ingest_port)):
            runner = web.AppRunner(app)
            self.loop.run_until_complete(runner.setup())
            self.loop.run_until_complete(web.TCPSite(runner, "127.0.0.1", port).start())
        self.started.set()
        self.loop.run_forever()

    def start(self):
        self.thread.start()
        assert self.started.wait(10)


@pytest.fixture
def cluster(environment, tmp_path):
    c = Cluster([sandbox("s-aaaaa"), sandbox("s-warm1", owner=None)])
    c.start()
    kubeconfig = tmp_path / "kubeconfig"
    kubeconfig.write_text(yaml.safe_dump({
        "apiVersion": "v1", "kind": "Config", "current-context": "fake",
        "clusters": [{"name": "fake", "cluster": {"server": f"http://127.0.0.1:{c.port}"}}],
        "users": [{"name": "fake", "user": {"token": "fake"}}],
        "contexts": [{"name": "fake", "context": {"cluster": "fake", "user": "fake", "namespace": NS}}]}))
    environment.setenv("KUBECONFIG", str(kubeconfig))
    # In a pod this is the ServiceAccount's client; here, the fake's. The sink is the real one, pointed at the fake.
    environment.setattr(handlers, "make_kube", lambda cfg: Client(cfg.namespace, f"http://127.0.0.1:{c.port}", "fake", False))
    environment.setenv("METRONOME_URL", f"http://127.0.0.1:{c.ingest_port}")
    try:
        yield c
    finally:
        c.loop.call_soon_threadsafe(c.loop.stop)


def test_the_observer_under_kopf(cluster, environment):
    environment.setenv("BILLING", "meter")
    environment.setenv("METRONOME_API_TOKEN", "made-up-token")
    environment.setenv("TICK", "1")
    liveness = free_port()
    # deploy.md's command, the liveness endpoint on a free port.
    with KopfRunner(["run", "--standalone", f"--namespace={NS}", f"--liveness=http://127.0.0.1:{liveness}/healthz",
                     "-m", "billing_operator"], timeout=60) as runner:
        assert wait_until(lambda: len(cluster.ingest.events) >= 2, 30), "no usage ever reached the ingest endpoint"
        with urllib.request.urlopen(f"http://127.0.0.1:{liveness}/healthz", timeout=5) as r:
            body = json.loads(r.read())
        assert r.status == 200 and body["meter"]["metering"] is True and body["meter"]["passes"] >= 1
    assert runner.exit_code == 0, runner.output
    assert runner.exception is None
    assert "made-up-token" not in runner.output
    events = list(cluster.ingest.events.values())
    assert all(e["event_type"] == "session.awake" and e["properties"]["session_id"] == "s-aaaaa"   # not the warm one
               and e["customer_id"] == "acct-7615aafcb45bcc853c4ed32cc5539842" for e in events)
    assert all(auth == "Bearer made-up-token" for auth, _ in cluster.ingest.requests)
    # In the namespace it lists Sandboxes and keeps its Lease, and nothing else.
    touched = {(m, p.rsplit("/namespaces/" + NS, 1)[1]) for m, p, _ in cluster.api.requests if "/namespaces/" in p}
    assert touched == {("GET", "/sandboxes"), ("GET", "/leases/billing-observer"), ("POST", "/leases"),
                       ("PUT", "/leases/billing-observer")}
    assert cluster.api.leases["billing-observer"]["spec"]["renewTime"].endswith("Z")


def test_under_kopf_with_billing_off_nothing_is_read_written_or_sent(cluster, environment):
    liveness = free_port()
    with KopfRunner(["run", "--standalone", f"--namespace={NS}", f"--liveness=http://127.0.0.1:{liveness}/healthz",
                     "-m", "billing_operator"], timeout=60) as runner:
        assert wait_until(lambda: _alive(liveness), 30), "the liveness endpoint never answered"
        time.sleep(1.5)
    assert runner.exit_code == 0, runner.output
    assert runner.exception is None
    assert [p for m, p, _ in cluster.api.requests if "/namespaces/" in p] == []
    assert cluster.ingest.requests == [] and cluster.api.leases == {}


def _alive(port: int) -> bool:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=2) as r:
            return r.status == 200 and json.loads(r.read()) == {"meter": {"metering": False}}
    except OSError:
        return False
