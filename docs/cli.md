# Command line

One binary, `dropgitbox`, provides every command, for the server node and
for devices alike. This page covers what all commands have in common. The
[command reference](cli-reference.md) documents each command and flag, and
is generated from the same definitions as `--help`.

## Commands

| Command | What it does | Where |
| --- | --- | --- |
| `server` | Run the server | Server |
| `device add\|list\|show\|rotate\|revoke` | Invite devices, check on them, replace or revoke their tokens | Server |
| `join INVITE` | Connect this device to a server | Device |
| `repos` | List the folders on the server, and where each is synced here | Device |
| `setup --repo NAME --dir DIR` | Start syncing a folder | Device |
| `watch` | Run the sync daemon | Device |
| `sync` | Commit and sync now | Device |
| `status [--short\|--json]` | The daemon's state, and each folder's details | Device |
| `events [--json]` | Stream the daemon's events | Device |
| `notify test` | Check desktop notifications and the hook command | Device |
| `resolve [--ours\|--theirs] [PATH...]` | Mark conflicted files resolved; the last one finishes the merge | Device |
| `log [PATH...]` | Show a folder's history | Device |
| `restore --rev REV PATH...` | Bring files back from an earlier commit | Device |
| `config` | Show which config file is used | Both |
| `version` | Print the version | Both |
| `completion bash\|zsh\|fish` | Print a shell completion script | Both |

## Getting help

Every command and subcommand explains itself, with a description, examples
and its flags:

```sh
dropgitbox --help              # every command, grouped
dropgitbox setup --help        # one command; also: dropgitbox help setup
dropgitbox device add --help
man dropgitbox                 # everything on one page
```

`install.sh` and the packages install the man page. The release archives
have it in `man/dropgitbox.1`.

## Flags

- Long flags take two dashes: `--dir ~/Box`, or `--dir=$HOME/Box`. A long
  flag typed with one dash gets a hint to use two.
- The one-letter flags are `-h` (help), `-v` (version, or `--verbose` for
  `server` and `watch`) and `-n` (`log --number`).
- Flags and arguments can come in any order: `device add laptop --url URL`
  and `device add --url URL laptop` are the same. After `--`, everything is
  an argument, for file names starting with a dash.
- `--config FILE` works with every command.

## Which folder

Commands that act on folders take `--dir DIR`:

- `sync`, `status` and `watch` act on `--dir`; else on the synced folder
  containing the current directory; else on every folder in the config.
- `resolve`, `log` and `restore` act on one folder: `--dir`, or the one
  containing the current directory. Their PATH arguments are relative to
  the current directory, as in git, or to `--dir` when it's given, like
  `git -C`.

## Exit status

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | The command failed. The reason is printed on stderr, starting `dropgitbox:` |
| `2` | The command was used wrongly: an unknown command or flag, a missing argument, or flags that can't go together. A hint points to its `--help` |

For example, `sync` exits with `1` if any folder failed to sync, and
`status --short` always exits with `0`, so it's safe in a status bar.

## Shell completion

Completion covers commands and flags, plus device names for
`device show`, `rotate` and `revoke`, the repositories on the server for
`setup --repo`, and the conflicted files for `resolve`.

The `.deb` and `.rpm` packages install it for bash, zsh and fish. The
release archives carry the scripts in `completions/`. Otherwise, generate
them from the binary:

```sh
# bash (needs the bash-completion package)
dropgitbox completion bash > ~/.local/share/bash-completion/completions/dropgitbox
# zsh: add  fpath=(~/.zfunc $fpath)  before compinit in ~/.zshrc
mkdir -p ~/.zfunc && dropgitbox completion zsh > ~/.zfunc/_dropgitbox
# fish
dropgitbox completion fish > ~/.config/fish/completions/dropgitbox.fish
```

Start a new shell afterwards.
