#!/bin/sh
# Start `summa42-box serve` for one GitHub repository from environment
# variables. deploy/systemd/summa42-serve.service runs this with the variables
# from /etc/summa42/serve.env; docs/operations/running-serve.md explains each.
#
# The pipeline works on one pinned commit. This script pins the commit the
# base branch has in the local source clone at the moment it starts, so after a
# merge: update the clone, then restart the service.
set -eu

need() {
    eval "value=\${$1:-}"
    if [ -z "$value" ]; then
        echo "summa42-serve: $1 is not set (see docs/operations/running-serve.md)" >&2
        exit 64
    fi
}
for name in SUMMA42_MISSION SUMMA42_ENVELOPE SUMMA42_GITHUB_REPOSITORY SUMMA42_MAINTAINER \
    SUMMA42_SOURCE_REPO SUMMA42_BASE_BRANCH SUMMA42_WORKSPACE_ROOT SUMMA42_GITHUB_TOKEN_FILE \
    SUMMA42_MODEL_USER SUMMA42_VALIDATION_USER SUMMA42_ALLOWED_PREFIX SUMMA42_VALIDATION_COMMAND_JSON \
    SUMMA42_PRODUCER_PROFILE SUMMA42_PRODUCER_MODEL SUMMA42_REVIEWER_PROFILE SUMMA42_REVIEWER_MODEL; do
    need "$name"
done

box="${SUMMA42_BOX_BIN:-summa42-box}"
git_bin="${SUMMA42_GIT_BIN:-git}"
sha="${SUMMA42_SOURCE_SHA:-$("$git_bin" -C "$SUMMA42_SOURCE_REPO" rev-parse --verify "${SUMMA42_BASE_BRANCH}^{commit}")}"

# --control is the local daemon that `summa42 status`, `approvals` and
# `approve` talk to.
set -- serve \
    --control \
    --intake \
    --mission "$SUMMA42_MISSION" \
    --repo "$SUMMA42_GITHUB_REPOSITORY" \
    --maintainer "$SUMMA42_MAINTAINER" \
    --envelope "$SUMMA42_ENVELOPE" \
    --credential-file "$SUMMA42_GITHUB_TOKEN_FILE" \
    --grant-capability github.issue.read \
    --max-steps "${SUMMA42_MAX_STEPS:-8}" \
    --remaining-budget "${SUMMA42_REMAINING_BUDGET:-10}" \
    --poll-interval "${SUMMA42_INTAKE_INTERVAL:-1m}"
if [ -n "${SUMMA42_AUTO_GRANT_LABEL:-}" ]; then
    # Issues carrying this label may be implemented: the label grants
    # workspace.repo.write and workspace.test to that issue's case.
    set -- "$@" --auto-grant-label "$SUMMA42_AUTO_GRANT_LABEL"
fi
if [ -n "${SUMMA42_BOT_LOGIN:-}" ]; then
    set -- "$@" --bot-login "$SUMMA42_BOT_LOGIN"
fi

# The pipeline runs a worker pass itself; no --worker section is needed.
set -- "$@" \
    --pipeline \
    --mission "$SUMMA42_MISSION" \
    --interval "${SUMMA42_PIPELINE_INTERVAL:-2m}" \
    --repository "$SUMMA42_GITHUB_REPOSITORY" \
    --base-branch "$SUMMA42_BASE_BRANCH" \
    --credential-file "$SUMMA42_GITHUB_TOKEN_FILE" \
    --workspace-root "$SUMMA42_WORKSPACE_ROOT" \
    --source-repo "$SUMMA42_SOURCE_REPO" \
    --source-repository "$SUMMA42_GITHUB_REPOSITORY" \
    --source-sha "$sha" \
    --allowed-prefix "$SUMMA42_ALLOWED_PREFIX" \
    --plan-read-mode \
    --validation-command-json "$SUMMA42_VALIDATION_COMMAND_JSON" \
    --validation-user "$SUMMA42_VALIDATION_USER" \
    --model-user "$SUMMA42_MODEL_USER" \
    --model-profile "$SUMMA42_PRODUCER_PROFILE" \
    --model-name "$SUMMA42_PRODUCER_MODEL" \
    --review-model-user "${SUMMA42_REVIEW_MODEL_USER:-$SUMMA42_MODEL_USER}" \
    --review-model-profile "$SUMMA42_REVIEWER_PROFILE" \
    --review-model-name "$SUMMA42_REVIEWER_MODEL"
if [ -n "${SUMMA42_CONTEXT_MAX_BYTES:-}" ]; then
    set -- "$@" --context-max-bytes "$SUMMA42_CONTEXT_MAX_BYTES"
fi

exec "$box" "$@"
