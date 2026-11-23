#!/usr/bin/env bash
#
# Release monetarium-node (every Go module in this repo), then monetarium-wallet
# and monetarium-ctl pinned to the new node version.
#
# Usage: ./release.sh vX.Y.Z "release notes" [--yes]
#
# Steps:
#   node:    bump all internal requires + internal/version to vX.Y.Z, build,
#            test, commit, tag every submodule, push main and the submodule
#            tags in one atomic push, then `go mod tidy` against those tags
#            and commit; tag the root on that commit, check that the tag
#            builds from a clean checkout, push main and the root tag on its
#            own, verify `go install` from outside the workspace.
#   wallet,  bump monetarium-node requires, tidy, set version constants, build
#   ctl:     with GOWORK=off, commit, tag, push.
#
# Pushing a root vX.Y.Z tag triggers each repo's release.yml, which builds and
# publishes the binaries.
#
# Rerunnable: steps that already happened (commit made, tags exist, tags on
# remote) are detected and skipped, so after a failure just run it again.
#
# Environment:
#   WALLET_DIR  path to monetarium-wallet (default: ../monetarium-wallet)
#   CTL_DIR     path to monetarium-ctl    (default: ../monetarium-ctl)
#   SKIP_TESTS  set to 1 to skip the node test run
#   GH_HTTPS    set to 1 to reach GitHub over HTTPS with `gh` credentials
#               instead of the SSH remote

set -euo pipefail

usage() {
	echo "usage: $0 vX.Y.Z \"release notes\" [--yes]" >&2
	exit 1
}

VERSION="${1:-}"
NOTES="${2:-}"
YES=0
[ "${3:-}" = "--yes" ] && YES=1
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || usage
[ -n "$NOTES" ] || usage

NODE_DIR="$(cd "$(dirname "$0")" && pwd)"
WALLET_DIR="$(cd "${WALLET_DIR:-$NODE_DIR/../monetarium-wallet}" && pwd)"
CTL_DIR="$(cd "${CTL_DIR:-$NODE_DIR/../monetarium-ctl}" && pwd)"

NODE_MOD=github.com/monetarium/monetarium-node
NODE_BUMP_MSG="chore: bump internal deps to $VERSION"
NODE_TIDY_MSG="chore: refresh go.sum for $VERSION"
DOWNSTREAM_MSG="chore: bump monetarium-node deps to $VERSION"
TAG_MSG="$VERSION: $NOTES"

GIT=(git)
if [ "${GH_HTTPS:-}" = 1 ]; then
	GIT=(git -c 'url.https://github.com/.insteadOf=git@github.com:'
		-c credential.helper= -c 'credential.helper=!gh auth git-credential')
fi

# Resolve monetarium modules straight from GitHub, bypassing the workspace and
# the module proxy/sumdb caches, so tags pushed seconds ago are visible.
GO_DIRECT=(env GOWORK=off GOPRIVATE=github.com/monetarium)

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nerror: %s\n' "$*" >&2; exit 1; }

confirm() {
	[ "$YES" = 1 ] && return
	local reply
	read -r -p "$1 [y/N] " reply
	[[ "$reply" =~ ^[Yy]$ ]] || die "aborted"
}

version_gt() { # a > b
	[ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]
}

latest_tag() {
	git -C "$1" tag -l 'v*' | { grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; } | sort -V | tail -1
}

# Number of tags named $VERSION or <module>/$VERSION on the remote.
remote_tag_count() {
	local refs
	refs=$("${GIT[@]}" -C "$1" ls-remote --tags --refs origin) || exit 1
	grep -cE "refs/tags/(.+/)?${VERSION//./\\.}\$" <<<"$refs" || true
}

# Module directories in the node repo, root as ".".
node_modules() {
	(cd "$NODE_DIR" && find . -name go.mod -not -path './vendor/*' -exec dirname {} \; |
		sed 's|^\./||' | sort)
}

