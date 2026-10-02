# Sleep, wake, stop and start

## Put a session to sleep

Choose **Sleep**, on the list or on the session's page. A snapshot of the
running desktop is taken, which takes a few seconds; the button says
**Saving state** meanwhile. Then the state becomes
<span class="wf-state">asleep</span>. The session uses no compute while
asleep. Its disk and the snapshot are kept.

From code, with a token that has `sessions:write`:

```sh
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://api.computeruse.site/v1/sessions/$ID/sleep
```

The answer comes when the snapshot is done. It is the session, with
`"stateSaved": true` if the snapshot was taken.

## Let a session sleep

Do nothing. A session that is not used for 15 minutes goes to sleep the
same way.

## Wake a session

Any of these wakes it:

- **From an agent**: the next MCP tool call. The call waits for the wake and
  then answers.
- **From the app**: choose **Wake**, on the list or on the session's page.
- **From code**: `POST /v1/sessions/{id}/wake`.

The desktop comes back as it was: the same windows, the same pages, the same
text in a half-filled form. This is so however it went to sleep: an agent's
call also wakes a session you put to sleep yourself.

A session whose state reads `asleep: state not saved` has no snapshot (it
could not be taken). It wakes from its disk, like a stopped one.

## Keep a session awake

A session stays awake while its screen is open in a visible browser tab, and
while an MCP call is running. A tab in the background stops counting after
one minute.

## Stop a session

Open the menu beside **Sleep** and choose **Stop without saving state**. Use
this when the session must stay off, or when you want a fresh desktop: a
stopped session does not wake for an agent, and no snapshot is taken. Its
state is <span class="wf-state">stopped: starts fresh</span> and its disk is
kept.

A sleeping session can be stopped too: the menu beside **Wake** has **Stop
and discard saved state**.

## Start a stopped session

Choose **Start**. The desktop starts from its disk. Chromium reopens its
tabs and reloads them. Logins are kept. What was only in memory is not: a
stop takes no snapshot.

## Delete a session

Choose **Delete** and confirm. The session, its disk and its snapshot are
removed. This cannot be undone, and the MCP URL stops working.

See [Session lifecycle](/reference/lifecycle) for every state.
