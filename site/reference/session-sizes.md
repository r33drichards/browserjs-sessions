# Session sizes

A session comes in three sizes. The size is how much processor and memory
its desktop has. Everything else is the same at every size: the desktop,
the browser, the tools an agent has, and the 5 GB disk.

| Size | Desktop CPU | Desktop memory | A new one is ready in |
| --- | --- | --- | --- |
| `small` (the default) | 1.5 | 2 GiB | A few seconds |
| `medium` | 2 | 5 GiB | Under a minute, or about two when a machine has to be started |
| `large` | 3 | 10 GiB | About two minutes, usually |

Small sessions are kept ready, which is why they start at once. Medium and
large ones start cold.

## Which to pick

Start with small. It runs a browser with a handful of tabs and ordinary
desktop programs. Pick a bigger one when a program needs more memory than
2 GiB: many heavy tabs, an image or video editor, a build.

A program that crashes is not always short of memory. If it crashes the
same way at every size, the size is not the cause.

## Choosing and changing

Choose the size on the **Create session** page, or with `size` in
[`POST /sessions`](/reference/api).

To change it, open the session and choose **change size**, or send
`PATCH /sessions/{id}` with `{"size": "large"}`.

- A session that is **asleep or stopped** has the new size at once.
- A session that is **awake** keeps running as it is. It has the new size
  from its next start: after you put it to sleep or stop it, or after it
  goes idle. Until then the API shows the size asked for as `pendingSize`.

**The first start at a new size is a fresh start.** The desktop starts from
the session's disk, as it does after a [stop](/reference/lifecycle): files,
logins and the agent's notes are kept; open windows and running programs
are not. A session that was asleep with its state saved loses that saved
state when it is resized. Save your work before you resize.

## When there is no room

Large sessions need a whole machine, and there are few machines. If none is
free, creating or waking a session answers at once:

```
409 Conflict
{"error": "no capacity for a large session right now: every session node is full. Try again later, or pick a smaller size.", "code": "no_capacity"}
```

Nothing is created, and a session that was asleep stays asleep with its
disk. Try again in a few minutes, or use a smaller size. Today at most one
large session, or three medium ones, can be awake at a time across all
users.

## Price

The service is not charged for today. When it is, a bigger session will
cost more for each hour it is awake (the planned rates are $0.20, $0.60 and
$1.60 an hour for small, medium and large), the disk will cost the same at
every size, and a session that is asleep will cost only its disk. The
create page will show the rate beside each size.