node_module_dir() {
	[ "$1" = "." ] && echo "$NODE_DIR" || echo "$NODE_DIR/$1"
}

# Write a go.work resolving every node module at $VERSION to this checkout.
# A plain workspace still fetches go.mod for each required version, and those
# don't exist until the tags are pushed.
write_release_gowork() {
	local mod path
	{
		awk '/^go /{print; exit}' "$NODE_DIR/go.mod"
		echo "use ("
		while read -r mod; do
			case "$mod" in */_*) continue ;; esac
			echo "	$(node_module_dir "$mod")"
		done <<<"$(node_modules)"
		echo ")"
		echo "replace ("
		while read -r mod; do
			case "$mod" in */_*) continue ;; esac
			path=$(awk '/^module /{print $2; exit}' "$(node_module_dir "$mod")/go.mod")
			echo "	$path $VERSION => $(node_module_dir "$mod")"
		done <<<"$(node_modules)"
		echo ")"
	} >"$1"
}

node_tag_name() {
	[ "$1" = "." ] && echo "$VERSION" || echo "$1/$VERSION"
}

# Point every monetarium-node require in the given go.mod files at $VERSION.
bump_requires() {
	perl -pi -e 's{(\Q'"$NODE_MOD"'\E(?:/[\w./-]+)?) v\d+\.\d+\.\d+(?=\s|$)}{$1 '"$VERSION"'}g' "$@"
}

# Create an annotated tag at HEAD unless it already exists there.
tag_commit() {
	local dir=$1 tag=$2 commit at
	commit=$(git -C "$dir" rev-parse "HEAD^{commit}")
	if at=$(git -C "$dir" rev-parse -q --verify "refs/tags/$tag^{commit}"); then
		[ "$at" = "$commit" ] ||
			die "$dir: local tag $tag exists at another commit; delete it (git tag -d $tag) and rerun"
		return
	fi
	git -C "$dir" tag -a "$tag" -m "$TAG_MSG" "$commit"
}

remote_has_tag() {
	local refs
	refs=$("${GIT[@]}" -C "$1" ls-remote --tags --refs origin "refs/tags/$2") || exit 1
	[ -n "$refs" ]
}

# Make sure the repo is on a clean main that matches origin, apart from commits
# this script made for $VERSION on a previous run.
sync_repo() {
	local dir=$1 branch subject
	branch=$(git -C "$dir" rev-parse --abbrev-ref HEAD)
	[ "$branch" = main ] || die "$dir: on branch $branch, expected main"
	[ -z "$(git -C "$dir" status --porcelain --untracked-files=no)" ] ||
		die "$dir: uncommitted changes"
	"${GIT[@]}" -C "$dir" fetch --quiet --tags origin main
	git -C "$dir" merge --ff-only --quiet origin/main ||
		die "$dir: main has diverged from origin/main"
	while read -r subject; do
		case "$subject" in
		"" | "$NODE_BUMP_MSG" | "$NODE_TIDY_MSG" | "$DOWNSTREAM_MSG") ;;
		*) die "$dir: unpushed commit \"$subject\"; push or drop it first" ;;
		esac
	done <<<"$(git -C "$dir" log --format=%s origin/main..HEAD)"
}

preflight() {
	local dir latest
	for dir in "$NODE_DIR" "$WALLET_DIR" "$CTL_DIR"; do
		log "$(basename "$dir"): syncing with origin"
		sync_repo "$dir"
		latest=$(latest_tag "$dir")
		if version_gt "$latest" "$VERSION"; then
			die "$(basename "$dir") is already at $latest, newer than $VERSION"
		fi
	done

	local expected count
	expected=$(node_modules | wc -l | tr -d ' ')
	count=$(remote_tag_count "$NODE_DIR")
	# expected-1 without the root tag: a previous run pushed the submodule
	# tags but not the root tag yet, which is resumable.
	if [ "$count" -ne 0 ] && [ "$count" -ne "$expected" ] &&
		! { [ "$count" -eq $((expected - 1)) ] && ! remote_has_tag "$NODE_DIR" "$VERSION"; }; then
		die "monetarium-node has $count of $expected $VERSION tags on origin (partial release); pick a newer version"
	fi
	if [ "$count" -eq 0 ]; then
		for dir in "$WALLET_DIR" "$CTL_DIR"; do
			count=$(remote_tag_count "$dir")
			[ "$count" -eq 0 ] ||
				die "$(basename "$dir") already has $VERSION but node does not; pick a newer version"
		done
	fi
}

