# Watch an agent and take over

The agent and you use the same desktop. There is one screen, one browser and
one set of logins.

## Watch

Open the session's page while the agent works. The screen shows what it does
as it does it. Watching keeps the session awake.

## Take over

Click into the screen and use the mouse and keyboard. There is no lock and
no hand-over step: your input goes to the desktop at once.

Do this for the steps an agent cannot or should not do:

- signing in, and second-factor codes,
- captchas,
- a file chooser, a permission prompt or another dialog outside the web
  page, which the agent cannot operate today,
- anything you want to check before it happens, such as a payment.

Then tell the agent, in your client, to continue.

## Avoid working at the same moment

Nothing stops you and the agent from acting at the same time, and the result
is confusing for both. Wait until the agent's current step is finished, or
stop it in your client first.

## Stop the agent

- **In your MCP client**: cancel the running request. This is the normal way.
- **For certain**: choose **Stop** on the session. A stopped session refuses
  agent calls until you choose **Resume**.

There is no pause button in the app. A call that is already running ends
within its time limit, 30 seconds by default.
