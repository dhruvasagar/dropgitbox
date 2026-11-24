# Command reference

<!-- Generated from the command definitions by `go generate ./cmd/dropgitbox`. Do not edit. -->

Every command also prints this with `--help` (or `dropgitbox help COMMAND`), and the
installers include it as the man page `man dropgitbox`. See [the CLI guide](cli.md)
for conventions shared by all commands.

- [dropgitbox](#dropgitbox): Git-backed folder sync
- [dropgitbox server](#dropgitbox-server): Run the server
- [dropgitbox device](#dropgitbox-device): Manage the devices allowed to sync
  - [dropgitbox device add](#dropgitbox-device-add): Register a device and print its invite
  - [dropgitbox device list](#dropgitbox-device-list): List devices and their sync status
  - [dropgitbox device show](#dropgitbox-device-show): Show one device's folders in detail
  - [dropgitbox device rotate](#dropgitbox-device-rotate): Replace a device's token
  - [dropgitbox device revoke](#dropgitbox-device-revoke): Revoke a device's access
- [dropgitbox join](#dropgitbox-join): Connect this device to a server
- [dropgitbox repos](#dropgitbox-repos): List the folders on the server
- [dropgitbox setup](#dropgitbox-setup): Sync a folder with a repository on the server
- [dropgitbox watch](#dropgitbox-watch): Run the sync daemon
- [dropgitbox sync](#dropgitbox-sync): Commit and sync once
- [dropgitbox status](#dropgitbox-status): Show the daemon's state and folder details
- [dropgitbox events](#dropgitbox-events): Stream sync events from the daemon
- [dropgitbox notify](#dropgitbox-notify): Check notifications
  - [dropgitbox notify test](#dropgitbox-notify-test): Send a test notification and run the hook command
- [dropgitbox resolve](#dropgitbox-resolve): Mark conflicted files resolved
- [dropgitbox log](#dropgitbox-log): Show a folder's history
- [dropgitbox restore](#dropgitbox-restore): Bring files back from an earlier commit
- [dropgitbox config](#dropgitbox-config): Show which config file is used
- [dropgitbox version](#dropgitbox-version): Print the version

## dropgitbox

Git-backed folder sync.

```text
dropgitbox COMMAND
```

dropgitbox keeps folders in sync between your devices through a server you run yourself. Every change is committed to git before it is synced, so the full history of every file is kept on every device and on the server.

Set up the server node:

```text
dropgitbox server                 run the server (usually as a system service)
dropgitbox device add NAME        register a device and print its invite
```

Set up each device:

```text
dropgitbox join INVITE            connect to the server
dropgitbox setup --repo NAME --dir DIR
dropgitbox watch                  keep every folder in sync (usually a user service)
```

Settings are read from a TOML config file: `--config` FILE, $DROPGITBOX_CONFIG, or the first that exists of $XDG_CONFIG_HOME/dropgitbox/config.toml, $XDG_CONFIG_DIRS and /etc/dropgitbox/config.toml (`dropgitbox config` shows which is used).

Folder commands take `--dir` DIR. Without it they act on the synced folder containing the current directory, or else on every folder in the config.

Commands:

- [server](#dropgitbox-server): Run the server
- [device](#dropgitbox-device): Manage the devices allowed to sync
- [join](#dropgitbox-join): Connect this device to a server
- [repos](#dropgitbox-repos): List the folders on the server
- [setup](#dropgitbox-setup): Sync a folder with a repository on the server
- [watch](#dropgitbox-watch): Run the sync daemon
- [sync](#dropgitbox-sync): Commit and sync once
- [status](#dropgitbox-status): Show the daemon's state and folder details
- [events](#dropgitbox-events): Stream sync events from the daemon
- [notify](#dropgitbox-notify): Check notifications
- [resolve](#dropgitbox-resolve): Mark conflicted files resolved
- [log](#dropgitbox-log): Show a folder's history
- [restore](#dropgitbox-restore): Bring files back from an earlier commit
- [config](#dropgitbox-config): Show which config file is used
- [version](#dropgitbox-version): Print the version


## dropgitbox server

Run the server.

```text
dropgitbox server [flags]
```

Run the dropgitbox server in the foreground until interrupted. It is usually run as the dropgitbox-server system service instead:

```text
sudo systemctl enable --now dropgitbox-server
```

The server stores one git repository per synced folder in its data directory, lets in only registered devices (see `dropgitbox device`), and tells connected devices when another device pushes, so they sync within seconds.

Settings come from the [server] table in the config; flags override them. Set server.public_url to the URL devices use to reach the server, so `dropgitbox device add` can build invites. Exposed beyond a trusted network, serve HTTPS: with `--tls-cert` and `--tls-key`, or behind a reverse proxy (see docs/deployment.md).

Examples:

```sh
dropgitbox server --config /etc/dropgitbox/config.toml
dropgitbox server --addr 127.0.0.1:8080 --data ~/dropgitbox-data -v
dropgitbox server --addr :8443 --tls-cert cert.pem --tls-key key.pem
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--addr ADDR` |  | listen on ADDR (config: server.addr, default ":8080") |
| `--data DIR` |  | keep repositories and devices in DIR (config: server.data_dir, default $XDG_DATA_HOME/dropgitbox) |
| `--no-auth` |  | disable authentication; trusted networks only (config: server.no_auth) |
| `--tls-cert FILE` |  | serve HTTPS with the certificate in FILE (config: server.tls_cert) |
| `--tls-key FILE` |  | private key FILE for --tls-cert (config: server.tls_key) |
| `--token TOKEN` |  | also accept the shared TOKEN besides device tokens ($DROPGITBOX_TOKEN, config: server.token) |
| `-v`, `--verbose` |  | log in more detail |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox device

Manage the devices allowed to sync.

```text
dropgitbox device COMMAND
```

Register, inspect and revoke the devices allowed to sync with this server.

Run these on the server node, as the server's user or with sudo: they work on the server's data directory directly, and take effect on the running server immediately.

Commands:

- [add](#dropgitbox-device-add): Register a device and print its invite
- [list](#dropgitbox-device-list): List devices and their sync status
- [show](#dropgitbox-device-show): Show one device's folders in detail
- [rotate](#dropgitbox-device-rotate): Replace a device's token
- [revoke](#dropgitbox-device-revoke): Revoke a device's access

Examples:

```sh
sudo dropgitbox device add laptop
sudo dropgitbox device list
sudo dropgitbox device revoke old-phone
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--data DIR` |  | the server's data directory DIR (config: server.data_dir) |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox device add

Register a device and print its invite.

```text
dropgitbox device add NAME [flags]
```

Register a new device called NAME and print an invite for it. On that device, run `dropgitbox join INVITE`.

The invite holds the server's URL (server.public_url, or `--url`) and the device's new access token, so send it over a private channel. add then checks that the URL answers as a dropgitbox server from here, and warns if it doesn't.

Examples:

```sh
dropgitbox device add laptop
dropgitbox device add phone --url https://box.example.com
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--url URL` |  | put URL in the invite as the server's address (config: server.public_url) |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |
| `--data DIR` |  | the server's data directory DIR (config: server.data_dir) |


## dropgitbox device list

List devices and their sync status.

```text
dropgitbox device list
```

List the registered devices: when each was last seen and last pushed, its sync status across its folders (in sync, behind, CONFLICT, ERROR), and the dropgitbox version it runs.

Aliases: `ls`

Examples:

```sh
dropgitbox device list
```

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |
| `--data DIR` |  | the server's data directory DIR (config: server.data_dir) |


## dropgitbox device show

Show one device's folders in detail.

```text
dropgitbox device show NAME
```

Show device NAME in detail: when it was added and last seen, from which address, and for each of its folders the state it last reported, when it last fetched and pushed, and whether it has the server's latest commit.

Examples:

```sh
dropgitbox device show laptop
```

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |
| `--data DIR` |  | the server's data directory DIR (config: server.data_dir) |


## dropgitbox device rotate

Replace a device's token.

```text
dropgitbox device rotate NAME [flags]
```

Issue a new access token for device NAME and print a new invite. The old token stops working immediately.

On that device, run `dropgitbox join --force INVITE` to switch to the new token. Its folders stay as they are.

Examples:

```sh
dropgitbox device rotate laptop
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--url URL` |  | put URL in the invite as the server's address (config: server.public_url) |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |
| `--data DIR` |  | the server's data directory DIR (config: server.data_dir) |


## dropgitbox device revoke

Revoke a device's access.

```text
dropgitbox device revoke NAME
```

Revoke device NAME's access immediately: its token stops working and its open connections are closed. The files already on that device stay there.

Aliases: `remove`, `rm`

Examples:

```sh
dropgitbox device revoke old-phone
```

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |
| `--data DIR` |  | the server's data directory DIR (config: server.data_dir) |


## dropgitbox join

Connect this device to a server.

```text
dropgitbox join INVITE [flags]
```

Connect this device to a dropgitbox server, using an invite printed by `dropgitbox device add NAME` on the server.

The invite is checked with the server, then the device's token is saved to a file readable only by you, and the server URL and device name are saved to the config. join then lists the folders on the server; sync them to this device with `dropgitbox setup`.

To move to a new token (after `dropgitbox device rotate`) or another server, join again with `--force`. Folders already set up stay as they are.

Examples:

```sh
dropgitbox join dgb1.eyJzIjoiaHR0cHM6Ly9ib3guZXhhbXBsZS5jb20i...
dropgitbox join --force dgb1.eyJ...      # after the token was rotated
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--device NAME` |  | name this device NAME in commit messages (default: the name in the invite) |
| `--force` |  | replace the server or token already in the config |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox repos

List the folders on the server.

```text
dropgitbox repos [flags]
```

List the folders (git repositories) on the server, with each one's latest commit and where it is synced on this device, if it is.

Examples:

```sh
dropgitbox repos
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--server URL` |  | ask the server at URL (config: client.server) |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox setup

Sync a folder with a repository on the server.

```text
dropgitbox setup [--repo NAME] [--dir DIR] [flags]
```

Sync the folder DIR (default: the current directory) with the repository NAME on the server, creating the repository if it doesn't exist yet.

Files already in DIR are kept and merged with what is on the server, and DIR is created if it doesn't exist. The folder is then added to the config, so `dropgitbox watch` keeps it in sync; a running daemon picks it up within seconds.

A folder can't be inside another synced folder, and can't contain other git repositories: their files wouldn't sync.

Examples:

```sh
dropgitbox setup --repo notes --dir ~/Notes
cd ~/Photos && dropgitbox setup          # repository "Photos"
dropgitbox setup --repo scratch --dir /tmp/scratch --no-register
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--device NAME` |  | name this device NAME in commit messages (config: client.device, default: hostname) |
| `--dir DIR` | `.` | the folder DIR to sync |
| `--no-register` |  | don't add the folder to the config file |
| `--repo NAME` |  | sync with the repository NAME, created if missing (default: the folder's name) |
| `--server URL` |  | use the server at URL (config: client.server) |
| `--token TOKEN` |  | authenticate with TOKEN ($DROPGITBOX_TOKEN, config: client.token_file) |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox watch

Run the sync daemon.

```text
dropgitbox watch [flags]
```

Run the sync daemon in the foreground until interrupted. It is usually run as the dropgitbox user service instead:

```text
systemctl --user enable --now dropgitbox
```

Each folder is synced when its files change (once they have been quiet for `--debounce`), as soon as the server announces changes from another device, and every `--poll` as a safety net. On a merge conflict, syncing of that folder pauses until you resolve it (`dropgitbox resolve`); other folders carry on.

Without `--dir`, every folder in the config is synced, and folders added to or removed from the config are picked up while the daemon runs. Configured folders not yet set up on this device are set up first.

The daemon shows desktop notifications for conflicts and lost connections, and can run a command on events ([notify] in the config). `dropgitbox status`, "events" and "sync" talk to it while it runs. Only one daemon runs per user.

Examples:

```sh
dropgitbox watch
dropgitbox watch --dir ~/Notes -v
dropgitbox watch --debounce 10s --no-desktop-notify
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--debounce duration` | `2s` | commit once files have been quiet this long (config: client.debounce) |
| `--dir DIR` |  | sync only the folder DIR (default: every configured folder) |
| `--max-delay duration` | `30s` | longest a burst of changes may postpone a commit (config: client.max_delay) |
| `--no-desktop-notify` |  | don't show desktop notifications (config: notify.desktop) |
| `--poll duration` | `5m0s` | sync at least this often, in case a notification is missed (config: client.poll) |
| `-v`, `--verbose` |  | log in more detail |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox sync

Commit and sync once.

```text
dropgitbox sync [flags]
```

Commit local changes and sync with the server once, then exit.

If the daemon is running, sync asks it to sync the folders it manages, so two processes never run git in the same folder at once; it waits for the result. `--direct` syncs in this process anyway. Configured folders not yet set up on this device are set up first.

Examples:

```sh
dropgitbox sync
dropgitbox sync --dir ~/Notes
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--dir DIR` |  | act on the synced folder DIR (default: the one containing the current directory, or every configured folder) |
| `--direct` |  | sync in this process even if the daemon is running |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox status

Show the daemon's state and folder details.

```text
dropgitbox status [flags]
```

Show whether the daemon is running and, for each folder, its server and repository, the daemon's live state (in sync, pending, syncing, PAUSED on a conflict, retrying, OFFLINE, ACCESS DENIED or STOPPED), and how far it is ahead of or behind the server.

`--short` prints one line for status bars and shell prompts, naming the most urgent state across all folders, such as `dropgitbox: synced` or `dropgitbox: CONFLICT notes`. `--json` prints the daemon's full live state. Neither needs the config.

Examples:

```sh
dropgitbox status
dropgitbox status --short
dropgitbox status --json | jq '.folders[] | {folder, state}'
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--dir DIR` |  | act on the synced folder DIR (default: the one containing the current directory, or every configured folder) |
| `--json` |  | print the daemon's live state as JSON |
| `--short` |  | print one line summarising the daemon's state |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox events

Stream sync events from the daemon.

```text
dropgitbox events [flags]
```

Print the running daemon's events as they happen, until interrupted: one line per event with its time, type and description.

Event types: pending, syncing, synced, remote_changes, conflict, resolved, error, offline, online, unauthorized.

`--json` prints each event as a JSON object on its own line, for scripts. To run a command on events instead, set notify.command in the config.

Examples:

```sh
dropgitbox events
dropgitbox events --json | jq -r 'select(.type == "conflict") | .folder'
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--json` |  | print events as JSON lines |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox notify

Check notifications.

```text
dropgitbox notify COMMAND
```

Check how the daemon notifies you about sync events: desktop notifications, and the command run on events (notify.command in the config).

Commands:

- [test](#dropgitbox-notify-test): Send a test notification and run the hook command

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox notify test

Send a test notification and run the hook command.

```text
dropgitbox notify test
```

Send a test desktop notification, and run notify.command (if configured) with a test event, reporting whether each worked. Use it after changing the [notify] settings in the config.

Examples:

```sh
dropgitbox notify test
```

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox resolve

Mark conflicted files resolved.

```text
dropgitbox resolve [--ours | --theirs] [PATH...] [flags]
```

Mark conflicted files resolved, like "git add" during a merge. Once no conflicted file is left, the merge is committed and the folder syncs again.

When changes on this device conflict with changes from the server, syncing of that folder pauses; `dropgitbox status` lists the conflicted files. For each one, either:

```text
- edit it: keep what you want and remove the <<<<<<< ======= >>>>>>>
  markers, then run "dropgitbox resolve FILE";
- or keep one side with --ours (this device's version) or --theirs (the
  server's version), which may mean deleting the file if that side did.
```

PATHs are files or directories, relative to the current directory (or to `--dir`). Without PATHs, resolve acts on every conflicted file. It refuses files that still have conflict markers (unless `--force`), and, unless named or given a side, files that have no markers to edit: binary files and files deleted on one side.

Only the merge and the resolved files are committed. Other changes made while the folder was paused are synced afterwards, as their own commit. The sync goes through the daemon when it is running.

Examples:

```sh
dropgitbox status                  # what is conflicted
dropgitbox resolve notes.txt       # after editing out the markers
dropgitbox resolve --theirs photo.jpg
dropgitbox resolve                 # every conflicted file, as edited
dropgitbox resolve --dir ~/Notes --ours
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--dir DIR` | `.` | the synced folder DIR; PATHs are relative to it |
| `--force` |  | accept files that still contain conflict markers |
| `--no-sync` |  | commit the merge but don't sync |
| `--ours` |  | keep this device's version of the files |
| `--theirs` |  | keep the server's version of the files |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox log

Show a folder's history.

```text
dropgitbox log [PATH...] [flags]
```

Show a synced folder's history: the latest commits, newest first, each with its date, a summary, and the files it changed. PATHs (files or directories, relative to the current directory or to `--dir`) limit the history to commits that touched them.

Use a commit's ID with `dropgitbox restore` to bring files back.

Examples:

```sh
dropgitbox log
dropgitbox log -n 5 notes/todo.md
dropgitbox log --dir ~/Notes
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--dir DIR` | `.` | the synced folder DIR |
| `-n`, `--number int` | `20` | show at most this many commits |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox restore

Bring files back from an earlier commit.

```text
dropgitbox restore --rev REV PATH... [flags]
```

Bring PATHs back to how they were at commit REV, as listed by `dropgitbox log`. PATHs are relative to the current directory (or to `--dir`). The restored files are ordinary changes: they are committed and synced like any other edit, so restoring never rewrites history and can itself be undone.

Examples:

```sh
dropgitbox log notes/todo.md
dropgitbox restore --rev 3f2a1c9 notes/todo.md
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--dir DIR` | `.` | the synced folder DIR |
| `--rev REV` |  | restore from commit REV (see dropgitbox log) |

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox config

Show which config file is used.

```text
dropgitbox config
```

Show which config file is used, the order config files are searched in, and the file that commands such as join and setup save settings to.

packaging/config.example.toml in the release archive (or /usr/share/doc/dropgitbox/config.example.toml from a package) documents every setting.

Examples:

```sh
dropgitbox config
dropgitbox --config ~/box.toml config
```

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |


## dropgitbox version

Print the version.

```text
dropgitbox version
```

Print dropgitbox's version, and the commit and date it was built from.

Inherited flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config FILE` |  | read settings from FILE (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc) |

