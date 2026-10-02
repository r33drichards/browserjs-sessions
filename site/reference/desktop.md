# What is on the desktop

A session is a minimal Linux desktop built for one job: a browser that a
person and an agent share. It is not a general workstation.

## What it has

| Part | What |
| --- | --- |
| Display | One X display. 1280 by 800 at first, 24-bit colour. It follows the viewer's size, from 320 by 200 up to 2560 by 1600. |
| Window manager | openbox, one desktop. Ordinary windows are maximised. `Alt+Tab` and `Alt+F4` work. |
| Application | Chromium, always open. If its last window is closed, or it crashes, it starts again with the same profile. |
| Fonts | DejaVu, Noto and colour emoji. |
| Clipboard | The X clipboard, for text and for files sent through the Files box. |
| Disk | One persistent disk: the browser profile, the Downloads folder, and the agent's memory. |
| Network | The public internet. |

## What it does not have

- No terminal or shell.
- No file manager, editor or office applications.
- No way to install software.
- No sound.
- No access to other sessions or to private networks.

If your task needs an application other than a browser, this is not the
right tool today.

## How an agent reaches it

| Way | Status |
| --- | --- |
| The browser's pages, through `browser_execute`: navigate, click and type by selector, run scripts, take screenshots of the page | Live |
| The whole desktop, by mouse, keyboard and screenshots of the screen (`desktop_execute`) | Planned, not available |

So today an agent cannot operate what is outside a web page: Chromium's own
menus and prompts, the file chooser, a second window's frame. A person at
the live view can. See [Watch an agent and take over](/guides/take-over).
