"""The catalogue, the configuration, and what the meter sees of a Sandbox."""
import logging

import pytest

from billing_operator.catalogue import CatalogueError, CatalogueFile, parse
from billing_operator.config import Config, ConfigError
from billing_operator.meter import iso, ts
from billing_operator.observe import awake, observe, owner_label

from conftest import CONTRACTS, OWNER, owner_hash, sandbox

CONTRACT = (CONTRACTS / "catalogue.yaml").read_text(encoding="utf-8")


# --- catalogue ------------------------------------------------------------------

def test_the_contracts_catalogue():
    assert parse(CONTRACT).session_disk_gb == 5
    assert parse(CONTRACT).sizes == {"medium", "large"}   # the sizes with a rate of their own


@pytest.mark.parametrize("change,message", [
    (("version: 1", "version: 2"), "version"),
    (("sessionDiskGB: 5", "sessionDiskGB: 0"), "sessionDiskGB"),
    (("sessionDiskGB: 5", "sessionDiskGB: 2.5"), "sessionDiskGB"),
    (("sessionDiskGB: 5", "sessionDiskGB: true"), "sessionDiskGB"),
])
def test_a_catalogue_that_is_wrong_is_refused(change, message):
    assert change[0] in CONTRACT
    with pytest.raises(CatalogueError, match=message):
        parse(CONTRACT.replace(*change))


@pytest.mark.parametrize("text", ["", "[]", "rates: {", "version: 1\n"])
def test_not_a_catalogue(text):
    with pytest.raises(CatalogueError):
        parse(text)


def test_the_file_is_read_again_when_it_changes_and_a_bad_one_keeps_the_last_good(tmp_path, caplog):
    path = tmp_path / "catalogue.yaml"
    path.write_text(CONTRACT)
    f = CatalogueFile(path)
    first = f.current()
    assert first.session_disk_gb == 5 and f.current() is first
    path.write_text(CONTRACT.replace("sessionDiskGB: 5", "sessionDiskGB: 8"))
    assert f.current().session_disk_gb == 8
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        path.write_text("version: 7")
        assert f.current().session_disk_gb == 8 and f.current().session_disk_gb == 8
        path.unlink()  # a ConfigMap being swapped, or unmounted
        assert f.current().session_disk_gb == 8 and f.current().session_disk_gb == 8
    assert caplog.text.count("does not parse") == 1 and caplog.text.count("cannot be read") == 1
    path.write_text(CONTRACT)
    assert f.current().session_disk_gb == 5


def test_no_catalogue_at_all_is_none(tmp_path):
    assert CatalogueFile(tmp_path / "none.yaml").current() is None
    bad = tmp_path / "bad.yaml"
    bad.write_text("nope")
    assert CatalogueFile(bad).current() is None


def test_a_configmap_swapped_through_its_symlink_is_seen(tmp_path):
    """How the kubelet updates a mounted ConfigMap: a new directory, and the
    ..data symlink moved to it."""
    for name, gb in (("v1", 5), ("v2", 8)):
        (tmp_path / name).mkdir()
        (tmp_path / name / "catalogue.yaml").write_text(CONTRACT.replace("sessionDiskGB: 5", f"sessionDiskGB: {gb}"))
    (tmp_path / "..data").symlink_to("v1")
    (tmp_path / "catalogue.yaml").symlink_to("..data/catalogue.yaml")
    f = CatalogueFile(tmp_path / "catalogue.yaml")
    assert f.current().session_disk_gb == 5
    (tmp_path / "..data_tmp").symlink_to("v2")
    (tmp_path / "..data_tmp").rename(tmp_path / "..data")
    assert f.current().session_disk_gb == 8


# --- configuration ----------------------------------------------------------------

ON = {"BILLING": "meter", "METRONOME_API_TOKEN": "made-up-token"}


def test_billing_is_off_unless_it_is_set():
    assert Config.from_env({}) == Config() and not Config.from_env({}).metering
    assert not Config.from_env({"BILLING": "off"}).metering and not Config.from_env({"BILLING": ""}).metering
    assert Config.from_env(ON).metering and Config.from_env({**ON, "BILLING": "enforce"}).metering


def test_the_defaults_are_deploy_mds():
    cfg = Config.from_env({})
    assert (cfg.tick, cfg.max_gap, cfg.catalogue, cfg.namespace, cfg.metronome_url) == (
        60, 150, "/etc/browserjs/catalogue.yaml", "browserjs-sessions", "https://api.metronome.com")


def test_durations_and_the_url():
    cfg = Config.from_env({"TICK": "30s", "MAX_GAP": "2m", "BILLING_CATALOGUE": "/c.yaml", "METRONOME_URL": "http://fake"})
    assert (cfg.tick, cfg.max_gap, cfg.catalogue, cfg.metronome_url) == (30, 120, "/c.yaml", "http://fake")


def test_the_windows_are_settings():
    cfg = Config.from_env({})
    assert (cfg.awake_window, cfg.kept_window) == (300, 21600)
    cfg = Config.from_env({"AWAKE_WINDOW": "1m", "KEPT_WINDOW": "1h"})
    assert (cfg.awake_window, cfg.kept_window) == (60, 3600)
    for env in ({"AWAKE_WINDOW": "30s"}, {"KEPT_WINDOW": "10"}, {"AWAKE_WINDOW": "often"}):
        with pytest.raises(ConfigError):
            Config.from_env(env)


