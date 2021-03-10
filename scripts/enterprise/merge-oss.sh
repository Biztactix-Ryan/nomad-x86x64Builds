#!/usr/bin/env bash

set -u
set -e

origin_branch="${1:-main}"
dest_branch="${2:-main}"
GIT_TMP_MERGE_BRANCH="${3:-}"

if [ -z "${GIT_TMP_MERGE_BRANCH}" ]; then
    GIT_TMP_MERGE_BRANCH="oss-merge-${origin_branch}-$(date -u +%Y%m%d%H%M%S)"
fi

git pull origin "${dest_branch}"

# Merge OSS main branch to Enterprise merge branch
if ! git remote get-url oss 1>/dev/null 2>/dev/null; then
    git remote add oss https://github.com/hashicorp/nomad.git
fi

git fetch oss "${origin_branch}"

git checkout -b "${GIT_TMP_MERGE_BRANCH}"
latest_oss_commit="$(git rev-parse "oss/${origin_branch}")"
message="Merge Nomad OSS branch '${origin_branch}' at commit ${latest_oss_commit}"

if ! git merge -m "$message" "oss/${origin_branch}"; then
    # try to merge common conflicting files
    git status
    git checkout --theirs CHANGELOG.md
    git checkout --theirs version/version.go
    git checkout --theirs command/agent/bindata_assetfs.go
    git checkout --theirs .circleci/config.yml
    git checkout --ours   vendor/modules.txt
    git checkout --ours   go.sum
    make sync

    # Regenerate enterprise CircleCI config to apply changes from OSS merge

    make -C .circleci config.yml

    git add CHANGELOG.md version/version.go command/agent/bindata_assetfs.go \
        go.sum vendor/modules.txt .circleci/config.yml

    # attempt merging again
    if ! git commit -m "$message"; then
        echo "failed to auto merge" >&2
        exit 1
    fi
fi

if [[ -z "${CI:-}" ]]; then
    echo
    echo "push branch and open a PR"
    echo "   git push origin ${GIT_TMP_MERGE_BRANCH}"
fi

