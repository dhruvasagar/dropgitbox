# dropgitbox

A Dropbox-like folder sync service that uses git behind the scenes. Every
change is **auto-committed before it is synced**, so every device and the
server hold the complete history of every file. Any past version can be
listed and restored, and history can never be rewritten on the server.

```
 laptop                         server                         desktop
┌──────────────────┐          ┌───────────────────┐          ┌──────────────────┐
│ ~/Box (git tree) │  push    │ data/docs.git     │  push    │ ~/Box (git tree) │
│ dropgitbox watch │ ───────► │ (bare repo)       │ ◄─────── │ dropgitbox watch │
│  fsnotify        │ ◄─────── │ git smart HTTP    │ ───────► │  fsnotify        │
│  auto-commit     │  fetch   │ /events  (SSE) ───┼─ notify ►│  auto-commit     │
└──────────────────┘          └───────────────────┘          └──────────────────┘
```

## How it works

**Server** (`dropgitbox server`) keeps one bare git repository per synced
folder. It serves git's smart HTTP protocol through `git http-backend`, and
after every push it notifies subscribed devices over a Server-Sent Events
stream, so other devices sync within moments instead of polling. The
repositories refuse force-pushes and branch deletion
(`receive.denyNonFastForwards`, `receive.denyDeletes`), so history is
append-only.

**Client daemon** (`dropgitbox watch`) watches the folder recursively. After
a change it waits for a quiet period (2s by default, 30s at most during
continuous writes), then:

1. `git add --all` and commits with a descriptive message, for example
   `Modified notes/todo.txt` or `3 files: 2 added, 1 deleted`. The commit
   body names the device and lists the changed files.
2. fetches from the server and merges (a fast-forward when possible).
3. pushes, retrying when another device pushed in between.

It also syncs when the server announces a push, every 5 minutes as a safety
net, and with backoff after a network error.

**Conflicts stop and ask.** If two devices change the same lines, the merge
leaves standard git conflict markers in the file, and that device **pauses
syncing**: nothing is auto-committed, so the markers can never be synced by
accident. You get a log warning and, if `notify-send` is available, a
desktop notification. Once you run `dropgitbox resolve`, syncing resumes by
itself. Edits to different parts of a file, or to different files, merge
automatically.

## Requirements

- Go 1.26+ to build
- git 2.28+ on the server and on every client (the server needs
  `git-http-backend`, which ships with git)

## Build

```sh
go build -o dropgitbox ./cmd/dropgitbox
```

## Usage

### 1. Run the server

```sh
export DROPGITBOX_TOKEN=$(openssl rand -hex 32)
dropgitbox server -addr :8080 -data /srv/dropgitbox
```

Put it behind a TLS reverse proxy (Caddy, nginx, …) for anything beyond a
trusted LAN, because the token is sent in an HTTP header. With nginx, keep
response buffering off for `/api/v1/repos/*/events` (the server already
sends `X-Accel-Buffering: no`).

### 2. Set up each device

```sh
export DROPGITBOX_TOKEN=...          # same token as the server
dropgitbox setup -server https://box.example.com -repo docs -dir ~/Box
dropgitbox watch -dir ~/Box          # run under systemd, launchd, etc.
```

`setup` creates the repository on the server if needed. You can point it at
a folder that already has files: they are kept and merged with whatever is
already on the server. The token is stored in `.git/dropgitbox-token` (mode
0600). It is passed to git through the environment, never in the remote URL
or `.git/config`.

### Commands

| Command | What it does |
| --- | --- |
| `dropgitbox watch [-dir D]` | Run the sync daemon (`-debounce`, `-max-delay`, `-poll`, `-v`) |
| `dropgitbox sync [-dir D]` | Commit and sync once |
| `dropgitbox status [-dir D]` | Show head, ahead/behind, uncommitted changes and any conflict |
| `dropgitbox resolve [-ours\|-theirs] [-force] [PATH...]` | Finish a paused merge, then sync |
| `dropgitbox log [-n N] [PATH...]` | Show history, optionally for specific files |
| `dropgitbox restore -rev REV PATH...` | Bring back an old version. It is committed as a *new* change, so nothing is lost |

### Resolving a conflict

```console
$ dropgitbox status -dir ~/Box
...
SYNC PAUSED: changes on this device conflict with changes from the server.
Conflicted files:
  notes/todo.txt

$ $EDITOR ~/Box/notes/todo.txt        # remove the <<<<<<< ======= >>>>>>> markers
$ dropgitbox resolve -dir ~/Box
Conflict resolved in commit faf87cdbdc.
Synced.
```

Or keep one side without editing: `dropgitbox resolve -ours` keeps this
device's version and `-theirs` keeps the server's, either for every
conflicted file or only for the paths you list. Both versions stay in
history either way.

### Plain git works too

Every synced folder is an ordinary git repository, and the server accepts
HTTP Basic auth with the token as the password:

```sh
git clone https://anything:$DROPGITBOX_TOKEN@box.example.com/git/docs.git
```

## Server API

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check (no auth) |
| `GET /api/v1/repos` | List repositories and their heads |
| `GET /api/v1/repos/{name}` | One repository's head |
| `POST /api/v1/repos/{name}` | Create a repository (idempotent) |
| `GET /api/v1/repos/{name}/events` | SSE stream of `{"repo","head"}` events: one on connect, then one per push |
| `/git/{name}.git/...` | git smart HTTP (clone, fetch, push) |

Authentication is `Authorization: Bearer <token>`, or Basic auth with the
token as the password.

## Layout

```
cmd/dropgitbox/      CLI entry point and subcommands
internal/server/     HTTP server: auth, repo management, git-http-backend, SSE hub
internal/client/     setup, sync engine, conflict handling, file watcher, daemon
internal/gitx/       thin wrapper around the git CLI
```

## Testing

```sh
go test -race ./...
```

The integration tests start a real server and drive several simulated
devices through syncing, merging, conflicts (including modify/delete),
restores, history-rewrite protection, and live syncing via the daemon.

## Known limitations

- **Large or binary files.** Every version of every file is kept forever,
  in full, on every device. That is the point, but it makes dropgitbox a
  poor fit for large media. Git LFS support would be the natural next step.
- **Empty directories** are not synced, because git does not track them.
- **Nested git repositories** inside a synced folder are recorded as
  gitlinks, not their contents. Don't put other repos inside a synced folder.
- **One shared token** for the whole server; there are no per-user accounts
  or per-repo permissions yet.
- Binary-file conflicts produce no markers, and this device's version is
  left in place. Pick one with `-ours` or `-theirs`, or put the right file
  in place, before running `resolve`.
