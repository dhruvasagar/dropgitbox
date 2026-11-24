# How dropgitbox works

dropgitbox has **one server node** and any number of **devices**. Devices
talk only to the server, never to each other, and only over outbound
HTTP(S), so only the server has to be reachable (see
[deployment](deployment.md)).

```
                 ┌──────────────────────────────┐
                 │ server node                  │
   laptop ─────► │ dropgitbox server            │ ◄───── desktop
                 │ one bare git repo per folder │
   work machine► │ HTTPS (or a VPN/tailnet)     │ ◄───── another device
                 └──────────────────────────────┘
```

Each synced folder is an ordinary git repository on every device, with a
matching bare repository on the server. One server can hold any number of
folders, and each device chooses which of them to sync.

## The server

`dropgitbox server` keeps one bare git repository per folder in its data
directory. It serves git's smart HTTP protocol through `git http-backend`,
and runs a small API beside it to list and create repositories and to
collect status reports from devices.

After every push, the server tells every device subscribed to that folder,
over a [Server-Sent Events](https://developer.mozilla.org/docs/Web/API/Server-sent_events)
stream. Devices therefore learn about changes within moments, instead of
polling. Each stream carries a heartbeat every 25 seconds, which also
closes the streams of devices that have been revoked.

Each device has its own token, which can be rotated or revoked on its own.
The server refuses force-pushes and branch deletion for every repository
(`receive.denyNonFastForwards`, `receive.denyDeletes`) and checks
everything pushed (`receive.fsckObjects`), so history on the server is
append-only.

## The sync cycle

The daemon, `dropgitbox watch`, watches every configured folder
recursively. A folder is synced:

- when its files change, once it has been quiet for 2 seconds
  (`client.debounce`). During continuous writes, a change waits at most
  30 seconds (`client.max_delay`);
- when the server announces a push from another device;
- every 5 minutes as a safety net (`client.poll`), in case a notice was
  missed;
- when you ask for it, with `dropgitbox sync`;
- after a failure, with backoff: 5 seconds, then 10, 20 and so on, up to
  the poll interval.

Each sync does the same three steps:

1. **Commit.** `git add --all`, then a commit with a descriptive subject:
   `Modified notes/todo.txt` for one file, or `3 files: 2 added, 1 deleted`
   for several. The commit body names the device and lists the changed
   files.
2. **Merge.** Fetch from the server and merge its changes. This is a
   fast-forward when only one side changed. If a file was saved again
   between the commit and the merge, that change is committed too and the
   merge is retried.
3. **Push.** If another device pushed in the meantime, fetch, merge and
   try again, up to 5 times.

If the merge can't combine the two sides, syncing that folder **pauses**
until you resolve the conflict. See [conflicts](conflicts.md).

Each sync's outcome (in sync, paused on a conflict, or failed) is reported
to the server, so `dropgitbox device list` on the server shows the health
of every device.

## Offline devices

A device without a connection keeps committing locally, so every change is
still recorded, with its time and device. When the connection comes back,
the next sync merges and pushes everything. After 10 minutes of failing
(`notify.offline_after`), the daemon tells you the device is offline, so
short network blips stay quiet. See [notifications](notifications.md).

## Several folders

One daemon syncs every folder listed in the config, each with its own
repository, history and state. A conflict or error in one folder doesn't
affect the others. A repository can live at a different path on each
device. Folders added with `setup`, or removed from the config, start or
stop syncing within a couple of seconds, without a restart. See
[using a device](devices.md).

## What's in a synced folder

Each folder contains a normal `.git` directory, on the branch `main`. Its
git config records the server, repository and device name
(`dropgitbox.server`, `dropgitbox.repo`, `dropgitbox.device`), and an
`origin` remote pointing at the server.

The device token normally lives only in the config directory
(`~/.config/dropgitbox/token`), shared by all folders, so re-joining with a
new token updates them all at once. A token given to `setup` with
`--token` or `$DROPGITBOX_TOKEN` is kept for that folder alone, in
`.git/dropgitbox-token` (mode 0600). Either way, git gets the token from
dropgitbox through the environment, never in a URL, `.git/config` or a
command line.

Editor and OS clutter is never synced. `setup` adds these patterns to
`.git/info/exclude`, which applies only on that device: `.DS_Store`,
`Thumbs.db`, `desktop.ini`, `*.swp`, `*.swx`, `*~`, `.#*`, `~$*`,
`*.crdownload` and `*.part`. To ignore more, add them there, or to a
`.gitignore` in the folder, which itself syncs to every device.

Because it's plain git, every git tool works on a synced folder. See
[history](history.md#using-git-directly).

## Limitations

- **Large or binary files.** Every version of every file is kept forever,
  in full, on every device. That is the point, but it makes dropgitbox a
  poor fit for large media. Git LFS support would be the natural next step.
- **Empty directories** are not synced, because git does not track them.
- **No git repositories inside a synced folder, and no synced folders
  inside one another.** Git can record a repository inside another only as
  a pointer to a commit, not as files, so those files would silently not
  sync. `setup` refuses such folders, and a sync stops the folder if a
  repository appears inside it later. A git repository *around* the folder,
  such as a dotfiles repository at `~/.git`, is fine.
- **Single user.** Every device can read and write every folder on the
  server. There are no per-folder permissions.
- **Binary-file conflicts** have no markers to edit: you pick one side. See
  [conflicts](conflicts.md#binary-files).
- **Windows** is not supported yet.
