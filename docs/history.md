# History and restoring

Every change in a synced folder becomes a commit before it is synced, so
each device, and the server, holds every version of every file. Nothing
dropgitbox does ever removes a version: restoring, resolving conflicts and
deleting files all add new commits.

## Browsing history

```console
$ dropgitbox log --dir ~/Box
3f2a1c9  2026-11-23 14:02:11  Modified notes/todo.md
 notes/todo.md | 3 ++-
b71e004  2026-11-23 13:58:40  3 files: 2 added, 1 deleted
 photos/a.jpg  | Bin 0 -> 48213 bytes
 ...
```

Each entry shows the commit ID, its date, a summary, and the files it
changed. Newest come first; `-n` sets how many (20 by default). Give paths
to see only the commits that touched them, including files that have since
been deleted:

```sh
dropgitbox log notes/todo.md
dropgitbox log -n 100 photos/
```

Run `log` inside the folder, or point at it with `--dir`.

## Bringing back an old version

```sh
dropgitbox restore --rev 3f2a1c9 notes/todo.md
```

This puts `notes/todo.md` back exactly as it was in commit `3f2a1c9`. It
doesn't rewind history: the restored file is just a new change, which the
daemon commits and syncs like any edit (or run `dropgitbox sync`). You can
therefore undo a restore by restoring again.

`--rev` takes anything git understands as a commit: an ID from `log`,
or `HEAD~3` for three commits ago. Several paths, and whole directories,
can be restored at once.

**A deleted file:** find the commit that deleted it, and restore from the
commit before it, with `^`:

```sh
dropgitbox log notes/old-idea.md      # the newest entry is the deletion, say 9c0d2e1
dropgitbox restore --rev 9c0d2e1^ notes/old-idea.md
```

`restore` refuses while the folder is paused on a conflict. Resolve that
first (see [conflicts](conflicts.md)).

## Using git directly

Every synced folder is an ordinary git repository, so every git tool works
on it. Commands that only read are always safe:

```sh
cd ~/Box
git log -p notes/todo.md                 # every change to a file, with diffs
git show 3f2a1c9:notes/todo.md           # print an old version without restoring it
git diff HEAD~10 -- notes/               # what changed recently
git log -5                               # the commit body names the device that made it
gitk / tig / lazygit                     # graphical and terminal browsers
```

Leave committing, merging and pushing to dropgitbox. If you do commit by
hand, that's fine: the next sync pushes it. The server refuses any push
that would rewrite or delete history, so mistakes can't spread there.

To get a folder's history somewhere dropgitbox isn't installed, clone it
from the server with plain git and a device token:

```sh
git clone https://x:$TOKEN@box.example.com/git/Box.git
```

That saves the token in the clone's `.git/config`, so give it a token of
its own (`dropgitbox device add archive`) that you can revoke later.

## History is kept forever

The upside of keeping everything is that nothing is ever lost. The
downside is size: a folder takes the space of every version of every file
it ever held, on every device and on the server. Large or frequently
changing binary files make the history grow quickly. See
[limitations](how-it-works.md#limitations).