# Push the root tag on its own: GitHub creates no push events (so release.yml
# never runs) when more than three tags are pushed at once.
push_node_root_tag() {
	local count
	log "node: pushing $VERSION"
	"${GIT[@]}" -C "$NODE_DIR" push origin "refs/tags/$VERSION"
	count=$(remote_tag_count "$NODE_DIR")
	[ "$count" -eq "$1" ] ||
		die "node: expected $1 $VERSION tags on origin, found $count"
}

# Build the root module from a clean export of the local $VERSION tag outside
# any workspace, the way a checkout of the release tag builds. `go install
# $NODE_MOD@$VERSION` passes even when the tag's go.sum is stale.
check_root_tag_builds() {
	local tmp
	tmp=$(mktemp -d)
	log "node: building $VERSION from a clean checkout of the tag"
	if ! (git -C "$NODE_DIR" archive "$VERSION" | tar -x -C "$tmp" &&
		cd "$tmp" && "${GO_DIRECT[@]}" GOFLAGS=-mod=readonly go build ./...); then
		rm -rf "$tmp"
		die "node: $VERSION does not build from a clean checkout of the tag"
	fi
	rm -rf "$tmp"
}

release_node() {
	local expected count mod tags=()
	expected=$(node_modules | wc -l | tr -d ' ')
	count=$(remote_tag_count "$NODE_DIR")
	cd "$NODE_DIR"

	if [ "$count" -eq "$expected" ]; then
		log "node: $VERSION already tagged on origin, skipping to verification"
	else
		if [ "$count" -eq $((expected - 1)) ]; then
			log "node: submodule tags already on origin"
		else
			if [ "$(git log -1 --format=%s)" != "$NODE_BUMP_MSG" ]; then
				log "node: bumping internal deps and version to $VERSION"
				# shellcheck disable=SC2046
				bump_requires $(find . -name go.mod -not -path './vendor/*')
				perl -pi -e 's/^(\s*Version\s*=\s*)"[^"]*"/$1"'"${VERSION#v}"'"/' internal/version/version.go
				grep -q "Version = \"${VERSION#v}\"" internal/version/version.go ||
					die "node: failed to set Version in internal/version/version.go"

				local gowork
				gowork=$(mktemp -d)/go.work
				write_release_gowork "$gowork"
				log "node: build"
				GOWORK="$gowork" go build ./...
				if [ "${SKIP_TESTS:-}" != 1 ]; then
					log "node: tests"
					GOWORK="$gowork" go test ./chaincfg/... ./internal/blockchain/... \
						./internal/mempool/... ./cointype/...
				fi
				rm -rf "$(dirname "$gowork")"
				git commit --quiet -am "$NODE_BUMP_MSG"
			fi

			while read -r mod; do
				[ "$mod" = . ] && continue
				tag_commit "$NODE_DIR" "$(node_tag_name "$mod")"
				tags+=("refs/tags/$(node_tag_name "$mod")")
			done <<<"$(node_modules)"

			confirm "Push node main + ${#tags[@]} submodule $VERSION tags to origin?"
			log "node: pushing main and ${#tags[@]} submodule tags"
			"${GIT[@]}" push --atomic origin main "${tags[@]}"
		fi

		# The root tag goes on the commit that fills go.sum, and filling it
		# needs the submodule tags on origin. Nothing but that commit may sit
		# between them, or the root would ship code the submodule tags lack.
		git merge-base --is-ancestor "$(node_tag_name chaincfg)" HEAD ||
			die "node: origin/main does not contain the $VERSION submodule tags; pick a newer version"
		local extra
		extra=$(git log --format=%s "$(node_tag_name chaincfg)..HEAD" | grep -vxF "$NODE_TIDY_MSG" || true)
		[ -z "$extra" ] ||
			die "node: main has commits after the $VERSION submodule tags; pick a newer version"
		log "node: refreshing go.sum against the published submodule tags"
		while read -r mod; do
			case "$mod" in */_*) continue ;; esac
			if ! (cd "$NODE_DIR/$mod" && "${GO_DIRECT[@]}" go mod tidy); then
				git checkout --quiet -- .
				die "node: go mod tidy failed in $mod; fix the cause and rerun"
			fi
		done <<<"$(node_modules)"
		if ! git diff --quiet; then
			git commit --quiet -am "$NODE_TIDY_MSG"
		fi
		tag_commit "$NODE_DIR" "$VERSION"
		check_root_tag_builds

		confirm "Push node main + the root $VERSION tag to origin?"
		log "node: pushing main"
		"${GIT[@]}" push origin main
		push_node_root_tag "$expected"
	fi

	log "node: verifying go install $NODE_MOD@$VERSION outside the workspace"
	local tmp
	tmp=$(mktemp -d)
	(cd "$tmp" && "${GO_DIRECT[@]}" GOBIN="$tmp/bin" go install "$NODE_MOD@$VERSION")
	rm -rf "$tmp"
}

