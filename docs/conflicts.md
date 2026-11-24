# Conflicts

dropgitbox merges changes from different devices automatically whenever
git can: edits to different files, or to different parts of the same text
file. A **conflict** is what's left: both devices changed the same lines,
or one changed a file the other deleted, or both changed the same binary
file. dropgitbox never guesses which version wins. It **stops and asks**.

## What happens

1. **A sync finds the conflict.** The daemon commits this device's changes,
   fetches the server's, and merges. Git combines everything it can, then
   leaves the merge unfinished: the conflicted text files get markers, and
   `.git/MERGE_HEAD` records the merge in progress.

   ```text
   <<<<<<< HEAD
   this device's version of the lines
   =======
   the server's version of the lines
   >>>>>>> refs/remotes/origin/main
   ```

2. **That folder pauses.** As long as the merge is unfinished, the daemon
   doesn't sync the folder at all. File changes, notices from the server,
   the 5-minute poll and `dropgitbox sync` are all ignored for it. In
   particular, nothing is committed, so the markers can never be synced to
   your other devices by accident. You can keep editing; your edits stay
   in the folder until the conflict is resolved.

   Only that folder pauses. Other folders keep syncing, and other devices
   keep syncing with the server as usual: one device's conflict never
   blocks the rest.

3. **You're told once.** The daemon emits a `conflict` event naming the
   files. That's a desktop notification (and your hook command, if you set
   one), and `dropgitbox status` shows the folder as
   `PAUSED: conflict in notes.txt`, `status --short` as
   `dropgitbox: CONFLICT Box`. The device also reports the conflict to the
   server, so `dropgitbox device list` shows it there. A paused folder stays
   paused across daemon restarts and reboots, and is reported again when
   the daemon starts.

## Resolving it

Resolving works like finishing a merge in git: decide each file, mark it
resolved, and the merge is committed once none is left. The difference is
that there's no separate commit step: resolving the last file commits the
merge and syncs.

First see what's conflicted, with `dropgitbox status`:

```console
$ dropgitbox status
...
SYNC PAUSED in /home/me/Box: changes on this device conflict with changes from the server.

Conflicted files (us: this device, them: the server):
  both modified:   notes/todo.txt
  both modified:   photo.jpg  (binary: no markers; pick a side, or name the file to keep it as it is)
  deleted by them: old-plan.md

For each file, either:
  - edit it to remove the <<<<<<< ======= >>>>>>> markers, then: dropgitbox resolve FILE
  - keep this device's version:  dropgitbox resolve --ours FILE
  - keep the server's version:   dropgitbox resolve --theirs FILE
Syncing resumes once every file is resolved.
```

The labels are git's own. "Us" is this device and "them" is the server,
meaning what the other devices have. `deleted by them` means it was
changed here but deleted elsewhere, and `deleted by us` the other way
round.

Then, for each file, either:

- **Combine them by hand.** Edit the file into what you want, delete the
  `<<<<<<<`, `=======` and `>>>>>>>` lines, and run
  `dropgitbox resolve FILE`. This is `git add FILE`.
- **Keep one side** with `dropgitbox resolve --ours FILE` (this device's
  version) or `--theirs FILE` (the server's). This is
  `git checkout --ours/--theirs FILE` followed by `git add`. If the chosen
  side deleted the file, it's deleted.

```console
$ cd ~/Box
$ $EDITOR notes/todo.txt
$ dropgitbox resolve notes/todo.txt
resolved: notes/todo.txt

Conflicted files (us: this device, them: the server):
  both modified:   photo.jpg  (binary: ...)
  deleted by them: old-plan.md

Syncing resumes once these are resolved too.

$ dropgitbox resolve --theirs photo.jpg old-plan.md
resolved: old-plan.md
resolved: photo.jpg
All conflicts resolved; merge committed as 6f645a3d72.
/home/me/Box: synced
```

The finer points:

- **Paths** are relative to where you are, as in git, and may name
  directories (`resolve --theirs photos/`). From outside the folder, use
  `--dir`: paths are then relative to it, like `git -C`. Shell completion
  offers the conflicted files.
- **Without paths**, `resolve` acts on every conflicted file:
  `resolve --theirs` keeps the server's version of everything, and plain
  `resolve` takes every file as you've edited it.
- **Markers are checked.** `resolve` refuses a file that still has lines
  starting with `<<<<<<< ` or `>>>>>>> `, and then changes nothing. If such
  a line is genuinely meant to be there (a document about git, say), pass
  `--force`.
- **Files with nothing to edit need a decision.** Binary files and files
  deleted on one side have no markers, so plain `resolve` won't take them
  silently. Pick a side, or name the file to keep it as it is in the
  folder.
- **The sync after the merge** goes through the daemon when it's running,
  so the two never run git in the folder at once. `--no-sync` only commits
  the merge and leaves the sync to the daemon.

Both versions stay in history whatever you choose: the merge commit has
your commit and the server's as parents. See [history](history.md) to
bring back the version you didn't keep.

## How syncing resumes

Syncing resumes **by itself**, but only once the merge is finished, not
when the markers disappear. When `dropgitbox resolve` commits the merge, it
asks the daemon to sync, and the daemon resumes at once. A merge finished
any other way is noticed within 2 seconds. Either way, the folder goes back
to normal, a `resolved` event fires (a desktop notification by default),
and a sync starts. If other devices pushed again during the pause, that
sync merges their changes too, which can occasionally conflict again.

Saving a file without markers is deliberately not enough: an automatic
commit then would sync a half-finished edit to every device, and binary
files never have markers to look for. Marking files resolved is an
explicit step.

Anything that finishes the merge counts, not only `dropgitbox resolve`:

| You run | Effect |
| --- | --- |
| `dropgitbox resolve` | Marks files resolved; the last one commits the merge and syncs. The usual way |
| `git add` and `git commit` in the folder | The same, without the marker check; the daemon resumes and syncs |
| `git merge --abort` | Puts the folder back as it was before the merge. The daemon resumes, and the next sync merges again, so the same conflict comes back |

## Special cases

### Binary files

Git can't merge binary files (images, PDFs, office documents) and puts no
markers in them. The file in the folder is **this device's version**.
Resolve it with `--ours` or `--theirs`, or put the version you want in
place and run `dropgitbox resolve FILE`. The other version is in history
either way.

### Changed on one device, deleted on another

The changed file is left in the folder. `resolve --ours` or `--theirs`
keeps or deletes it according to that side. Naming the file keeps it if
it's there, and deletes it if you've deleted it.

### Edits made while paused

You can keep working while a folder is paused. Changes to files that
aren't conflicted are left out of the merge commit, and are committed and
synced as usual right after it, as their own commit. Only the merge and the
files you resolved go into the merge commit.

### Several devices

The first device to push its change wins the race and syncs normally. The
conflict appears on the device that syncs second, when it merges the first
one's change, and that device resolves it. The others carry on, and get the
resolution with their next sync. On the server, `dropgitbox device list`
shows which device is paused, and `dropgitbox device show NAME` lists its
conflicted files.

## Avoiding conflicts

- Keep the daemon running on every device (`systemctl --user enable --now
  dropgitbox`, or `install.sh --service`). Changes then reach other devices
  within seconds, so editing the same thing in two places in between is
  rare.
- A device that has been offline catches up all at once when it
  reconnects, which is when conflicts are most likely. Check
  `dropgitbox status` after a long time offline.
- Many applications keep a file open and rewrite it as a whole. That's
  fine for one device at a time, but editing the same document on two
  devices at once will conflict.
