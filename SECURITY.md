# Security

## Reporting a vulnerability

Report it privately, through GitHub: **Security, Advisories, "Report a
vulnerability"**, or
<https://github.com/r33drichards/computer-use/security/advisories/new>.
Only the maintainers can read such a report.

Please do not open a public issue, a pull request or a discussion for it, and
do not test against sessions or accounts of the hosted service
(computeruse.site) that are not your own.

Useful in a report: what an attacker gains, the steps to reproduce it (against
a local cluster, `hack/local-up.sh`, where that is possible), and the commit
or the date you saw it on the hosted service.

You will get an answer within a week. Once a fix is deployed the advisory is
published, with credit if you want it. There is no bounty.

## What counts

The parts whose failure matters most:

- one user reaching another user's session, files, snapshots or API tokens;
- a session reaching the cluster, the node, the metadata server or private
  addresses (the gVisor sandbox and the `session-pods` NetworkPolicy);
- a tool call that a session's policy refuses being carried out all the same,
  through that same tool;
- the sign-in path (Pomerium, Dex) and the API-token exchange;
- the GitHub Actions workflows and what they may do in Google Cloud.

Known and intended, so not vulnerabilities: a policy that restricts one tool
while allowing another that can do the same thing (the editor warns about
these); whatever a session's own user does inside their session; someone with
`kubectl` on the cluster.

## Supported versions

`main`, which is what the hosted service runs. There are no releases yet.
