# Your first session

In this tutorial you create a session, use its browser, and see that it keeps
your work. It takes about five minutes.

## 1. Sign in

Open [app.browserjs.com](https://app.browserjs.com) and sign in with Google or
GitHub. You land on the **Sessions** list. It is empty.

## 2. Create a session

1. Choose **New session**.
2. The name is optional. Leave it empty to get the suggested one, two words
   such as `brave-otter`.
3. Choose **Create**.

The session's page opens. It may say *Starting the browser…* for a moment.
Then the state becomes <span class="wf-state">running</span> and the screen shows
a Chromium window.

## 3. Use the browser

Click inside the screen and type an address. The browser is yours: sign in to
a site, open a few tabs.

Under the screen are two boxes:

- **Clipboard** moves text between your computer and the session.
- **Files** moves files. What the session's browser downloads shows up there.

## 4. Stop it and bring it back

1. Choose **Stop**. The state goes to <span class="wf-state">stopping</span>,
   then <span class="wf-state">stopped</span>. The screen is replaced by a
   placeholder.
2. Choose **Resume**. The browser starts again and reopens your tabs. You are
   still signed in to the site.

The session's disk holds the browser's profile. That is why the logins and
tabs came back.

## 5. Find the MCP URL

Under **Details** is the session's **MCP URL**, with a **Copy** button. That
address is how an agent uses this browser.

## What next

- [Connect a session to Claude](/tutorials/connect-claude)
- [What persists and what does not](/explanation/persistence)
