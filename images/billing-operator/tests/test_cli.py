"""python -m billing_operator simulate: the documented command."""
import io
import subprocess
import sys
from pathlib import Path

import yaml

from billing_operator import cli

from conftest import CONTRACTS, acct, sandbox

EXAMPLES = Path(__file__).resolve().parents[1] / "examples"
TIMES = ["2026-10-02T10:00:00Z", "2026-10-02T10:01:00Z", "2026-10-02T10:02:00Z", "2026-10-02T10:05:00Z"]
T0 = 1790935200


def test_simulate_prints_the_events_of_each_tick():
    out = io.StringIO()
    assert cli.main(["simulate", str(EXAMPLES), *TIMES], out) == 0
    docs = list(yaml.safe_load_all(out.getvalue()))
    assert [d["tick"] for d in docs] == TIMES
    customer = "acct-7615aafcb45bcc853c4ed32cc5539842"
    # The 20 s since Ready, in the window that ends at 10:00:00.
    assert docs[0]["events"] == [{"transaction_id": f"awake/s-aaaaa/{T0 - 300}", "customer_id": customer,
                                  "event_type": "session.awake", "timestamp": TIMES[0],
                                  "properties": {"session_id": "s-aaaaa", "seconds": "20"}}]
    assert docs[1]["events"] == [] and "awake_seconds=60 disk_gb_seconds=600 events=0" in docs[1]["pass"]
    # Three minutes later is beyond the gap: the window holds the two minutes that were seen.
    assert [(e["transaction_id"], e["timestamp"], e["properties"]) for e in docs[3]["events"]] == [
        (f"awake/s-aaaaa/{T0}", TIMES[3], {"session_id": "s-aaaaa", "seconds": "120"})]
    assert sorted(docs[3]["sessions"]) == ["s-aaaaa", "s-bbbbb"]  # the warm one is nobody's


def test_simulate_takes_a_catalogue_the_windows_and_a_kubectl_list(tmp_path):
    catalogue = tmp_path / "catalogue.yaml"
    catalogue.write_text((CONTRACTS / "catalogue.yaml").read_text().replace("sessionDiskGB: 5", "sessionDiskGB: 7"))
    directory = tmp_path / "cluster"
    directory.mkdir()
    # As `kubectl get sandboxes -o yaml` writes them.
    (directory / "export.yml").write_text(yaml.safe_dump({"apiVersion": "v1", "kind": "List", "items": [sandbox("s-aaaaa")]}))
    (directory / "notes.txt").write_text("not YAML")
    out = io.StringIO()
    assert cli.main(["simulate", "--catalogue", str(catalogue), "--awake-window", "60", "--kept-window", "120",
                     str(directory), *TIMES[:3]], out) == 0
    docs = list(yaml.safe_load_all(out.getvalue()))
    assert "disk_gb_seconds=420" in docs[1]["pass"] and docs[1]["events"][0]["customer_id"] == acct()
    assert [(e["event_type"], e["properties"]) for e in docs[2]["events"]] == [
        ("session.awake", {"session_id": "s-aaaaa", "seconds": "60"}),
        ("session.kept", {"session_id": "s-aaaaa", "gb_seconds": "840"})]


def test_as_a_command():
    done = subprocess.run([sys.executable, "-m", "billing_operator", "simulate", str(EXAMPLES), *TIMES],
                          capture_output=True, text=True, cwd=Path(__file__).resolve().parents[1], timeout=60)
    assert done.returncode == 0, done.stderr
    assert f"transaction_id: awake/s-aaaaa/{T0}" in done.stdout and "seconds: '120'" in done.stdout


def test_without_a_catalogue_it_says_so(tmp_path, capsys):
    assert cli.main(["simulate", "--catalogue", str(tmp_path / "none.yaml"), str(EXAMPLES), TIMES[0]]) == 2
    assert "no catalogue" in capsys.readouterr().err


def test_the_default_catalogue(monkeypatch, tmp_path):
    monkeypatch.delenv("BILLING_CATALOGUE", raising=False)
    assert cli.default_catalogue() == CONTRACTS / "catalogue.yaml"       # in a checkout
    monkeypatch.setattr(cli, "__file__", "/app/billing_operator/cli.py")  # in the image: no checkout above it
    assert str(cli.default_catalogue()) == "/etc/browserjs/catalogue.yaml"
    monkeypatch.setenv("BILLING_CATALOGUE", str(tmp_path / "c.yaml"))
    assert cli.default_catalogue() == tmp_path / "c.yaml"


def test_the_images_selfcheck():
    out = io.StringIO()
    assert cli.main(["selfcheck"], out) == 0 and out.getvalue() == "an hour awake: 3600 seconds, 18000 GB-seconds: ok\n"
