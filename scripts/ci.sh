#!/usr/bin/env bash
# Local CI for ai-manager. GitHub Actions is not used.
#
#   scripts/ci.sh <pr-number>   merge the PR's head commit into the current tip
#                               of its base branch in a temporary worktree, run
#                               `make check` there, and post the result to the
#                               head commit as the `local-ci` status
#   scripts/ci.sh               run `make check` in the current checkout only;
#                               nothing is posted
#
# A PR run uses a clean checkout of exactly the commits GitHub has, so files
# lying around in a local worktree can't make it pass. Testing the merge with
# the latest base (not the PR alone) catches two PRs that pass separately but
# break the base together.
#
# CI runs PR code on this Mac, so the maintainer reviews the diff first, and the
# script narrows what that code can reach:
#   - it refuses PRs from forks, and PRs whose author isn't in AIM_CI_AUTHORS
#     (default: the gh account in use);
#   - `make check` runs under `env -i` with a throwaway HOME, TMPDIR and
#     CLAUDE_CONFIG_DIR, so no tokens from the environment and no gh or
#     Claude Code login config reach it. Go's build and module caches are
#     shared so builds stay fast, and GOTOOLCHAIN=auto lets go.mod's
#     toolchain line pick the Go version (downloaded once into the module
#     cache). This is not a sandbox: the code still runs
#     as this user.
#   - the status is posted by this outer script, never by the build.
set -euo pipefail

usage() { echo "usage: scripts/ci.sh [pr-number]" >&2; exit 2; }

cd "$(git rev-parse --show-toplevel)"

if ! command -v claude >/dev/null 2>&1; then
  echo "ci: claude is not on PATH, and the plugin checks are part of CI" >&2
  exit 2
fi

if [ $# -eq 0 ]; then
  exec make check
fi
[ $# -eq 1 ] && [[ $1 =~ ^[0-9]+$ ]] || usage
pr=$1

nwo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
read -r sha base author cross < <(gh pr view "$pr" \
  --json headRefOid,baseRefName,author,isCrossRepository \
  --jq '"\(.headRefOid) \(.baseRefName) \(.author.login) \(.isCrossRepository)"')
allowed=${AIM_CI_AUTHORS:-$(gh api user --jq .login)}
if [ "$cross" != false ]; then
  echo "ci: PR #$pr comes from a fork; local CI only runs this repo's own branches" >&2
  exit 2
fi
case " $allowed " in
  *" $author "*) ;;
  *) echo "ci: PR #$pr is by $author, who isn't in AIM_CI_AUTHORS ($allowed)" >&2; exit 2 ;;
esac
git fetch -q origin "$base" "pull/$pr/head"
if ! git cat-file -e "$sha^{commit}" 2>/dev/null; then
  echo "ci: PR #$pr moved while fetching; run it again" >&2
  exit 2
fi

tmp=$(mktemp -d -t aim-ci)
log="$tmp.log"
home="$tmp.home"
mkdir -p "$home/claude"
posted=0

post() {
  gh api -X POST "repos/$nwo/statuses/$sha" \
    -f state="$1" -f context=local-ci -f description="$2" >/dev/null
}

cleanup() {
  # If the run ended without a verdict (Ctrl-C, an error above), don't leave
  # the status pending.
  if [ "$posted" = 0 ]; then post error "local CI did not finish" || true; fi
  # The temporary worktree is ours alone, so forcing its removal is safe.
  git worktree remove --force "$tmp" >/dev/null 2>&1 || true
  rm -rf "$tmp" "$home"
}
trap cleanup EXIT

git worktree add -q --detach "$tmp" "origin/$base"
post pending "make check running on the maintainer's Mac"
if ! git -C "$tmp" -c user.name=local-ci -c user.email=local-ci@invalid \
    merge -q --no-edit "$sha" >"$log" 2>&1; then
  git -C "$tmp" merge --abort >/dev/null 2>&1 || true
  post failure "conflicts with $base: rebase the PR"
  posted=1
  echo "ci: PR #$pr ${sha:0:7} conflicts with origin/$base; rebase it" >&2
  exit 1
fi
base_sha=$(git rev-parse --short "origin/$base")

# The PATH the build gets: the folders holding the tools it needs, plus the
# system ones.
path=""
for tool in go claude git make; do
  dir=$(dirname "$(command -v "$tool")")
  case ":$path:" in *":$dir:"*) ;; *) path="$path:$dir" ;; esac
done
path="${path#:}:/usr/bin:/bin:/usr/sbin:/sbin"

start=$(date +%s)
if env -i PATH="$path" HOME="$home" TMPDIR="$home" LANG=C.UTF-8 \
    GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" \
    GOPATH="$(go env GOPATH)" GOTOOLCHAIN=auto \
    CLAUDE_CONFIG_DIR="$home/claude" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    make -C "$tmp" check >"$log" 2>&1; then
  secs=$(( $(date +%s) - start ))
  post success "make check passed in ${secs}s (merged with $base@$base_sha)"
  posted=1
  echo "ci: PR #$pr ${sha:0:7} merged with $base@$base_sha passed in ${secs}s (log: $log)"
else
  secs=$(( $(date +%s) - start ))
  post failure "make check failed after ${secs}s (merged with $base@$base_sha)"
  posted=1
  tail -n 40 "$log"
  echo "ci: PR #$pr ${sha:0:7} FAILED (full log: $log)" >&2
  exit 1
fi
