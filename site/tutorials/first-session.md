# Your first desktop

In this tutorial you create a session, use its desktop, and see that it keeps
your work. It takes about five minutes.

## 1. Sign in

Open [app.computeruse.site](https://app.computeruse.site) and sign in with
Google or GitHub. You land on the **Sessions** list. It is empty.

## 2. Create a session

1. Choose **Create session**.
2. The name is optional. Leave it empty to get the suggested one, two words
   such as `brave-otter`.
3. Choose **Create**.

The session's page opens. It may say *Starting the browser…* for a moment.
Then the state becomes <span class="wf-state">running</span> and the screen
shows the desktop: one Chromium window that fills it.

## 3. Use the desktop

Click inside the screen and type an address. Sign in to a site. Open a few
tabs.

This is the same screen an agent will work on. What you do here, it finds
done.

Under the screen are two boxes:

- **Clipboard** moves text between your computer and the desktop.
- **Files** moves files. What the desktop's browser downloads shows up there.

## 4. Stop it and bring it back

1. Choose **Stop**. The state goes to <span class="wf-state">stopping</span>,
   then <span class="wf-state">stopped</span>. The screen is replaced by a
   placeholder.
2. Choose **Resume**. The desktop starts again and Chromium reopens your
   tabs. You are still signed in to the site.

The session's disk holds the browser's profile. That is why the logins and
tabs came back.

## 5. Find the MCP URL

Choose **Copy MCP URL** at the top of the page. That address is how an agent
reaches this desktop.

## What next

- [Connect an agent with Claude](/tutorials/connect-claude)
- [What is on the desktop](/reference/desktop)
- [What persists and what does not](/explanation/persistence)
