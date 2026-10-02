# A call denied because an OPA replica was being replaced

`OpaReplacement.tla` is a TLA+ model of what happens between a session's
mcp-js and the shared OPA while one OPA replica after another is replaced (a
rollout, a node drain, a Spot preemption). It was written to find how a call
the policy allows can be denied although a healthy replica exists the whole
time: under enforcement that is a tool call an agent is refused for no
reason of its own.

## What was seen

The kind test deleted and replaced an OPA pod 20 times while calls were made
back to back. Three runs:

| Run | Deployment | Calls | Denied |
|---|---|---|---|
| 37053319194 | as before (5 s preStop, no readiness delay) | 5451 | 3 |
| 37059747481 | 10 s preStop, readiness 5 s after start | 7771 | 2 |
| 37068211621 | the same, plus 240 s of calls with no replacement first | 4218 + 11794 | 0 |

Every denial was mcp-js giving up after its 5 second timeout (`OPA request
failed: error sending request`): no refusal and no reset, a request that went
somewhere that never answered. They began 2.2, 9.6, 2.3 s after a delete in
the first run and 8.1, 15.7 s in the second, with no consistent relation to
the delete, the old pod's SIGTERM or the new pod turning Ready.

In the second and third runs a prober in the session's pod opened a new
connection to the Service every 50 ms throughout. **It never failed**, also
in the seconds around a new replica turning Ready. So the first trace below,
a replica that is Ready before it can be reached, is not what happened on
kind; and the timing changes made for it did not stop the denials in the
second run. What did happen is not established. What is known: a request
by mcp-js, rarely (about 1 in 2500 in two runs, none in 16012 in the third),
got no answer for 5 seconds, and mcp-js then denied.

## What the model says

Four things are modelled: the pod's life (starting, serving, draining in
its preStop sleep, stopping after SIGTERM, gone); the dataplane's list of
the Service's endpoints, which follows the pods with a lag; whether the
NetworkPolicy that lets session pods in has been programmed for a pod's
address, which follows the pod's creation with a lag; and mcp-js, which
keeps a connection and reuses it, opens a new one through the Service when
it has none, and denies when it gets no answer.

TLC finds two ways to a denial as things are (`traces/`), each four or five
steps long:

1. **A replica that is Ready before it can be reached** (`traces/drain-only.txt`).
   A pod is deleted; its replacement starts, loads its bundle and passes
   its readiness probe; the dataplane adds it to the Service; mcp-js opens
   a connection and is sent to it; the NetworkPolicy has not been
   programmed for the new address yet, so the connection is dropped without
   an answer; mcp-js waits out its timeout and denies. The old pod is still
   serving throughout. The kind runs did not show this (see above).
2. **A replica that stops before the dataplane forgot it** (`traces/today.txt`).
   A pod is deleted; its preStop sleep ends and OPA closes its listener
   while the dataplane still lists the pod; mcp-js opens a connection and
   is sent to it; the connection fails; mcp-js denies. This one would be
   refused at once, not time out, so it is not what the kind run showed; it
   is what the 5 second preStop sleep was there to prevent, by time.

And which changes close them, each configuration being a `.cfg` here:

| Configuration | Endpoints forgotten before SIGTERM | Reachable before Ready | Retries | Result |
|---|---|---|---|---|
| `today` | no | no | 0 | denied, 4 steps |
| `retry-only` | no | no | 1 | denied, 5 steps: the retry is sent to the same dead replica |
| `drain-only` | yes | no | 0 | denied (way 1) |
| `ready-only` | no | yes | 0 | denied (way 2) |
| `ordered` | yes | yes | 0 | holds, 3093 states |
| `fixed` | yes | yes | 1 | holds, 3093 states |

So the two orderings are what make the property hold, and a retry alone
does not: the dataplane may pick the same replica twice.

## What was changed, and what it rests on

- mcp-js 0.21.0-rc.4 (r33drichards/mcp-js#271, pinned in the session image
  by its own pull request): connecting is bounded at 1 second, and a request
  that fails in transport is made once more on a new connection; a decision
  is never asked for twice. This addresses what was actually seen, whatever
  stalled the one request: a second request on a new connection got an
  answer every time the prober tried. The model says a retry is not enough
  when the dataplane can route both attempts to a replica that cannot
  answer; the prober never saw such a replica.
- `deploy/base/opa.yaml`: the readiness probe waits 5 seconds before its
  first try, and the preStop sleep is 10 seconds. These are hedges against
  the two hazards the model finds (a replica Ready before it can be reached,
  one that stops before the dataplane forgot it), by time, since nothing in
  Kubernetes lets a pod wait for either. Neither was observed on kind; on
  GKE's dataplane they are not known either way.

## What is not modelled or not shown

- A replica that dies without closing its connections (its node vanishes).
  A kept connection to it answers nothing, and the Service lists it until
  the node is noticed missing. Only the client's retry helps there, and
  only when the second attempt is sent elsewhere.
- What stalled the requests that were denied. It was not a new connection
  to a replica that could not be reached (the prober), and it happened at
  different points in the replacement. A name lookup that loses a packet
  and waits 5 s to retry would fit the 5 s; the prober now times lookups by
  themselves and reports how many took over 0.5 s.
- hyper's behaviour on a kept connection the server closed is assumed (it
  notices the close, or retries the request on a new connection), not
  tested here.

## Running it

```
cd spec/opa-replacement
for c in today retry-only drain-only ready-only ordered fixed; do
  nix shell nixpkgs#tlaplus -c tlc -config $c.cfg OpaReplacement.tla
done
```
