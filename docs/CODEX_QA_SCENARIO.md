# Codex Q/A Validation Scenario

This walkthrough proves that the Codex/OpenAI agent can submit a question/answer workload through `xrunner`, using the kubeconfig-style CLI defaults.

## 1. Prepare config (once per machine)

```bash
mkdir -p "$HOME/.xrunner"
cat <<'YAML' > "$HOME/.xrunner/config"
currentContext: local-codex
contexts:
  local-codex:
    server: localhost:50051
    timeoutSeconds: 20
YAML
```

## 2. Launch worker and API locally

```bash
nohup go run ./cmd/worker \
  --listen :50052 \
  --workspace /tmp/xrunner/workspaces \
  --allow-unsafe-fallback=false \
  --default-sandbox-type none \
  > /tmp/xrunner-worker.log 2>&1 &

nohup go run ./cmd/api \
  --listen :50051 \
  --worker localhost:50052 \
  > /tmp/xrunner-api.log 2>&1 &
```

Wait for `worker ready` and `api ready` in the log files.

## 3. Create the QA payload

```bash
cat <<'EOF' > /tmp/codex-qa.sh
set -euo pipefail
QUESTION=${XR_QUESTION:-"What is Codex?"}
ANSWER=${XR_EXPECTED_ANSWER:-"Codex is the OpenAI code-generation model."}
cat <<MSG
[Codex QA]
Question: $QUESTION
Answer: $ANSWER
MSG
EOF
chmod +x /tmp/codex-qa.sh
```

## 4. Submit the job via CLI

```bash
JOB_ID=$(go run ./cmd/xrunner \
  job submit \
  --tenant default \
  --display-name codex-qa \
  --cmd /bin/sh \
  --arg /tmp/codex-qa.sh \
  --env XR_QUESTION="What does Codex do?" \
  --env XR_EXPECTED_ANSWER="It powers the OpenAI coding agent that runs jobs via xrunner." \
  | awk '/^Job/ {print $2}')
```

Because the config already points at `localhost:50051`, no `--api-addr` flag is required.

## 5. Verify state and logs

```bash
go run ./cmd/xrunner job get --tenant default "$JOB_ID"
go run ./cmd/xrunner job logs --tenant default "$JOB_ID"
```

Expected output (abbreviated):

```
Job $JOB_ID (codex-qa)
  State: JOB_STATE_SUCCEEDED
  Exit: 0
...
[Codex QA]
Question: What does Codex do?
Answer: It powers the OpenAI coding agent that runs jobs via xrunner.
```

## 6. Cleanup

```bash
pkill -f "cmd/api" || true
pkill -f "cmd/worker" || true
rm -f /tmp/codex-qa.sh
rm -rf /tmp/xrunner/workspaces
```

This completes an end-to-end Codex Q/A validation run using the kubeconfig-like defaults.
