# Local Codex-Agent Scenario

Use this walkthrough to validate the full CLI → API → Worker path on your dev host when the Codex/OpenAI agent is enabled. It recreates the exact inline job we just ran, which writes and executes a script entirely inside the worker workspace.

## Prereqs

- Go 1.22+, `protoc` artifacts already generated.
- No API/worker currently listening on :50051/:50052.
- `nsjail` optional; this flow forces `--default-sandbox-type none` to keep the focus on wiring.
- Optional: LLM-backed chat (DeepSeek/OpenAI-compatible or Gemini) via `XR_LLM_*` env vars in `xrunner-remote`.

## 1. Prepare the inline payload

```bash
cat <<'EOF' > /tmp/script-writer.sh
set -euo pipefail
WORKDIR="$PWD"
cat <<'SCRIPT' > generated_script.sh
#!/bin/sh
timestamp="$(date -Iseconds)"
echo "[$timestamp] codex-agent generated this script on $(hostname)" | tee script.log
echo "Workspace contents:"; ls -a
SCRIPT
chmod +x generated_script.sh
./generated_script.sh
printf '\nGenerated files:\n'
ls -l generated_script.sh script.log
printf '\nLog contents:\n'
cat script.log
EOF
```

This script writes `generated_script.sh`, runs it, and prints both filesystem contents and the resulting log so you can confirm artifacts live under the worker workspace.

## 2. Start the worker

```bash
nohup go run ./cmd/worker \
  --listen :50052 \
  --workspace /tmp/xrunner/workspaces \
  --allow-unsafe-fallback=false \
  --default-sandbox-type none \
  > /tmp/xrunner-worker.log 2>&1 &
```

Wait for `worker ready` inside `/tmp/xrunner-worker.log` before proceeding.

## 3. Start the control plane

```bash
nohup go run ./cmd/api \
  --listen :50051 \
  --worker localhost:50052 \
  > /tmp/xrunner-api.log 2>&1 &
```

Confirm `/tmp/xrunner-api.log` shows `api ready` and the worker endpoint.

## 4. Submit the job via CLI

```bash
JOB_ID=$(go run ./cmd/xrunner \
  --api-addr localhost:50051 \
  job submit \
  --tenant default \
  --display-name codex-script-writer \
  --cmd /bin/sh \
  --arg -s \
  --stdin-file /tmp/script-writer.sh \
  --env XR_AGENT_NAME=codex-openai-agent \
  | awk '/^Job/ {print $2}')
```

- `--arg -s` tells `/bin/sh` to read the payload from stdin.
- `XR_AGENT_NAME` proves the host-agent metadata reaches the workload environment.
- The CLI prints the job line immediately; `JOB_ID` captures it for reuse.

## 5. Verify state and logs

```bash
go run ./cmd/xrunner --api-addr localhost:50051 job get --tenant default "$JOB_ID"
go run ./cmd/xrunner --api-addr localhost:50051 job logs --tenant default "$JOB_ID"
```

Expected output:

- `job get` reports `JOB_STATE_SUCCEEDED` with `Exit: 0` and timestamps near submission.
- `job logs` shows the `codex-agent generated this script` banner, the workspace listing (`generated_script.sh`, `script.log`), and two copies of the script output (one from live streaming, one from the aggregated response).

## 6. Inspect artifacts (optional)

On the machine hosting the worker, the job workspace lives under `/tmp/xrunner/workspaces/<JOB_ID>/`. You can list it to confirm `generated_script.sh` and `script.log` exist.

## 7. Cleanup

```bash
pkill -f "cmd/api" || true
pkill -f "cmd/worker" || true
rm -f /tmp/script-writer.sh
rm -rf /tmp/xrunner/workspaces
```

Stop the daemons and remove the temporary payload/workspaces when you are done to keep subsequent tests clean.

Following these steps exercises the full stack using real binaries on the host, matching the scenario Codex/OpenAI agents expect when submitting inline workloads.
