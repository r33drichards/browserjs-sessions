#!/usr/bin/env python3
"""Reference implementation of the metering step (metering.md), and the
runner for metering-vectors.json.

Not product code: the billing operator's own implementation must give the
same answers. Usage:

    python3 docs/contracts/billing/spike/meter_ref.py docs/contracts/billing/metering-vectors.json
    python3 docs/contracts/billing/spike/meter_ref.py --write docs/contracts/billing/metering-vectors.json

--write fills in each vector's "expect" from this implementation; a change
it makes to the file is a change to the contract and is reviewed as one.
"""
import json
import sys
from datetime import datetime, timezone

MAX_GAP = 150        # seconds; a longer gap between two observations is not billed
LOW_FRACTION = 0.2   # "low" when the balance is at most this share of the allowance...
LOW_FLOOR = 1800     # ...or at most this many seconds, whichever is larger
PERIODIC = ("free", "plan")  # grants that make up the period's allowance


def ts(s):
    return int(datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp())


def iso(t):
    return datetime.fromtimestamp(t, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def live(grant, now):
    """A grant counts at `now` if it has begun, has not expired, is not revoked."""
    if grant.get("revoked"):
        return False
    if ts(grant["validFrom"]) > now:
        return False
    return grant.get("expiresAt") is None or now < ts(grant["expiresAt"])


def order(grant):
    exp = grant.get("expiresAt")
    return (exp is None, ts(exp) if exp else 0, ts(grant["validFrom"]), grant["name"])


def step(state, grants, observed, now_iso):
    """One tick. `state` is Account.status.meter (mutated and returned);
    `grants` the account's Grant specs; `observed` maps session ID to
    {"billable": bool, "readySince": iso or None}."""
    now = ts(now_iso)
    sessions = state.setdefault("sessions", {})
    consumed = state.setdefault("consumed", {})   # grant name -> seconds
    credited = {}

    for sid, o in sorted(observed.items()):
        if not o["billable"]:
            continue
        prev = sessions.get(sid)
        gap = now - ts(prev["lastSeen"]) if prev else None
        if gap is not None and 0 < gap <= MAX_GAP:
            credit = gap
        else:
            # First sight of this run, or the meter was away too long, or the
            # clock went backwards: bill only from when the pod became Ready,
            # and only if that was recent enough to have been seen.
            since = now - ts(o["readySince"]) if o.get("readySince") else -1
            credit = since if 0 <= since <= MAX_GAP else 0
        if credit:
            credited[sid] = credit
        sessions[sid] = {"lastSeen": now_iso}
    for sid in list(sessions):
        if sid not in observed or not observed[sid]["billable"]:
            del sessions[sid]           # the tail since lastSeen is never billed

    owed = sum(credited.values())
    state["usedSeconds"] = state.get("usedSeconds", 0) + owed
    for g in sorted((g for g in grants if live(g, now)), key=order):
        if owed == 0:
            break
        take = min(owed, g["seconds"] - consumed.get(g["name"], 0))
        if take > 0:
            consumed[g["name"]] = consumed.get(g["name"], 0) + take
            owed -= take
    # What no grant covers was used but is not owed by anyone.
    state["overdraftSeconds"] = state.get("overdraftSeconds", 0) + owed

    balance = sum(g["seconds"] - consumed.get(g["name"], 0) for g in grants if live(g, now))
    allowance = sum(g["seconds"] for g in grants if live(g, now) and g["source"] in PERIODIC)
    if balance > 0:
        state.pop("exhaustedAt", None)
        level = "low" if balance <= max(LOW_FRACTION * allowance, LOW_FLOOR) else "ok"
    else:
        state.setdefault("exhaustedAt", now_iso)
        level = "exhausted"
    state["observedAt"] = now_iso
    return {"credited": credited, "balanceSeconds": balance, "allowanceSeconds": allowance,
            "level": level, "overdraftSeconds": state["overdraftSeconds"],
            "exhaustedAt": state.get("exhaustedAt")}


def run(vector):
    state = json.loads(json.dumps(vector.get("state", {})))
    return [step(state, vector["grants"], t["observed"], t["now"]) for t in vector["ticks"]]


def main():
    write = sys.argv[1] == "--write"
    path = sys.argv[2] if write else sys.argv[1]
    vectors = json.load(open(path))
    bad = 0
    for v in vectors:
        got = run(v)
        if write:
            v["expect"] = got
        elif got != v["expect"]:
            bad += 1
            print("FAIL", v["name"])
            for i, (g, e) in enumerate(zip(got, v["expect"])):
                if g != e:
                    print("  tick", i, "got", g, "want", e)
    if write:
        json.dump(vectors, open(path, "w"), indent=2)
        open(path, "a").write("\n")
    print(f"{len(vectors) - bad}/{len(vectors)} vectors pass")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
