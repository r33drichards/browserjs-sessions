# Wake a sleeping session

A session that has not been used for 15 minutes goes to sleep. Its state is
<span class="wf-state">asleep</span>.

## From the app

Open the session and choose **Wake** (on the list the button says
**Resume**). The state goes to <span class="wf-state">starting</span>, then
<span class="wf-state">running</span>, and the screen comes back as it was.

## From an agent

Do nothing. The next MCP tool call wakes the session and waits for it. The
call takes longer than usual and then answers normally.

## If it was stopped, not asleep

A session you stopped with **Stop** is <span class="wf-state">stopped</span>.
Agent calls do not start it. Choose **Resume** in the app.

## Keep a session awake

A session stays awake while its screen is open in a visible tab, and during
an MCP call. A tab in the background lets go after a minute. See
[Sleep and wake](/explanation/sleep-and-wake).
