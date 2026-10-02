# A call denied because an OPA replica was being replaced

`OpaReplacement.tla` is a TLA+ model of what happens between a session's
mcp-js and the shared OPA while one OPA replica after another is replaced (a
rollout, a node drain, a Spot preemption). It was written to find how a call
the policy allows can be denied although a healthy replica exists the whole
time: under enforcement that is a tool call an agent is refused for no
reason of its own.

## What was seen

The kind test deleted and replaced an OPA pod 20 times while calls were made
back to back (run 37053319194): 5451 calls, 3 denied. All three were mcp-js
giving up after its 5 second timeout (`OPA request failed: error sending
request`), not a refusal and not a reset: the request went somewhere that
never answered. Two began 2.2 s after a delete was issued, when the deleted
pod was still in its preStop sleep and answering, and about when its
replacement turned Ready. The third began 9.6 s after a delete.

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
   serving throughout. This is the shape of the two failures at 2.2 s.
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

- `deploy/base/opa.yaml`: the readiness probe waits 5 seconds before its
  first try, and the preStop sleep is 10 seconds. These stand in for the
  model's two orderings **by time**: nothing in Kubernetes lets a pod wait
  for "the NetworkPolicy is programmed for my address" or "every node has
  forgotten me". The model holds given the orderings; the deployment makes
  them very likely, not certain.
- mcp-js (r33drichards/mcp-js#271): connecting is bounded at 1 second, and a
  request that fails in transport is made once more; a decision is never
  asked for twice. This is for what the orderings cannot cover. The model
  says it is not sufficient alone, and it is not needed when the orderings
  hold; in a real cluster the second attempt usually lands on another
  replica.

## What is not modelled or not shown

- A replica that dies without closing its connections (its node vanishes).
  A kept connection to it answers nothing, and the Service lists it until
  the node is noticed missing. Only the client's retry helps there, and
  only when the second attempt is sent elsewhere.
- That the unreachable-when-Ready window is what happened on kind is an
  inference from the timing. The run that would have shown it directly (a
  prober opening a new connection to the Service every 50 ms, and a log of
  every OPA pod's readiness change, both now in `test/policy/run.sh`) has
  not completed.
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
