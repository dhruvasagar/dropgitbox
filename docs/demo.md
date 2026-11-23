# Local demo

This walkthrough runs a complete dropgitbox setup on one machine: a server
and two "devices", a laptop and a desktop, each syncing a folder called
`Box`. You'll see:

- live syncing
- history
- a conflict and how it's resolved
- notifications, status and events
- the server's view of each device
- revoking a device
- going offline and coming back

Everything lives under `~/dgb-demo`. Your real dropgitbox config and data
are never touched, and cleaning up is one `rm -rf`.

The walkthrough takes about 15 minutes. For a real deployment, see
[deployment.md](deployment.md) instead.

## Prerequisites

- Linux or macOS
- Go 1.26 or newer, and git 2.28 or newer
- zsh or bash
- Four terminal windows (or tmux panes). This page calls them **server**,
  **laptop**, **desktop** and **play**.

## 1. Build

In any terminal:

```sh
cd /path/to/dropgitbox
go build -o ~/.local/bin/dropgitbox ./cmd/dropgitbox
dropgitbox version
```

`~/.local/bin` must be on your `PATH`. If it isn't, build to some other
directory that is.

## 2. Set up each terminal

Paste this into **every** terminal:

```sh
export DEMO=~/dgb-demo
alias dl='XDG_CONFIG_HOME=$DEMO/laptop/config  DROPGITBOX_SOCKET=$DEMO/laptop/d.sock  dropgitbox'
alias dd='XDG_CONFIG_HOME=$DEMO/desktop/config DROPGITBOX_SOCKET=$DEMO/desktop/d.sock dropgitbox'
alias ds='dropgitbox device -data $DEMO/server'
```

- `dl` runs dropgitbox as the laptop and `dd` as the desktop. Each alias
  points at its own config directory (which also holds the device's
  token) and its own daemon control socket.
- `ds` manages devices on the demo server.

You only need these aliases because two devices share one user account
here. Only one daemon may run per user, and both would otherwise read
`~/.config/dropgitbox`. On real machines you would type plain
`dropgitbox`.

## 3. Start the server

In the **server** terminal:

```sh
dropgitbox server -addr 127.0.0.1:8080 -data $DEMO/server
```

It logs that it is listening, and warns that no devices are registered
yet. Leave it running: this terminal now shows the server's log, including
every rejected request.

The server stores a bare git repository per synced folder
(`$DEMO/server/Box.git`), plus `devices.json` (device names and token
*hashes*) and `activity.json` (what each device last did).

## 4. Register the two devices

In the **play** terminal:

```sh
ds add laptop  -url http://127.0.0.1:8080
ds add desktop -url http://127.0.0.1:8080
```

Each command prints an invite line like this:

```text
  dropgitbox join dgb1.eyJzZXJ2ZXIiOiJodHRwOi8vMTI3LjAuMC4xOjgwODAiLCJ0b2tlbiI6...
```

The invite holds the server URL, the device's name and its access token,
and it is the only time the token is shown. The server keeps only a hash.
On real devices you would send the invite over a private channel.

## 5. Join, set up the folder, and start the daemons

In the **laptop** terminal, paste the laptop's invite after `dl join`:

```sh
dl join dgb1.…laptop invite…
mkdir -p $DEMO/laptop/Box && echo "first note" > $DEMO/laptop/Box/notes.txt
dl setup -repo Box -dir $DEMO/laptop/Box
dl watch
```

In the **desktop** terminal, with the desktop's invite:

```sh
dd join dgb1.…desktop invite…
dd setup -repo Box -dir $DEMO/desktop/Box
dd watch
```

What happens:

- `join` checks the token with the server (`whoami`), then writes
  `$DEMO/<device>/config/dropgitbox/config.toml`, plus a `token` file
  (mode 0600) next to it.
- `setup` on the laptop creates the `Box` repo on the server, commits the
  existing `notes.txt` and pushes it. On the desktop it clones the same
  repo, so `notes.txt` appears there. `setup` also records the folder in
  that device's config under `[[folders]]`, which is how `watch` knows what
  to sync.
- `watch` starts the daemon. It watches the folder for changes, listens to
  the server's event stream for pushes from other devices, and serves the
  control socket that `status`, `events` and `sync` talk to.

Plain `http://` is fine here because `127.0.0.1` is local. Over a network,
use HTTPS (see [deployment.md](deployment.md)).

