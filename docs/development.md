# Development

## Layout

```
cmd/dropgitbox/      main: builds the command line from internal/cli
internal/cli/        the commands: definitions, help text, flags, completion
internal/client/     setup, the sync engine, conflicts, the file watcher, invites
internal/daemon/     the daemon around the sync engine: state, control socket, notifications
internal/server/     the HTTP server: device auth, repositories, git-http-backend, events
internal/config/     XDG config discovery, TOML loading and in-place editing
internal/gitx/       a thin wrapper around the git command
internal/gendocs/    generates the man page, completion scripts and docs/cli-reference.md
packaging/           systemd units, the launchd agent, configs, package scripts
scripts/             release tooling
```

## Building and testing

```sh
go build ./cmd/dropgitbox
go test -race ./...
```

The tests need git 2.28+. The integration tests start real servers and
drive several simulated devices through syncing, merging, conflicts
(including modify/delete), restores, history-rewrite protection, device
revocation, and live syncing through the daemon.

## Documentation

The command definitions in `internal/cli` are the source of every
command's documentation: `--help`, the man page, the shell completion and
[`docs/cli-reference.md`](cli-reference.md). After changing a command, its
flags or its help text, regenerate the reference:

```sh
go generate ./cmd/dropgitbox
```

Tests fail if the reference is stale, if a command lacks a description or
an example, or if a page under `docs/` or the README writes a long flag
with one dash. To see the man page and completion scripts as they will be
released:

```sh
go run ./internal/gendocs /tmp/dgb-docs && man /tmp/dgb-docs/man/dropgitbox.1
```

## Continuous integration

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs on every
push to `main` and on every pull request:

- **lint**: `gofmt`, `go mod tidy`, `go vet`, `shellcheck` on all shell
  scripts, the release scripts' self-test, and
  [govulncheck](https://go.dev/blog/govulncheck) for known vulnerabilities
  in the code paths actually used.
- **test**: `go test -race` on Linux and macOS.
- **package**: `goreleaser check`, then a full snapshot build of every
  release artifact, so packaging problems show up before you tag.

`go.mod` pins a patched Go toolchain (`toolchain go1.26.11`), which the go
command downloads automatically if yours is older. When govulncheck reports
a standard-library issue, raise that line to the fixed version.

## Releasing

Versions follow [semantic versioning](https://semver.org):
`vMAJOR.MINOR.PATCH`, and `vX.Y.Z-rc.N` for prereleases. Cut a release
with [`scripts/release.sh`](../scripts/release.sh):

```sh
git push origin main          # wait for CI to pass
scripts/release.sh            # or: patch | minor | major | 1.4.0 | minor --pre rc
```

The script:

1. Checks you're on an up-to-date, clean `main`.
2. Picks the next version. Without an argument it suggests one from the
   commits since the last release:
   - **major** for a breaking change: `feat!:` style, or `BREAKING CHANGE`
     in a commit body. Before 1.0.0 this becomes **minor**, as semver
     allows for 0.x.
   - **minor** if anything was added.
   - **patch** otherwise.

   `--pre rc` makes `v1.3.0-rc.1`, then `rc.2` on the next run.
3. Drafts release notes from those commits, grouped as Breaking changes,
   New, Fixes, Changes and Documentation, each with its commit hash and a
   full-changelog link. Commits starting with "Add …", "Fix …" and so on
   are grouped automatically, as are Conventional Commit prefixes
   (`feat:`, `fix:`, …). Maintenance commits (`chore:`, `ci:`, `test:`,
   `refactor:`) are left out.
4. Opens the draft in `$VISUAL` or `$EDITOR`. Reword it, add a summary,
   or reorder it: **what you save is the release text on GitHub**. Save an
   empty file to abort. An aborted draft is kept in
   `.git/RELEASE_NOTES_<version>.md` and reused next time.
5. Creates an **annotated** tag whose message holds the notes, and checks
   it with [`scripts/check-release-tag.sh`](../scripts/check-release-tag.sh).
6. Asks, then pushes the tag.

Useful options: `--dry-run` (show the version and the draft, change
nothing), `--no-edit`, `--no-push`, `-y`.

Pushing a `v*` tag starts
[`.github/workflows/release.yml`](../.github/workflows/release.yml):

1. **validate** runs `check-release-tag.sh` again, so a tag pushed by hand
   gets the same checks. The tag must be strict semver, annotated, with
   notes, on `main`, and higher than every earlier tag.
2. **ci** runs the whole CI workflow above against the tagged commit.
3. **publish** runs GoReleaser. It generates the man page and completion
   scripts, builds the archives, `.deb` and `.rpm` packages and checksums,
   and creates the GitHub release named `dropgitbox vX.Y.Z`. The tag's
   notes are the body, with install instructions appended. `-rc` and other
   prerelease versions are marked as prereleases.

Nothing is published unless all three pass. If a release goes wrong,
delete the GitHub release and the tag (`git push origin :refs/tags/vX.Y.Z`
and `git tag -d vX.Y.Z`), fix the problem, and release again. Use a new
patch version if anyone may already have downloaded it.

To try the packaging locally, run `goreleaser release --snapshot --clean`.
The output goes to `dist/`.
