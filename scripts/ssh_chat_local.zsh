#!/usr/bin/env zsh
set -euo pipefail

# Local dev helper: source ~/.zprofile (where you likely keep OPENAI_* exports),
# then run `xrunner ssh chat` against a local `xrunner-remote` (via --proxy).
#
# Usage:
#   ./scripts/ssh_chat_local.zsh
#   XRUNNER_SSH_PROXY=127.0.0.1:7337 ./scripts/ssh_chat_local.zsh -- --raw
#
# Notes:
# - This script does not start `xrunner-remote` for you; run it separately.
# - No secrets are stored in the repo; keep OPENAI_API_KEY in your shell/profile.

if [[ -f "$HOME/.zprofile" ]]; then
  source "$HOME/.zprofile"
fi

export XR_LLM_ENABLED="${XR_LLM_ENABLED:-1}"
export XR_LLM_PROVIDER="${XR_LLM_PROVIDER:-openai_compat}"

proxy="${XRUNNER_SSH_PROXY:-127.0.0.1:7337}"
bundle="${AGENT_TOOL_SPEC:-tools/bundles/bundle.v2025-12-15.json}"

if [[ -z "${OPENAI_BASE_URL:-}" && -z "${XR_LLM_BASE_URL:-}" ]]; then
  echo "missing OPENAI_BASE_URL (or XR_LLM_BASE_URL) in env (try putting it in ~/.zprofile)" >&2
  exit 2
fi
if [[ -z "${OPENAI_API_KEY:-}" && -z "${XR_LLM_API_KEY:-}" ]]; then
  echo "missing OPENAI_API_KEY (or XR_LLM_API_KEY) in env (try putting it in ~/.zprofile)" >&2
  exit 2
fi
if [[ -z "${OPENAI_MODEL:-}" && -z "${XR_LLM_MODEL:-}" ]]; then
  echo "missing OPENAI_MODEL (or XR_LLM_MODEL) in env" >&2
  exit 2
fi

args=()
if [[ "${1:-}" == "--" ]]; then
  shift
  args=("$@")
fi

exec go run ./cmd/xrunner ssh chat \
  --proxy "$proxy" \
  --tool-bundle "$bundle" \
  "${args[@]}"