def test_billing_on_needs_the_token_and_the_token_is_never_shown():
    with pytest.raises(ConfigError, match="METRONOME_API_TOKEN"):
        Config.from_env({"BILLING": "meter"})
    cfg = Config.from_env(ON)
    assert cfg.metronome_token == "made-up-token" and "made-up-token" not in repr(cfg) and "made-up-token" not in str(cfg)
    assert Config.from_env({"METRONOME_API_TOKEN": ""}).metronome_token == ""   # off: none needed


@pytest.mark.parametrize("env", [{"BILLING": "on"}, {"TICK": "soon"}, {"TICK": "500ms"}, {"TICK": "0"},
                                 {"TICK": "150s"}, {"MAX_GAP": "60s"}])
def test_a_wrong_configuration_is_refused(env):
    with pytest.raises(ConfigError):
        Config.from_env(env)


# --- time -------------------------------------------------------------------------

def test_times_are_whole_seconds_utc():
    t = ts("2026-10-02T10:00:00Z")
    assert iso(t) == "2026-10-02T10:00:00Z"
    assert ts("2026-10-02T10:00:00.999999Z") == t == ts("2026-10-02T12:00:00+02:00") == ts("2026-10-02T10:00:00")


# --- observation ------------------------------------------------------------------

def test_owner_label_is_the_backends():
    # sessions.OwnerLabel: the first 32 hex characters of the SHA-256 of the address.
    assert owner_label("u@example.com") == "7615aafcb45bcc853c4ed32cc5539842" == owner_hash()


@pytest.mark.parametrize("sb,want", [
    (sandbox("s-aaaaa"), True),
    (sandbox("s-aaaaa", mode=None), True),                              # as FromSandbox: not Suspended, and Ready
    (sandbox("s-aaaaa", ready="False"), False),                         # starting
    (sandbox("s-aaaaa", ready=None), False),                            # no conditions yet
    (sandbox("s-aaaaa", mode="Suspended", ready="True"), False),        # stopping: suspended, pod not yet down
    (sandbox("s-aaaaa", mode="Suspended", ready="False"), False),       # asleep, stopped
    (sandbox("s-aaaaa", deleting=True), False),
])
def test_awake_is_the_backends_running(sb, want):
    assert awake(sb) is want


def test_observe():
    seen = observe([
        sandbox("s-aaaaa", ready_since="2026-10-02T09:59:40Z"),
        sandbox("s-bbbbb", mode="Suspended", ready="False"),
        sandbox("s-ccccc", deleting=True),            # being deleted: not there
        sandbox("s-warm1", owner=None),               # the warm pool: nobody's
        sandbox("s-ddddd", owner="v@example.com", ready="False"),
    ], 5)
    assert seen == {
        owner_hash(): {"s-aaaaa": {"awake": True, "readySince": "2026-10-02T09:59:40Z", "diskGB": 5, "size": "small"},
                       "s-bbbbb": {"awake": False, "readySince": None, "diskGB": 5, "size": "small"}},
        owner_hash("v@example.com"): {"s-ddddd": {"awake": False, "readySince": None, "diskGB": 5, "size": "small"}},
    }


def test_a_sessions_size_is_its_annotation_if_the_catalogue_prices_it():
    def sized(name, size):
        sb = sandbox(name)
        sb["metadata"]["annotations"]["browserjs.dev/size"] = size
        return sb

    seen = observe([sandbox("s-aaaaa"), sized("s-bbbbb", "medium"), sized("s-ccccc", "large"), sized("s-ddddd", "huge")],
                   5, frozenset({"medium", "large"}))[owner_hash()]
    assert {sid: o["size"] for sid, o in seen.items()} == {
        "s-aaaaa": "small", "s-bbbbb": "medium", "s-ccccc": "large",
        "s-ddddd": "small",   # no rate for it: charged as small, the lowest
    }
    # With a catalogue that prices no sizes, every session is small.
    assert observe([sized("s-bbbbb", "medium")], 5)[owner_hash()]["s-bbbbb"]["size"] == "small"


def test_a_session_without_the_label_is_grouped_by_its_owner():
    sb = sandbox("s-aaaaa")
    del sb["metadata"]["labels"]
    assert list(observe([sb], 5)) == [owner_hash(OWNER)]


def test_a_warm_pod_is_awake_for_its_owner_from_when_it_was_taken():
    """Ready in the pool since 10:00:00, adopted at 10:01:55: at the first
    tick the owner has had it for five seconds, not two minutes."""
    sb = sandbox("s-warm1", ready_since="2026-10-02T10:00:00Z")
    sb["metadata"]["annotations"]["browserjs.dev/created"] = "2026-10-02T10:01:55Z"
    assert observe([sb], 5)[owner_hash()]["s-warm1"]["readySince"] == "2026-10-02T10:01:55Z"
    # Woken later than it was adopted: Ready's own time.
    sb["status"]["conditions"][0]["lastTransitionTime"] = "2026-10-02T11:00:00Z"
    assert observe([sb], 5)[owner_hash()]["s-warm1"]["readySince"] == "2026-10-02T11:00:00Z"
    sb["metadata"]["annotations"]["browserjs.dev/created"] = "not a time"
    assert observe([sb], 5)[owner_hash()]["s-warm1"]["readySince"] == "2026-10-02T11:00:00Z"
