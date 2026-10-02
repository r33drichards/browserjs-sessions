# Sleep, wake, stop and resume

## Let a session sleep

Do nothing. A session that is not used for 15 minutes goes to sleep. Its
state becomes <span class="wf-state">asleep</span>. It uses no compute while
asleep. Its disk and a snapshot of the running desktop are kept.

## Wake a session

Any of these wakes it:

- **From an agent**: the next MCP tool call. The call waits for the wake and
  then answers.
- **From the app**: open the session and choose **Wake** (on the list the
  button says **Resume**).

The desktop comes back as it was: the same windows, the same pages, the same
text in a half-filled form.

## Keep a session awake

A session stays awake while its screen is open in a visible browser tab, and
while an MCP call is running. A tab in the background stops counting after
one minute.

## Stop a session

Choose **Stop**. Use this when the session must stay off: a stopped session
does not wake for an agent. Its state is
<span class="wf-state">stopped</span> and its disk is kept.

## Resume a stopped session

Choose **Resume**. The desktop starts from its disk. Chromium reopens its
tabs and reloads them. Logins are kept. What was only in memory is not: a
stop takes no snapshot.

## Delete a session

Choose **Delete** and confirm. The session, its disk and its snapshot are
removed. This cannot be undone, and the MCP URL stops working.

See [Session lifecycle](/reference/lifecycle) for every state.
