#!/usr/bin/env bash
# Cuts a gpb release: checks, the version bump in every file that carries it, the Release
# commit, the tag, the GitHub Release and the amd64 image. Write the CHANGELOG.md section for
# the new version first; that is the one uncommitted change this expects to find.
#
#   scripts/release.sh 0.3.4
set -euo pipefail

version="${1:?usage: scripts/release.sh X.Y.Z}"
image="sodre90/gpb"
version_files=(internal/version/version.go docker-compose.yml deploy/quadlet.md deploy/gpb.container)

cd "$(git rev-parse --show-toplevel)"

fail() { echo "release: $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

require_releasable_state() {
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "$version is not X.Y.Z"
  [[ "$(git branch --show-current)" == main ]] || fail "not on main"
  git rev-parse -q --verify "refs/tags/v$version" >/dev/null && fail "v$version is already tagged"
  grep -q "^## $version — " CHANGELOG.md || fail "CHANGELOG.md has no '## $version — <date>' section"
  local dirty
  dirty="$(git status --porcelain | grep -v ' CHANGELOG.md$' || true)"
  [[ -z "$dirty" ]] || fail "uncommitted changes besides CHANGELOG.md:"$'\n'"$dirty"
  command -v govulncheck >/dev/null || fail "govulncheck is not installed"
  command -v gh >/dev/null || fail "gh is not installed"
}

run_quality_gates() {
  go vet ./...
  go test ./...
  (cd spike && go test ./...)
  govulncheck ./...
}

bump_version_files() {
  local current
  current="$(sed -n 's/^const Current = "\(.*\)"$/\1/p' internal/version/version.go)"
  [[ -n "$current" ]] || fail "cannot read the current version from internal/version/version.go"
  local file
  for file in "${version_files[@]}"; do
    grep -q "$current" "$file" || fail "$file does not mention $current; the list of version files is out of date"
    perl -pi -e "s/\Q$current\E/$version/g" "$file"
  done
  echo "$current -> $version in ${version_files[*]}"
}

changelog_section() {
  awk -v heading="^## $version — " '$0 ~ heading {found=1; next} /^## / && found {exit} found' CHANGELOG.md
}

confirm_or_undo_bump() {
  read -r -p "Commit, tag, push main and v$version, publish the GitHub Release and push the image? [y/N] " answer
  if [[ "$answer" != y && "$answer" != Y ]]; then
    git checkout -- "${version_files[@]}"
    fail "stopped; the version bump is undone and CHANGELOG.md is left as it was"
  fi
}

step "Checking the tree"
require_releasable_state

step "Quality gates"
run_quality_gates

step "Bumping the version"
bump_version_files
git --no-pager diff --stat
confirm_or_undo_bump

step "Release commit and tag"
git add CHANGELOG.md "${version_files[@]}"
git commit -m "Release $version"
git tag -a "v$version" -m "gpb $version"

step "Pushing by explicit refspec"
# Never --all, --mirror or --force: the public repo is one root commit on purpose.
git push origin main
git push origin "v$version"

step "GitHub Release"
# A pushed tag is not a Release; this is the step that used to be forgotten.
notes="$(mktemp)"
trap 'rm -f "$notes"' EXIT
changelog_section >"$notes"
gh release create "v$version" --title "gpb $version" --notes-file "$notes" --verify-tag
gh release list --limit 5

step "Image (linux/amd64: Google ships Chrome for Linux on that architecture only)"
docker buildx build --platform linux/amd64 -t "$image:$version" -t "$image:latest" --push .
docker buildx imagetools inspect "$image:$version" | grep -q 'linux/amd64' \
  || fail "$image:$version was pushed without a linux/amd64 manifest"

echo
echo "Released gpb $version. The box still runs the previous image until its Image= line moves."