## 6. Watch it sync

In the **play** terminal, start a live event stream for the desktop:

```sh
dd events
```

Then open a fifth terminal, or use the laptop's, and change something on
the laptop:

```sh
echo "hello from the laptop" > $DEMO/laptop/Box/hello.txt
```

About two seconds later:

- The laptop daemon commits and pushes. The commit waits until the folder
  has been quiet for 2 seconds (`client.debounce`).
- The server tells the desktop over its event stream.
- The desktop fetches and merges. `dd events` prints:

  ```text
  14:02:11  syncing        Box: syncing
  14:02:11  synced         Box: synced: 0 committed, 1 updated from the server
  14:02:11  remote_changes Box: 1 file(s) updated: hello.txt
  ```

Check it:

```sh
cat $DEMO/desktop/Box/hello.txt
```

Edits, renames, deletions and new subdirectories all sync the same way.
Changes go both ways, so edit something on the desktop as well.

Stop `dd events` with Ctrl-C when you're done.

## 7. Status, history and the server's view

Everything below runs in the **play** terminal.

**Status on a device:**

```sh
dd status            # live daemon state + git details for each folder
dd status -short     # one line, for a status bar: "dropgitbox: synced"
dd status -json      # machine-readable daemon state
```

**History.** Every change is a commit, so nothing is ever lost:

```sh
dl log -dir $DEMO/laptop/Box                # recent commits and the files they changed
dl log -dir $DEMO/laptop/Box hello.txt      # history of one file
```

Restore an old version of a file with:

```sh
dl restore -dir $DEMO/laptop/Box -rev <commit> hello.txt
```

The restore is itself a new change, so it syncs to the other devices and
stays in history.

**The server's view of each device:**

```sh
ds list              # every device: last seen, last push, folder state
ds show desktop      # details for one device, per folder
```

## 8. Make a conflict

A conflict happens when two devices change the same lines of a file
before they have seen each other's change. To force one:

1. In the **desktop** terminal, stop the daemon with Ctrl-C.
2. In the **play** terminal, edit the same file on both devices:

   ```sh
   echo "laptop's version"  > $DEMO/laptop/Box/notes.txt     # the laptop daemon syncs this
   echo "desktop's version" > $DEMO/desktop/Box/notes.txt    # the desktop is offline
   ```

3. In the **desktop** terminal, start the daemon again with `dd watch`.

The desktop commits its edit, tries to merge the laptop's, and pauses on
the conflict. The daemon logs `SYNC PAUSED`, and with a desktop session
you get a notification. Then check:

```sh
dd status -short     # dropgitbox: CONFLICT Box
dd status            # State: PAUSED: conflict in notes.txt, plus how to resolve
cat $DEMO/desktop/Box/notes.txt
ds list              # the server shows the desktop's conflict too
```

The file now has git conflict markers:

```text
<<<<<<< HEAD
desktop's version
=======
laptop's version
>>>>>>> refs/remotes/origin/main
```

If your git config sets `merge.conflictStyle = diff3` or `zdiff3`, there is
also a `|||||||` section holding the common ancestor's version.

While the folder is paused, the desktop daemon neither commits nor pulls.
It won't add anything to the conflict, and later edits wait until you
resolve it. The laptop and other devices carry on syncing as normal.

**Resolve it** in one of three ways:

```sh
# a) edit the file into what you want, then:
dd resolve -dir $DEMO/desktop/Box

# b) keep this device's version of every conflicted file:
dd resolve -dir $DEMO/desktop/Box -ours

# c) keep the server's (the laptop's) version:
dd resolve -dir $DEMO/desktop/Box -theirs
```

With (a), `resolve` refuses while conflict markers are still in a file
(`conflict markers still present in: notes.txt`). Add `-force` if they're
meant to be there.

The resolution is committed and synced, the daemon emits `resolved` and
resumes, and `status -short` returns to `dropgitbox: synced`. Both
versions stay in history, so `dl log -dir $DEMO/laptop/Box notes.txt`
shows them.

## 9. Notifications and hooks (optional)

**Desktop notifications.** Check whether they work on this machine:

```sh
dd notify test
```

On Linux this needs a desktop session and `notify-send` (libnotify) or
`gdbus`. On WSL that means WSLg. On a headless machine, `notify test`
reports `desktop: FAILED: no desktop notification mechanism found` and
exits non-zero. Status, events and hooks still work.