# Set the app version in wallet (Major/Minor/Patch consts) or ctl (Version string).
set_app_version() {
	local dir=$1 major minor patch
	IFS=. read -r major minor patch <<<"${VERSION#v}"
	if [ -f "$dir/version/version.go" ]; then
		perl -pi -e '
			s/^(\s*Major\s*=\s*)\d+/${1}'"$major"'/;
			s/^(\s*Minor\s*=\s*)\d+/${1}'"$minor"'/;
			s/^(\s*Patch\s*=\s*)\d+/${1}'"$patch"'/;
			s/^(var PreRelease\s*=\s*)"[^"]*"/$1""/;
		' "$dir/version/version.go"
		grep -qE "^\s*Patch\s*=\s*$patch\$" "$dir/version/version.go" ||
			die "$dir: failed to set version in version/version.go"
		echo version/version.go
	else
		perl -pi -e 's/^(\s*Version\s*=\s*)"[^"]*"/$1"'"${VERSION#v}"'"/' "$dir/version.go"
		grep -q "Version = \"${VERSION#v}\"" "$dir/version.go" ||
			die "$dir: failed to set Version in version.go"
		echo version.go
	fi
}

release_downstream() {
	local dir=$1 name version_file count
	name=$(basename "$dir")
	count=$(remote_tag_count "$dir")
	if [ "$count" -ne 0 ]; then
		log "$name: $VERSION already on origin, skipping"
		return
	fi
	cd "$dir"

	if [ "$(git log -1 --format=%s)" != "$DOWNSTREAM_MSG" ]; then
		log "$name: bumping monetarium-node deps and version to $VERSION"
		bump_requires go.mod
		"${GO_DIRECT[@]}" go mod tidy
		version_file=$(set_app_version "$dir")
		log "$name: build"
		if ! GOWORK=off go build ./...; then
			git checkout --quiet -- .
			die "$name does not build against monetarium-node $VERSION; land the fix on $name main, then rerun (finished node steps are skipped)"
		fi
		git add go.mod go.sum "$version_file"
		git commit --quiet -m "$DOWNSTREAM_MSG"
	fi

	tag_commit "$dir" "$VERSION"
	confirm "Push $name main + $VERSION tag to origin?"
	log "$name: pushing main and $VERSION"
	"${GIT[@]}" push --atomic origin main "refs/tags/$VERSION"
}

preflight
release_node
release_downstream "$WALLET_DIR"
release_downstream "$CTL_DIR"

log "released $VERSION; release.yml is now building binaries in each repo"
