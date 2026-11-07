#!/usr/bin/env bash
# Phi's fork: rebase `custom` onto the latest upstream release, test, build the
# image, hand it over for deployment and push `custom` to the fork.
#
# Usage: scripts/custom-update.sh [--no-rebase] [--no-push] [tag]
#   --no-rebase  Build the current checkout as is (uncommitted changes allowed;
#                the image tag then ends in -dirty).
#   --no-push    Don't push `custom` to origin.
#   tag          Upstream release to rebase onto (default: the newest v* tag).
#
# Rebase conflicts that git rerere has seen before resolve on their own.
# When /srv/cpa-images is writable (zbook, user t3), the image is also saved
# there as an archive for user phi to `podman load`; the newest three are kept.
#
# Everything runs inside main(): bash reads the script as it goes, and the rebase
# can rewrite this file.
set -euo pipefail

main() {
  local rebase=1 push=1 tag="" arg
  for arg in "$@"; do
    case $arg in
      --no-rebase) rebase=0 ;;
      --no-push) push=0 ;;
      -h | --help) sed -n '2,13p' "$0"; return 0 ;;
      -*) die "Unknown option $arg." ;;
      *) tag=$arg ;;
    esac
  done
  cd "$(git rev-parse --show-toplevel)"

  local engine
  engine=$(command -v podman || command -v docker) || die "Neither podman nor docker is installed."

  if ((rebase)); then
    [[ $(git branch --show-current) == custom ]] || die "Not on branch custom."
    [[ -z $(git status --porcelain) ]] || die "The checkout has uncommitted changes." \
      "Commit them as a 'local:' commit, or build them with --no-rebase."
    git fetch -q upstream --tags
    # sed reads all input; head would close the pipe and fail under pipefail.
    [[ -n $tag ]] || tag=$(git tag -l 'v*' --sort=-v:refname | sed -n 1p)
    git rev-parse -q --verify "$tag^{commit}" >/dev/null || die "Tag $tag not found."
    rebase_onto "$tag"
  fi
  tag=$(git describe --tags --abbrev=0)

  local commit name
  commit=$(git rev-parse --short HEAD)
  [[ -z $(git status --porcelain) ]] || commit+=-dirty
  name=localhost/cli-proxy-api:custom-$tag-$commit

  # Upstream tests that fail at random, also on a clean release (checked on v8.0.59):
  # the test's temp dir is removed while request-log files are still being written.
  local flaky='^TestServerResponsesWebsocketReauthorizesEachTurn$'
  echo "== Tests ($tag + $(git rev-list --count "$tag"..HEAD) local commits)"
  "$engine" run --rm -v "$PWD":/src:Z -w /src -e GOFLAGS=-buildvcs=false \
    -v cpa-gomod:/go/pkg/mod -v cpa-gobuild:/root/.cache/go-build golang:1.26-bookworm \
    sh -c "go test -skip '$flaky' ./internal/reqlogdb/ ./internal/logging/ ./internal/api/... && go build -o /dev/null ./cmd/..."

  echo "== Image $name"
  "$engine" build -t "$name" --build-arg VERSION="$tag-custom" --build-arg COMMIT="$commit" \
    --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" .

  local dir=/srv/cpa-images
  if [[ -w $dir ]]; then
    local file=$dir/cli-proxy-api-custom-$tag-$commit.tar
    "$engine" save -o "$file.part" "$name" && mv "$file.part" "$file"
    chmod 0640 "$file"
    ls -1t "$dir"/cli-proxy-api-custom-*.tar | tail -n +4 | xargs -r rm -f
    echo "Saved $file"
  fi

  if ((push)) && ((rebase)); then
    git push --force-with-lease origin custom
  fi
  echo "Done: $name"
}

rebase_onto() {
  local tag=$1 unresolved
  if git merge-base --is-ancestor "$tag" HEAD; then
    echo "custom is already on $tag."
    return 0
  fi
  echo "== Rebasing custom onto $tag"
  local rr=(-c rerere.enabled=true -c rerere.autoUpdate=true)
  git "${rr[@]}" rebase "$tag" && return 0
  while [[ -e $(git rev-parse --git-path rebase-merge) ]]; do
    unresolved=$(git diff --name-only --diff-filter=U)
    [[ -z $unresolved ]] || die "Rebase conflict that rerere has not seen before, in: ${unresolved//$'\n'/, }." \
      "Fix these files, 'git add' them, run 'git rebase --continue', then run this script again. rerere records the fix for next time."
    echo "rerere resolved the conflict; continuing the rebase."
    GIT_EDITOR=true git "${rr[@]}" rebase --continue && return 0
    [[ -n $(git diff --name-only --diff-filter=U) ]] ||
      die "'git rebase --continue' failed with no conflicted files." "Read the git output above and finish the rebase by hand."
  done
}

die() {
  printf 'custom-update: %s\n' "$@" >&2
  exit 1
}

main "$@"
