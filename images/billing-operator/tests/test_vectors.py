"""Every vector of metering-vectors.json through the seconds function:
`awakeSeconds` and `diskGBSeconds`. The money in the vectors is Metronome's
now (and the backend's fake's)."""
import copy
import io
import json

import pytest

from billing_operator import cli
from billing_operator.meter import seconds

from conftest import CONTRACTS

VECTORS = json.loads((CONTRACTS / "metering-vectors.json").read_text(encoding="utf-8"))
IDS = [v["name"] for v in VECTORS]


def test_the_contract_has_its_vectors():
    assert len(VECTORS) == 18


@pytest.mark.parametrize("vector", VECTORS, ids=IDS)
def test_vector(vector):
    sessions = copy.deepcopy((vector.get("state") or {}).get("sessions") or {})
    for tick, want in zip(vector["ticks"], vector["expect"], strict=True):
        sessions, got = seconds(sessions, tick["observed"], tick["now"])
        assert got.awake == want["awakeSeconds"], tick["now"]
        assert got.disk_gb == want["diskGBSeconds"], tick["now"]
        assert set(sessions) == set(tick["observed"])


@pytest.mark.parametrize("vector", VECTORS, ids=IDS)
def test_the_function_changes_nothing_it_is_given(vector):
    sessions = copy.deepcopy((vector.get("state") or {}).get("sessions") or {})
    observed = copy.deepcopy(vector["ticks"][0]["observed"])
    seconds(sessions, observed, vector["ticks"][0]["now"])
    assert sessions == ((vector.get("state") or {}).get("sessions") or {})
    assert observed == vector["ticks"][0]["observed"]


def test_the_command_line_runs_the_vectors():
    out = io.StringIO()
    assert cli.main(["vectors", str(CONTRACTS / "metering-vectors.json")], out) == 0
    assert out.getvalue() == "18/18 vectors pass (awakeSeconds, diskGBSeconds)\n"


def test_the_command_line_fails_on_a_wrong_vector(tmp_path):
    wrong = copy.deepcopy(VECTORS[:1])
    wrong[0]["expect"][0]["awakeSeconds"]["s-aaaaa"] += 1
    path = tmp_path / "v.json"
    path.write_text(json.dumps(wrong))
    out = io.StringIO()
    assert cli.main(["vectors", str(path)], out) == 1
    assert "FAIL steady" in out.getvalue() and "0/1 vectors pass" in out.getvalue()
