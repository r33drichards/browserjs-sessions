--------------------------- MODULE OpaReplacement ---------------------------
(* One OPA replica after another is replaced (a rollout, a node drain) while *)
(* a session's mcp-js keeps asking for decisions through the Service.        *)
(* Can a call the policy allows be denied although a healthy replica exists? *)
(* mcp-js denies when it gets no answer: there is nothing else it can do.    *)
(*                                                                           *)
(* What is modelled, and what each thing stands for in the cluster:          *)
(*   phase      the pod: starting, serving, draining (deleted, in its        *)
(*              preStop sleep, still answering), stopping (SIGTERM: the      *)
(*              listener is closed), gone                                    *)
(*   endpoint   whether the dataplane still sends new connections for the    *)
(*              Service to the pod. It follows the pod's state with a lag.   *)
(*   reachable  whether the NetworkPolicy that lets session pods in has been *)
(*              programmed for the pod's address. It follows the pod's       *)
(*              creation with a lag. Until then a connection is dropped      *)
(*              without an answer.                                           *)
(*   conn       the connection mcp-js keeps open and reuses                  *)
(* Not modelled: a pod that dies without closing its connections (a node     *)
(* that vanishes); see the README.                                           *)
EXTENDS Naturals, FiniteSets

CONSTANTS Pods,                   \* every pod there will ever be
          Initial,                \* the replicas at the start
          MaxDeletes,             \* how many replacements to explore
          DrainWaitsForEndpoints, \* SIGTERM only after the dataplane forgot the pod
          ReadyWaitsForNetwork,   \* Ready only after the pod can be reached
          Retries,                \* attempts mcp-js makes after a transport failure
          None                    \* no pod

VARIABLES phase, endpoint, reachable, conn, call, tries, deletes

vars == <<phase, endpoint, reachable, conn, call, tries, deletes>>

Init ==
    /\ phase = [p \in Pods |-> IF p \in Initial THEN "serving" ELSE "none"]
    /\ endpoint = [p \in Pods |-> p \in Initial]
    /\ reachable = [p \in Pods |-> p \in Initial]
    /\ conn = None
    /\ call = "idle"
    /\ tries = 0
    /\ deletes = 0

\* The pod answers a request that reaches it.
Answers(p) == phase[p] \in {"serving", "draining"} /\ reachable[p]

\* A replica a call could have been answered by.
Healthy(p) == phase[p] = "serving" /\ endpoint[p] /\ reachable[p]

\* --- The cluster ------------------------------------------------------------

\* A replica is deleted while another is healthy (the PodDisruptionBudget
\* and maxUnavailable: 0), and the ReplicaSet makes its replacement.
Delete(p) ==
    /\ deletes < MaxDeletes
    /\ phase[p] = "serving"
    /\ \E q \in Pods \ {p} : Healthy(q)
    /\ deletes' = deletes + 1
    /\ IF \E r \in Pods : phase[r] = "none"
         THEN \E r \in Pods :
                /\ phase[r] = "none"
                /\ phase' = [phase EXCEPT ![p] = "draining", ![r] = "starting"]
         ELSE phase' = [phase EXCEPT ![p] = "draining"]
    /\ UNCHANGED <<endpoint, reachable, conn, call, tries>>

\* The dataplane stops sending new connections to a pod that is going.
EndpointRemoved(p) ==
    /\ endpoint[p]
    /\ phase[p] \in {"draining", "stopping", "gone"}
    /\ endpoint' = [endpoint EXCEPT ![p] = FALSE]
    /\ UNCHANGED <<phase, reachable, conn, call, tries, deletes>>

\* The preStop sleep is over: OPA gets SIGTERM and closes its listener.
PreStopOver(p) ==
    /\ phase[p] = "draining"
    /\ DrainWaitsForEndpoints => ~endpoint[p]
    /\ phase' = [phase EXCEPT ![p] = "stopping"]
    /\ UNCHANGED <<endpoint, reachable, conn, call, tries, deletes>>

Exit(p) ==
    /\ phase[p] = "stopping"
    /\ phase' = [phase EXCEPT ![p] = "gone"]
    /\ UNCHANGED <<endpoint, reachable, conn, call, tries, deletes>>

\* The NetworkPolicy is programmed for a new pod's address.
Programmed(p) ==
    /\ phase[p] \in {"starting", "serving"}
    /\ ~reachable[p]
    /\ reachable' = [reachable EXCEPT ![p] = TRUE]
    /\ UNCHANGED <<phase, endpoint, conn, call, tries, deletes>>

\* A new pod has its bundle and passes its readiness probe.
BecomesReady(p) ==
    /\ phase[p] = "starting"
    /\ ReadyWaitsForNetwork => reachable[p]
    /\ phase' = [phase EXCEPT ![p] = "serving"]
    /\ UNCHANGED <<endpoint, reachable, conn, call, tries, deletes>>

EndpointAdded(p) ==
    /\ phase[p] = "serving"
    /\ ~endpoint[p]
    /\ endpoint' = [endpoint EXCEPT ![p] = TRUE]
    /\ UNCHANGED <<phase, reachable, conn, call, tries, deletes>>

\* --- mcp-js -----------------------------------------------------------------

\* The kept connection is still good: the request goes down it.
Reuse ==
    /\ call = "idle"
    /\ conn # None
    /\ Answers(conn)
    /\ call' = "ran"
    /\ UNCHANGED <<phase, endpoint, reachable, conn, tries, deletes>>

\* No kept connection, or its pod closed it (the client sees the close and
\* opens another, which hyper does for a reused connection): a new connection
\* through the Service, to whichever pod the dataplane picks.
Connect ==
    /\ call = "idle"
    /\ IF conn = None THEN TRUE ELSE ~Answers(conn)
    /\ \/ \E p \in Pods :
            /\ endpoint[p]
            /\ IF Answers(p)
                 THEN /\ conn' = p
                      /\ call' = "ran"
                      /\ tries' = tries
                 \* Refused (the listener is closed) or never answered (the
                 \* pod is gone, or cannot be reached yet): a transport
                 \* failure, after up to the whole timeout.
                 ELSE /\ conn' = None
                      /\ IF tries < Retries
                           THEN call' = "idle" /\ tries' = tries + 1
                           ELSE call' = "denied" /\ tries' = tries
       \/ /\ \A p \in Pods : ~endpoint[p]
          /\ conn' = None
          /\ call' = "denied"
          /\ tries' = tries
    /\ UNCHANGED <<phase, endpoint, reachable, deletes>>

NextCall ==
    /\ call = "ran"
    /\ call' = "idle"
    /\ tries' = 0
    /\ UNCHANGED <<phase, endpoint, reachable, conn, deletes>>

Next ==
    \/ \E p \in Pods :
         \/ Delete(p) \/ EndpointRemoved(p) \/ PreStopOver(p) \/ Exit(p)
         \/ Programmed(p) \/ BecomesReady(p) \/ EndpointAdded(p)
    \/ Reuse \/ Connect \/ NextCall

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ phase \in [Pods -> {"none", "starting", "serving", "draining", "stopping", "gone"}]
    /\ endpoint \in [Pods -> BOOLEAN]
    /\ reachable \in [Pods -> BOOLEAN]
    /\ conn \in Pods \cup {None}
    /\ call \in {"idle", "ran", "denied"}
    /\ tries \in 0..Retries
    /\ deletes \in 0..MaxDeletes

\* The property: a call is never denied for want of an answer. (Delete keeps
\* a healthy replica in existence throughout.)
NoSpuriousDeny == call # "denied"
=============================================================================