**A hook command.** Stop the desktop daemon (Ctrl-C), then add a hook and
a short offline timer to the desktop's config. The timer means step 11
doesn't take the default 10 minutes:

```sh
cat > $DEMO/desktop/hook.sh <<'EOF'
#!/bin/sh
echo "$(date +%T) $DROPGITBOX_EVENT: $DROPGITBOX_TITLE" >> "$(dirname "$0")/hook.log"
EOF
chmod +x $DEMO/desktop/hook.sh

cat >> $DEMO/desktop/config/dropgitbox/config.toml <<EOF

[notify]
command = ["$DEMO/desktop/hook.sh"]
command_events = ["*"]
offline_after = "30s"
EOF

dd notify test                      # also runs the hook once, with a "test" event
cat $DEMO/desktop/hook.log          # e.g. 14:56:40 test: folder
```

Start `dd watch` again in the **desktop** terminal. In **play**, follow
the log with `tail -f $DEMO/desktop/hook.log` while you work through the
next two steps. With `"*"`, every `syncing` and `synced` is logged too. A
real hook would normally use the default list, which is only the events
worth telling someone about.

A real hook would forward the event to your phone (ntfy, Pushover), a
chat webhook, or a sound. The README's "Notifications and status" section
lists every variable the hook gets.

## 10. Revoke a device

In the **play** terminal:

```sh
ds revoke desktop
```

The token stops working immediately. Within about 25 seconds:

- The server closes the desktop's open event stream. It re-checks open
  streams on every heartbeat.
- The desktop's reconnect is rejected with `401`, and it tries one sync to
  confirm. The **server** terminal logs each rejection as
  `auth failed: invalid token`.
- The desktop daemon emits `unauthorized`, once. Its log, `dd status
  -short` (`dropgitbox: access denied Box`), the hook and a desktop
  notification all show it.

The files already on the desktop stay put.

Bring the desktop back with a new invite:

```sh
ds add desktop -url http://127.0.0.1:8080
```

Stop the desktop daemon. Then run the following, pasting the new invite
after `join -force`:

```sh
dd join -force dgb1.…new invite…
dd watch
```

`ds rotate desktop` is the gentler option. It issues the device a new
token without deleting the device (for when a token may have leaked). The
old token stops working immediately.

## 11. Go offline and come back

Stop the **server** with Ctrl-C, then edit something on the desktop:

```sh
echo "written while offline" >> $DEMO/desktop/Box/notes.txt
```

Here's what you'll see:

- The edit is committed locally right away. Nothing is lost.
- Each failed sync emits `error`, and retries back off (5s, 10s, 20s, and
  so on). `status -short` shows `retrying`.
- After 30 seconds of failing (your `offline_after`), the daemon emits
  `offline` once. That's a desktop notification and a hook run, and
  `status -short` shows `offline`.

Start the server again with the same command as in step 3. To skip
waiting for the next retry, run `dd sync`. It asks the running daemon to
sync now and prints the result. The daemon emits `online` and pushes the
offline commit.

The laptop picks the commit up once its own event stream reconnects. That
stream backs off while the server is down, so it can take up to 30
seconds. Run `dl sync` to fetch it at once.

## 12. Clean up

1. Stop the two daemons and the server with Ctrl-C.
2. Remove everything the demo created:

   ```sh
   rm -rf ~/dgb-demo
   unalias dl dd ds
   ```

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `a dropgitbox daemon is already running` | That device's daemon is still running in another terminal, or you used plain `dropgitbox` instead of `dl` or `dd`. |
| `join`: `connection refused` | The server isn't running, or isn't on `127.0.0.1:8080`. |
| `join`: `the server rejected the token` | The invite was mistyped, or that device was revoked or rotated after the invite was made. Make a new one with `ds add` or `ds rotate`. |
| Changes don't appear on the other device | Is that device's daemon running? `dd status` shows it. `dd sync` forces a sync and prints the result. |
| `notify test`: `no desktop notification mechanism found` | No desktop session, or `notify-send` isn't installed. See step 9. |
| Port 8080 is taken | Use another port in steps 3, 4 and 10, e.g. `-addr 127.0.0.1:18080` with `-url http://127.0.0.1:18080`. |
