# Using a device

A device is any machine that syncs folders with your server: a laptop, a
desktop, a work machine. This guide covers connecting one, choosing what it
syncs, running the sync daemon, and looking after it. For setting up the
server, see [running the server](server.md).

## Joining a server

Every device gets its own invite, made on the server with
`sudo dropgitbox device add NAME`. The invite is a single string holding the
server's URL, the device's name and its token. On the device:

```sh
dropgitbox join dgb1.eyJ...
```

`join` checks the invite with the server, then saves:

- the device token to `~/.config/dropgitbox/token`, readable only by you;
- the server URL and device name to the `[client]` table of
  `~/.config/dropgitbox/config.toml`.

It then lists the folders already on the server. `--device NAME` uses
another name for this device in commit messages. If the server URL uses
plain `http://`, `join` warns that the token and files travel unencrypted:
fine on a trusted LAN or inside a VPN, not otherwise.

A device joins one server. To switch servers, or to use a new token after
`device rotate`, join again with `--force`. See
[when the token changes](#when-the-token-changes).

## Choosing folders

`dropgitbox repos` lists the folders on the server and where each is synced
on this device. To sync one, or to create a new one:

```sh
dropgitbox setup --repo Notes --dir ~/Notes
```

`setup`:

1. creates the repository on the server if it doesn't exist yet;
2. creates the folder if needed, or adopts an existing one: files already
   in it are kept, and merged with what's on the server;
3. runs the first sync, which downloads the folder's whole history;
4. adds the folder to `[[folders]]` in the config, so the daemon syncs it.
   A running daemon picks it up within a few seconds. `--no-register`
   skips this step.

Without `--repo`, the repository is named after the folder; without
`--dir`, the folder is the current directory. Repository names may contain
letters, digits, `.`, `_` and `-`, start with a letter or digit, are at
most 100 characters, and can't end in `.git`.

If the folder's files conflict with the server's (two different versions of
the same file), the first sync pauses on a conflict, just like a later one
would. See [conflicts](conflicts.md).

### Several folders

Run `setup` once per folder:

```sh
dropgitbox setup --repo Notes  --dir ~/Notes
dropgitbox setup --repo Photos --dir ~/Pictures/Synced
```

Each folder has its own repository and history, and its own line in
`dropgitbox status`. A conflict or error in one doesn't affect the others.
Each device picks its own set of folders, and a repository can live at a
different path on each device.

### Folders can't be nested

`setup` refuses a folder that is inside another synced folder, or that
contains any git repository. Git records a repository inside another one
only as a pointer to a commit, not as files, so those files would silently
not sync. A git repository *around* the folder is fine, such as a dotfiles
repository at `~/.git`.

If a repository appears inside a synced folder later, say you `git clone`
into it, the next sync refuses to commit it. That folder shows as
`STOPPED` in `dropgitbox status`, naming the repository; the others carry
on. Move the repository out (or delete its `.git`), then restart the daemon.

### Setting up another device quickly

The config file holds no secrets, and uses `~` paths, so it can live in a
dotfiles repository. Copy the `[[folders]]` list from an existing device to
the new one, run `dropgitbox join`, then start the daemon: it sets up
every listed folder that doesn't exist on this device yet.

## The sync daemon

`dropgitbox watch` is the daemon that keeps every configured folder in
sync. Run it as a service, so it starts when you log in:

```sh
# Linux (from the .deb/.rpm package, or install.sh --service)
systemctl --user enable --now dropgitbox
journalctl --user -u dropgitbox -f              # its log

# macOS: install.sh --service installs a launchd agent
tail -f ~/Library/Logs/dropgitbox.log
```

Controlling the service:

| | Linux (systemd user unit) | macOS (launchd agent) |
| --- | --- | --- |
| Restart | `systemctl --user restart dropgitbox` | `launchctl kickstart -k gui/$(id -u)/com.github.dhruvasagar.dropgitbox` |
| Stop | `systemctl --user stop dropgitbox` | `launchctl bootout gui/$(id -u)/com.github.dhruvasagar.dropgitbox` |
| Start | `systemctl --user start dropgitbox` | `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.dhruvasagar.dropgitbox.plist` |
| Log | `journalctl --user -u dropgitbox` | `~/Library/Logs/dropgitbox.log` |

On a Linux machine that nobody logs in to, run
`sudo loginctl enable-linger $USER` so the user service runs anyway.

Only one daemon runs per user; starting a second one is refused. To run it
in a terminal instead, stop the service and run `dropgitbox watch` (add
`-v` for a detailed log, or `--dir DIR` to sync just one folder).

When it starts, the daemon sets up any configured folder that isn't set up
on this device yet. While it runs, it follows the config file: folders
added with `setup`, or removed from `[[folders]]`, start or stop syncing
within a couple of seconds.

When the daemon syncs, and how quickly, is described in
[how it works](how-it-works.md#the-sync-cycle). The timing can be changed
with `client.debounce`, `client.max_delay` and `client.poll` in the config,
or the matching `watch` flags.

## Syncing now

```sh
dropgitbox sync
```

commits and syncs the folder you're in, or every configured folder when run
elsewhere (`--dir DIR` picks one). If the daemon is running, `sync` asks it
to do the work and prints each folder's outcome, so two processes never run
git in one folder at once. `--direct` syncs in the `sync` process instead.

## Checking status

```console
$ dropgitbox status
Daemon:  running (pid 4242, version 0.1.0, up 3h2m)

Folder:  /home/me/Box
Server:  https://box.example.com
Repo:    Box
Device:  laptop
State:   in sync (last synced 12s ago)
Head:    3f2a1c9e8b...
Server head (last fetched): 3f2a1c9e8b...
Ahead 0 / behind 0, 0 uncommitted change(s)
```

The `State` line is the daemon's live view of the folder:

| State | Meaning | What to do |
| --- | --- | --- |
| `in sync` | Everything is synced | Nothing |
| `changes pending` | Local changes are waiting for the folder to go quiet | Nothing |
| `syncing...` | A sync is running | Nothing |
| `PAUSED: conflict in ...` | Changes from two devices conflict | [Resolve the conflict](conflicts.md) |
| `retrying after error: ...` | The last sync failed; it is retried with backoff | Usually nothing: a network blip |
| `OFFLINE since ...` | Syncing has kept failing for `notify.offline_after` (10 minutes) | Check the network and the server |
| `ACCESS DENIED: ...` | The server rejected this device's token | See [when the token changes](#when-the-token-changes) |
| `STOPPED, not syncing: ...` | The folder can't be synced until you fix something | Read the message: usually a [nested repository](#folders-cant-be-nested) |

Without a running daemon there's no `State` line, but the rest is still
shown. `Ahead` counts commits not yet on the server, `behind` commits from
the server not yet merged.

For status bars and scripts, `status --short` prints one line, and
`status --json` the daemon's full state. See
[notifications and status](notifications.md).

## Which folder a command acts on

- `sync` and `status`, and `watch`: the folder given with `--dir`; else the
  synced folder containing the current directory; else every folder in the
  config.
- `resolve`, `log` and `restore` work on one folder: the one containing the
  current directory, or `--dir`. Their file arguments are relative to the
  current directory, or to `--dir` when it's given.

## When the token changes

After `sudo dropgitbox device rotate NAME` or `device revoke NAME` on the
server, the device's old token stops working at once. The daemon notices
within about 25 seconds, shows `ACCESS DENIED`, and sends one
notification. The files on the device stay where they are.

To get the device syncing again, get a new invite (`device rotate NAME`, or
`device add NAME` for a revoked device), then on the device:

```sh
dropgitbox join --force dgb1.eyJ...new invite...
systemctl --user restart dropgitbox     # the daemon reads the token when it starts
```

(On macOS, restart it with `launchctl kickstart`; see
[the sync daemon](#the-sync-daemon).)

## Moving a folder

Stop the daemon, move the folder, update its `path` in `[[folders]]`, and
start the daemon again (on macOS, with the `launchctl` commands in
[the sync daemon](#the-sync-daemon)):

```sh
systemctl --user stop dropgitbox
mv ~/Notes ~/Documents/Notes
$EDITOR ~/.config/dropgitbox/config.toml     # path = "~/Documents/Notes"
systemctl --user start dropgitbox
```

The folder keeps its history and its link to the server, which live in its
`.git` directory.

## Stopping syncing a folder

Remove its `[[folders]]` entry from the config. The daemon stops syncing it
within a couple of seconds. Its files stay where they are, along with its
`.git` directory and history. Delete `.git` too if you want a plain folder
again. The folder stays on the server and on other devices; to delete it
there, see [running the server](server.md#deleting-a-folder).

## Moving to another server

Join the new server with `--force`, then run `setup` again for each folder
you want to keep. `setup` points the existing folder at the new server,
creates the repository there, and pushes the folder's whole history to it.

```sh
dropgitbox join --force dgb1.eyJ...invite from the new server...
dropgitbox setup --repo Notes --dir ~/Notes
```

## Uninstalling

Stop and disable the service (`systemctl --user disable --now dropgitbox`,
or `launchctl bootout gui/$(id -u)/com.github.dhruvasagar.dropgitbox` on
macOS), then remove the package or the binary. Your folders stay as they
are. `~/.config/dropgitbox` holds the config and token.
