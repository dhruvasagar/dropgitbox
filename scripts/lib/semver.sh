# shellcheck shell=bash
# Semantic version helpers shared by scripts/release.sh and
# scripts/check-release-tag.sh. Source it; works with bash 3.2 (macOS).

# A release tag: vMAJOR.MINOR.PATCH with an optional -PRERELEASE, no leading
# zeros and no build metadata.
SEMVER_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'

is_semver() { [[ $1 =~ $SEMVER_RE ]]; }

is_prerelease() { [[ $1 == *-* ]]; }

# semver_core v1.2.3-rc.1 -> 1.2.3
semver_core() {
	local v=${1#v}
	echo "${v%%-*}"
}

# semver_cmp A B prints -1, 0 or 1, following semver.org precedence.
semver_cmp() {
	local a=${1#v} b=${2#v} ap="" bp="" i
	local ac=${a%%-*} bc=${b%%-*}
	[[ $a == *-* ]] && ap=${a#*-}
	[[ $b == *-* ]] && bp=${b#*-}
	local -a an bn
	IFS=. read -r -a an <<<"$ac"
	IFS=. read -r -a bn <<<"$bc"
	for i in 0 1 2; do
		if ((an[i] != bn[i])); then
			if ((an[i] < bn[i])); then echo -1; else echo 1; fi
			return
		fi
	done
	# Same core: a release sorts after its prereleases.
	if [[ $ap == "$bp" ]]; then echo 0; return; fi
	if [[ -z $ap ]]; then echo 1; return; fi
	if [[ -z $bp ]]; then echo -1; return; fi
	# Both prereleases: compare dot-separated identifiers. Numeric ones
	# compare numerically and sort before alphanumeric ones; a shorter list
	# sorts first when it is a prefix of the longer one.
	local -a ai bi
	IFS=. read -r -a ai <<<"$ap"
	IFS=. read -r -a bi <<<"$bp"
	local n=${#ai[@]}
	((${#bi[@]} > n)) && n=${#bi[@]}
	for ((i = 0; i < n; i++)); do
		if ((i >= ${#ai[@]})); then echo -1; return; fi
		if ((i >= ${#bi[@]})); then echo 1; return; fi
		local x=${ai[i]} y=${bi[i]}
		[[ $x == "$y" ]] && continue
		local xn=0 yn=0
		[[ $x =~ ^[0-9]+$ ]] && xn=1
		[[ $y =~ ^[0-9]+$ ]] && yn=1
		if ((xn && yn)); then
			if ((10#$x < 10#$y)); then echo -1; else echo 1; fi
		elif ((xn)); then
			echo -1
		elif ((yn)); then
			echo 1
		elif [[ $x < $y ]]; then
			echo -1
		else
			echo 1
		fi
		return
	done
	echo 0
}

# latest_tag [--releases] prints the highest semver tag (only final
# releases with --releases), or nothing if there is none.
latest_tag() {
	local only_releases=${1:-} t latest=""
	while read -r t; do
		is_semver "$t" || continue
		[[ $only_releases == --releases ]] && is_prerelease "$t" && continue
		if [[ -z $latest || $(semver_cmp "$t" "$latest") == 1 ]]; then
			latest=$t
		fi
	done < <(git tag --list 'v*')
	echo "$latest"
}

# semver_bump VERSION major|minor|patch prints the next core version,
# e.g. semver_bump v1.4.2 minor -> v1.5.0.
semver_bump() {
	local -a p
	IFS=. read -r -a p <<<"$(semver_core "$1")"
	case $2 in
	major) echo "v$((p[0] + 1)).0.0" ;;
	minor) echo "v${p[0]}.$((p[1] + 1)).0" ;;
	patch) echo "v${p[0]}.${p[1]}.$((p[2] + 1))" ;;
	*) return 1 ;;
	esac
}
