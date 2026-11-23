#!/usr/bin/env bash
# Checks that a tag is fit to release:
#   - a strict semantic version: vMAJOR.MINOR.PATCH, optionally -PRERELEASE
#     (e.g. v1.2.0, v1.2.0-rc.1); no leading zeros, no build metadata
#   - an annotated tag (its message is the release notes)
#   - points at a commit on the release branch (default origin/main)
#   - higher than every earlier release tag
#
# Usage: scripts/check-release-tag.sh TAG
#        scripts/check-release-tag.sh --selftest
# scripts/release.sh runs it before pushing; the Release workflow runs it
# again before publishing anything.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib/semver.sh
. "$(dirname "$0")/lib/semver.sh"

fail() {
	echo "error: $*" >&2
	exit 1
}

if [[ ${1:-} == --selftest ]]; then
	check() {
		local got
		got=$(semver_cmp "$1" "$2")
		[[ $got == "$3" ]] || fail "semver_cmp $1 $2 = $got, want $3"
	}
	check v1.0.0 v1.0.0 0
	check v1.0.0 v1.0.1 -1
	check v1.10.0 v1.9.0 1
	check v2.0.0 v1.99.99 1
	check v1.0.0-rc.1 v1.0.0 -1
	check v1.0.0 v1.0.0-rc.1 1
	check v1.0.0-alpha v1.0.0-alpha.1 -1
	check v1.0.0-alpha.1 v1.0.0-alpha.beta -1
	check v1.0.0-beta.2 v1.0.0-beta.11 -1
	check v1.0.0-rc.1 v1.0.0-beta.11 1
	check v1.0.0-rc-1 v1.0.0-rc-2 -1
	for t in v1.2.3 v0.1.0 v1.0.0-rc.1 v1.0.0-alpha-1.x; do
		is_semver "$t" || fail "$t should be valid"
	done
	for t in 1.2.3 v1.2 v01.2.3 v1.2.3+build v1.2.3- v1.2.3-rc..1 v1.2.3.4; do
		if is_semver "$t"; then fail "$t should be invalid"; fi
	done
	[[ $(semver_bump v1.4.2 major) == v2.0.0 ]] || fail "bump major"
	[[ $(semver_bump v1.4.2 minor) == v1.5.0 ]] || fail "bump minor"
	[[ $(semver_bump v1.4.2-rc.1 patch) == v1.4.3 ]] || fail "bump patch"
	echo "selftest ok"
	exit 0
fi

tag=${1:?usage: $0 TAG   (e.g. v0.2.0)}
branch=${RELEASE_BRANCH:-origin/main}

is_semver "$tag" ||
	fail "$tag is not a semantic version tag like v1.2.3 or v1.2.3-rc.1 (see https://semver.org)"

git rev-parse -q --verify "refs/tags/$tag" >/dev/null ||
	fail "tag $tag does not exist in this repository"

[[ $(git cat-file -t "refs/tags/$tag") == tag ]] ||
	fail "$tag is a lightweight tag; its message is the release notes, so it must be annotated.
Recreate it with: git tag -d $tag && scripts/release.sh ${tag#v}"

git rev-parse -q --verify "$branch" >/dev/null ||
	fail "$branch not found; fetch it first (git fetch origin), or set RELEASE_BRANCH"
git merge-base --is-ancestor "$tag^{commit}" "$branch" ||
	fail "$tag points at a commit that is not on $branch; release from $branch only"

latest=""
while read -r t; do
	[[ $t == "$tag" ]] && continue
	is_semver "$t" || continue
	if [[ -z $latest || $(semver_cmp "$t" "$latest") == 1 ]]; then
		latest=$t
	fi
done < <(git tag --list 'v*')

if [[ -n $latest && $(semver_cmp "$tag" "$latest") != 1 ]]; then
	fail "$tag is not higher than the latest release tag $latest"
fi

[[ -n $(git tag --list --format='%(contents:body)' "$tag" | tr -d '[:space:]') ]] ||
	fail "$tag has no release notes: its message has only a subject line"

kind=release
is_prerelease "$tag" && kind=prerelease
echo "ok: $tag is a valid $kind${latest:+ (previous: $latest)}"
