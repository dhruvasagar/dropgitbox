# Notifications and status

The sync daemon (`dropgitbox watch`) turns everything that happens to a
folder into **events**, and shows them in four ways: desktop
notifications, live status, an event stream, and a hook command you
provide. Routine syncing stays quiet; you hear about what needs your
attention.

## Events

| Event | When | Desktop notification | Hook, by default |
| --- | --- | --- | --- |
| `pending` | Local changes were detected; they sync once the folder is quiet | | |
| `syncing` | A sync started | | |
| `synced` | A sync finished, with counts of files committed and pulled | | ✔ |
| `remote_changes` | A sync brought in changes from other devices | | ✔ |
| `conflict` | Syncing paused on a [conflict](conflicts.md); you need to act | ✔ | ✔ |
| `resolved` | The conflict was resolved and syncing resumed | ✔ | ✔ |
| `error` | A sync attempt failed; it is retried automatically | | ✔ |
| `offline` | Syncing has kept failing for `offline_after` (10 minutes), so brief blips stay quiet | ✔ | ✔ |
| `online` | Syncing works again after `offline` or `unauthorized` | ✔ | ✔ |
| `unauthorized` | The server rejected this device's token (revoked or rotated) | ✔ | ✔ |

`conflict`, `offline` and `unauthorized` are sent once, when they start,
not on every retry. Choose which events go where with
`notify.desktop_events` and `notify.command_events` (`"*"` means all). See
[configuration](configuration.md#notify).

## Desktop notifications

On Linux these use `notify-send`, falling back to `gdbus` (libnotify or
GLib, present on most desktops). On macOS they use the built-in
Notification Center, through `osascript`. Turn them off with
`notify.desktop = false`, or `watch --no-desktop-notify`.

Check that they work:

```console
$ dropgitbox notify test
desktop: sent a test notification
command: none configured (notify.command)
```

On a machine without a desktop session, `notify test` reports that no
notification mechanism was found. The other channels below still work.

## Live status

The daemon serves its state over a private Unix socket (see
[configuration](configuration.md#files)), which `status`, `events` and
`sync` use. Nothing else can connect to it: the socket is mode 0600 in a
0700 directory.

**One line** for status bars and prompts, naming the most urgent state
across all folders:

```console
$ dropgitbox status --short
dropgitbox: CONFLICT Box
```

From most to least urgent: `access denied`, `STOPPED`, `CONFLICT`,
`offline`, `retrying`, `syncing`, `pending`, then `synced` when all is
well, followed by the affected folders. `dropgitbox: not running` means the
daemon isn't running.

```sh
# tmux
set -g status-right '#(dropgitbox status --short)'
# waybar / polybar / i3blocks: a custom module running `dropgitbox status --short` every few seconds
# macOS menu bar: a SwiftBar or xbar plugin that prints `dropgitbox status --short`
```

**JSON** for richer integrations:

```console
$ dropgitbox status --json
{
  "pid": 4242,
  "version": "0.1.0",
  "started": "2026-11-23T09:12:03Z",
  "folders": [
    {
      "folder": "/home/me/Box",
      "repo": "Box",
      "state": "conflict",
      "since": "2026-11-23T13:43:31Z",
      "last_sync": "2026-11-23T13:40:02Z",
      "conflicts": ["notes.txt"]
    }
  ]
}
```

`state` is one of `idle`, `pending`, `syncing`, `conflict`, `error`,
`offline`, `unauthorized` and `stopped`. `last_error` holds the latest
error message, when there is one.

## The event stream

```console
$ dropgitbox events
13:43:31  syncing        Box: syncing
13:43:31  conflict       Box: sync paused, conflict: Conflicting changes in notes.txt. ...
13:43:35  resolved       Box: conflict resolved: Syncing has resumed.
```

prints events as they happen, until interrupted. `events --json` prints
each as a JSON object on its own line, with `time`, `type`, `folder`,
`repo`, and when relevant `message`, `files`, `committed`, `pulled` and
`pushed`:

```sh
dropgitbox events --json | jq -r 'select(.type == "conflict") | .folder'
```

## Hook command

To forward events anywhere (a phone through ntfy or Pushover, email, a
chat webhook, a sound), have the daemon run a program of yours:

```toml
[notify]
command = ["~/bin/dropgitbox-hook"]
command_events = ["conflict", "resolved", "offline", "online", "unauthorized"]
```

The command runs directly, not through a shell, so it can't be tricked by
file names. The event arrives as JSON on stdin (as from `events --json`),
and in environment variables:

| Variable | Contents |
| --- | --- |
| `DROPGITBOX_EVENT` | Event type, such as `conflict` |
| `DROPGITBOX_FOLDER`, `DROPGITBOX_REPO` | Which folder |
| `DROPGITBOX_TITLE`, `DROPGITBOX_BODY` | Ready-made notification text |
| `DROPGITBOX_URGENT` | `true` for events that need action |
| `DROPGITBOX_FILES` | Conflicted or changed files, one per line |
| `DROPGITBOX_MESSAGE` | The error or detail message |
| `DROPGITBOX_TIME` | When it happened, in RFC 3339 |

```sh
#!/bin/sh
# ~/bin/dropgitbox-hook: forward to a phone through ntfy
curl -s -H "Title: $DROPGITBOX_TITLE" -d "$DROPGITBOX_BODY" https://ntfy.sh/my-private-topic >/dev/null
```

Hooks run one at a time, each with a 30-second limit, and failures are
logged. The daemon checks that the command exists when it starts.
`dropgitbox notify test` runs it once with a test event.
