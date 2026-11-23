#!/usr/bin/env bash
# Cuts a release: picks the next version, drafts release notes from the
# commits since the last release, lets you edit them, creates the annotated
# tag and pushes it. Pushing the tag starts the Release workflow, which
# checks the tag, runs CI, and publishes the GitHub release with exactly
# these notes plus install instructions.
#
# Usage: scripts/release.sh [patch|minor|major|VERSION] [options]
#
#   patch | minor | major   bump from the latest release (default: suggested
#                           from the commits, see below)
#   VERSION                 an explicit version, e.g. 1.0.0 or v1.0.0-rc.1
#
#   --pre LABEL   make a prerelease of the bumped version: minor --pre rc
#                 gives v0.3.0-rc.1, then v0.3.0-rc.2 when run again
#   --dry-run     print the version and draft notes; change nothing
#   --no-edit     use the drafted notes without opening an editor
#   --no-push     create the tag locally but don't push it
#   -y, --yes     don't ask before pushing
#
# The suggested bump: major for breaking changes (a "!" after a
# conventional-commit type, or "BREAKING CHANGE" in a commit body), minor
# if anything was added, patch otherwise. Before 1.0.0, breaking changes
# bump the minor version instead, as semver allows for 0.x.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source-path=SCRIPTDIR source=lib/semver.sh
. scripts/lib/semver.sh

project=dropgitbox
branch=main
remote=origin

die() {
	echo "error: $*" >&2
	exit 1
}
say() { echo "==> $*"; }

bump="" version="" pre="" dry=0 edit=1 push=1 yes=0
while (($#)); do
	case $1 in
	patch | minor | major) bump=$1 ;;
	--pre)
		pre=${2:?--pre needs a label, e.g. rc}
		shift
		;;
	--pre=*) pre=${1#--pre=} ;;
	--dry-run) dry=1 ;;
	--no-edit) edit=0 ;;
	--no-push) push=0 ;;
	-y | --yes) yes=1 ;;
	-h | --help)
		sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
		exit 0
		;;
	v[0-9]* | [0-9]*) version=v${1#v} ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
	shift
done
[[ -n $bump && -n $version ]] && die "give either a bump (patch/minor/major) or a version, not both"
[[ -n $pre && -n $version ]] && die "--pre goes with a bump; for an explicit version write it in full, e.g. 1.0.0-rc.1"
[[ -z $pre || $pre =~ ^[0-9A-Za-z-]+$ ]] || die "--pre label must be letters, digits or '-' (e.g. rc, beta)"

# ---- preconditions ---------------------------------------------------------

[[ $(git symbolic-ref --short -q HEAD || true) == "$branch" ]] ||
	die "releases are cut from $branch; check it out first"
[[ -z $(git status --porcelain) ]] ||
	die "the working tree has uncommitted changes; commit or stash them first"

say "Fetching $remote"
git fetch --quiet --tags "$remote" "$branch"
local_head=$(git rev-parse HEAD)
remote_head=$(git rev-parse "$remote/$branch")
if [[ $local_head != "$remote_head" ]]; then
	if git merge-base --is-ancestor "$remote_head" "$local_head"; then
		die "$branch has commits that aren't on $remote yet; push them first (git push $remote $branch), let CI pass, then rerun"
	fi
	die "$branch is behind or has diverged from $remote/$branch; pull first"
fi

# ---- the commits being released ---------------------------------------------

last_release=$(latest_tag --releases)
latest=$(latest_tag)
range=HEAD
[[ -n $last_release ]] && range="$last_release..HEAD"

# Each commit as: hash<TAB>subject<TAB>breaking(0/1)
commits=()
while IFS= read -r -d $'\x1e' rec; do
	rec=${rec#$'\n'}
	[[ -z $rec ]] && continue
	hash=${rec%%$'\x1f'*}
	rest=${rec#*$'\x1f'}
	subject=${rest%%$'\x1f'*}
	body=${rest#*$'\x1f'}
	breaking=0
	[[ $subject =~ ^[a-zA-Z]+(\([^\)]*\))?!: || $body == *"BREAKING CHANGE"* ]] && breaking=1
	commits+=("$hash"$'\t'"$subject"$'\t'"$breaking")
done < <(git log --no-merges --reverse --format='%h%x1f%s%x1f%b%x1e' "$range")

((${#commits[@]})) || die "no commits since ${last_release:-the beginning}; nothing to release"

# Sort commits into sections by their subject.
classify() {
	local s=$1 lower
	lower=$(printf '%s' "$s" | tr '[:upper:]' '[:lower:]')
	case $lower in
	chore* | ci:* | ci\(* | build:* | build\(* | test:* | test\(* | tests:* | style:* | refactor:* | refactor\(* | "release "* | "bump version"*) echo skip ;;
	feat* | add* | implement* | introduce* | support* | allow* | new\ *) echo new ;;
	fix* | correct* | handle* | prevent* | avoid* | stop\ * | repair* | resolve*) echo fix ;;
	docs* | doc:* | document* | readme*) echo docs ;;
	perf* | speed* | faster*) echo change ;;
	*) echo change ;;
	esac
}

# Strip a conventional-commit prefix: "feat(cli)!: add x" -> "Add x"
tidy() {
	local s=$1
	if [[ $s =~ ^[a-zA-Z]+(\([^\)]*\))?!?:[[:space:]]*(.*)$ ]]; then
		s=${BASH_REMATCH[2]}
	fi
	printf '%s' "$(printf '%s' "${s:0:1}" | tr '[:lower:]' '[:upper:]')${s:1}"
}

new=() fixes=() docs=() changes=() breaking=() skipped=0
for c in "${commits[@]}"; do
	IFS=$'\t' read -r hash subject brk <<<"$c"
	kind=$(classify "$subject")
	line="- $(tidy "$subject") ($hash)"
	if ((brk)); then
		breaking+=("$line")
		continue
	fi
	case $kind in
	skip) skipped=$((skipped + 1)) ;;
	new) new+=("$line") ;;
	fix) fixes+=("$line") ;;
	docs) docs+=("$line") ;;
	*) changes+=("$line") ;;
	esac
done

# ---- the version --------------------------------------------------------------

base=${last_release:-v0.0.0}
if [[ -z $version ]]; then
	if [[ -z $bump ]]; then
		if ((${#breaking[@]})); then
			bump="major"
		elif ((${#new[@]})); then
			bump="minor"
		else
			bump="patch"
		fi
		# 0.x: breaking changes only bump the minor version.
		[[ $bump == major && $(semver_core "$base") == 0.* ]] && bump="minor"
		suggested=" (suggested from the commits; pass patch, minor or major to override)"
	fi
	version=$(semver_bump "$base" "$bump")
	if [[ -n $pre ]]; then
		n=0
		while read -r t; do
			[[ $t =~ ^$version-$pre\.([0-9]+)$ ]] && ((BASH_REMATCH[1] > n)) && n=${BASH_REMATCH[1]}
		done < <(git tag --list "$version-$pre.*")
		version="$version-$pre.$((n + 1))"
	fi
fi

is_semver "$version" || die "$version is not a valid version like v1.2.3 or v1.2.3-rc.1"
git rev-parse -q --verify "refs/tags/$version" >/dev/null && die "tag $version already exists"
if [[ -n $latest && $(semver_cmp "$version" "$latest") != 1 ]]; then
	die "$version is not higher than the latest tag $latest"
fi

# ---- the release notes -------------------------------------------------------

repo_url=$(git remote get-url "$remote" | sed -E 's#^git@([^:]+):#https://\1/#; s#^ssh://git@([^/]+)/#https://\1/#; s#\.git$##')

notes_file=$(git rev-parse --git-dir)/RELEASE_NOTES_$version.md
section() {
	local title=$1
	shift
	(($#)) || return 0
	printf '## %s\n\n' "$title"
	printf '%s\n' "$@"
	echo
}
draft() {
	echo "<!-- Release notes for $project $version. Lines like this one are removed. -->"
	echo "<!-- Everything else becomes the GitHub release text (Markdown). Reword, regroup, -->"
	echo "<!-- add a summary at the top, or delete lines. Save an empty file to abort. -->"
	echo
	if is_prerelease "$version"; then
		echo "This is a prerelease for testing; it may change before ${version%%-*}."
		echo
	fi
	# set -u and empty arrays don't mix in bash 3.2, hence the ${a[@]+...} form.
	section "Breaking changes" ${breaking[@]+"${breaking[@]}"}
	section "New" ${new[@]+"${new[@]}"}
	section "Fixes" ${fixes[@]+"${fixes[@]}"}
	section "Changes" ${changes[@]+"${changes[@]}"}
	section "Documentation" ${docs[@]+"${docs[@]}"}
	if ((skipped)); then
		echo "<!-- $skipped maintenance commit(s) (chore, ci, test, refactor...) left out. -->"
		echo
	fi
	if [[ -n $last_release ]]; then
		echo "**Full changelog**: $repo_url/compare/$last_release...$version"
	else
		echo "**Full changelog**: $repo_url/commits/$version"
	fi
}

say "Releasing $version${suggested:-}"
say "Changes since ${last_release:-the first commit}: ${#commits[@]} commit(s)"

if ((dry)); then
	echo
	draft
	echo
	say "Dry run: no tag created."
	exit 0
fi

if [[ -s $notes_file ]]; then
	say "Reusing your earlier draft: $notes_file (delete it to start over)"
else
	draft >"$notes_file"
fi
if ((edit)); then
	"${VISUAL:-${EDITOR:-vi}}" "$notes_file"
fi

# Drop the instruction comments and surrounding blank lines.
body=$(grep -v -E '^[[:space:]]*<!--.*-->[[:space:]]*$' "$notes_file" | cat -s | sed -e '/./,$!d' || true)
body=$(printf '%s\n' "$body" | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')
[[ -n ${body//[[:space:]]/} ]] || die "the release notes are empty; aborting (no tag created)"

echo
echo "----- release notes for $version -----"
printf '%s\n' "$body"
echo "--------------------------------------"
echo

# The tag message is "<project> <version>", a blank line, then the notes.
# --cleanup=verbatim keeps Markdown headings, which git would otherwise
# strip as comments.
msg_file=$(mktemp)
trap 'rm -f "$msg_file"' EXIT
printf '%s %s\n\n%s\n' "$project" "$version" "$body" >"$msg_file"
git tag --annotate --cleanup=verbatim --file "$msg_file" "$version"
say "Created tag $version"

if ! scripts/check-release-tag.sh "$version"; then
	git tag -d "$version" >/dev/null
	die "the tag failed its checks and was deleted (your notes are kept in $notes_file)"
fi
# The tag holds the notes now; the draft is no longer needed.
rm -f "$notes_file"

if ((!push)); then
	say "Not pushed. When ready: git push $remote $version   (undo: git tag -d $version)"
	exit 0
fi
if ((!yes)); then
	read -r -p "Push $version to $remote and start the release? [y/N] " answer
	if [[ ! $answer =~ ^[Yy] ]]; then
		say "Not pushed. Later: git push $remote $version   (undo: git tag -d $version)"
		exit 0
	fi
fi
git push --quiet "$remote" "refs/tags/$version"
say "Pushed. The Release workflow is publishing it: $repo_url/actions/workflows/release.yml"
say "Release page (once published): $repo_url/releases/tag/$version"
