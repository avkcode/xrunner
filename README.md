# xrunner

API-first prototype for a Windmill-style headless worker service. Everything is defined in protobuf first, gRPC is the system of record, and the CLI simply wraps the same APIs for parity.

## Layout

- `proto/xrunner/v1/*.proto` – canonical protobuf definitions for jobs, workers, artifacts, and policies.
- `gen/go` / `gen/ts` – generated SDKs. Go is used by the control plane + worker. TypeScript is generated via `ts-proto` for other clients.
- `cmd/api` – minimal control-plane gRPC server that owns the `JobService` contract and dispatches work to a worker over gRPC.
- `cmd/worker` – nsjail-aware worker agent implementing `WorkerService.RunJob`.
- `cmd/xrunner` – Cobra-based CLI with full feature parity for the exposed API surface (submit/get/list/logs).
- `configs/default.nsjail.cfg` – starter nsjail profile that clamps resources for inline workloads.

## Prerequisites

- Go 1.24+
- `protoc` (libprotoc ≥ 3.21) with plugins:
  - `protoc-gen-go` and `protoc-gen-go-grpc` (`go install google.golang.org/...`).
  - `ts-proto` (installed via `npm install --save-dev ts-proto`).
- `nsjail` binary on `$PATH`. If it is missing, the worker can fall back to unsandboxed execution when `--allow-unsafe-fallback` is set (default `true` in dev environments).

## Generating SDKs

All changes flow through protobuf definitions. After editing files under `proto/xrunner/v1`, regenerate the SDKs:

```bash
# Go
PATH="$PATH:$(go env GOPATH)/bin" protoc \
  --proto_path=proto \
  --go_out=gen/go --go_opt=paths=source_relative \
  --go-grpc_out=gen/go --go-grpc_opt=paths=source_relative \
  proto/xrunner/v1/*.proto

# TypeScript (ts-proto)
npm run generate:ts
```

Keep generated code checked in so consumers do not need the toolchain.

## Running the prototype

Start the worker agent (listens on `:50052` by default):

```bash
go run ./cmd/worker \
  --listen :50052 \
  --nsjail-config configs/default.nsjail.cfg \
  --workspace /tmp/xrunner/workspaces \
  --allow-unsafe-fallback=true
```

Then start the control-plane API (talks to the worker over gRPC):

```bash
go run ./cmd/api \
  --listen :50051 \
  --worker localhost:50052
```

### Optional: JetStream persistence

To keep job metadata and logs across control-plane restarts, point the API at your NATS deployment:

```bash
nats str add xrunner_jobs --subjects "events.jobs.*.*" --storage file --retention limits --max-msgs=-1 --max-bytes=10GB --dupe-window=2m
nats str add xrunner_logs --subjects "events.logs.*.*" --storage file --retention limits --max-bytes=200GB --dupe-window=2m

go run ./cmd/api \
  --listen :50051 \
  --worker localhost:50052 \
  --enable-jetstream \
  --nats-url nats://188.124.37.233:4222 \
  --nats-user eventbus \
  --nats-pass "$XRUNNER_NATS_PASS"
```

Environment variables `XRUNNER_NATS_URL`, `XRUNNER_NATS_USER`, and `XRUNNER_NATS_PASS` populate the corresponding flags when present. When enabled, the API publishes `JobEvent` and `LogEvent` envelopes to JetStream and replays them on startup before serving traffic, so `xrunner job list/logs` can resume exactly where they left off.

Security note: never commit real credentials to git; use env vars or local-only config files.

## API-first CLI parity

The CLI only speaks gRPC. Every control-plane action currently available via the API is exposed through the CLI:

- `xrunner job submit` → `JobService.SubmitJob`
- `xrunner job get` → `JobService.GetJob`
- `xrunner job list` → `JobService.ListJobs`
- `xrunner job logs` → `JobService.StreamJobLogs`

Example end-to-end flow:

```bash
# Submit an inline workload that echoes JSON
xrunner job submit \
  --tenant default \
  --display-name demo \
  --cmd /bin/sh \
  --arg -c \
  --arg 'echo {"hello":"world"}'

# Run a container directly through docker
xrunner job submit \
  --workload docker-run \
  --container-image alpine:3 \
  --container-cmd /bin/sh \
  --container-cmd -c \
  --container-cmd 'echo hi from container'

# Build an image with buildx/BuildKit inside nsjail
xrunner job submit \
  --workload docker-build \
  --build-context ./examples/agent \
  --dockerfile Dockerfile \
  --tag ghcr.io/me/agent:dev \
  --build-use-buildkit \
  --sandbox-type nsjail \
  --sandbox-config configs/default.nsjail.cfg \
  --sandbox-bind /var/run/docker.sock:/var/run/docker.sock \
  --sandbox-bind /tmp/xrunner/workspaces:/workspace \
  --sandbox-workdir /workspace

# Inspect state
xrunner job list --tenant default
xrunner job get <job-id>

# Replay captured stdout/stderr
xrunner job logs <job-id>
```

The JobService immediately dispatches workloads to the worker agent, records status transitions, and replays the logs captured through the worker’s gRPC response.
`xrunner job logs` now streams output as soon as the worker produces it, so long-running docker builds can be tailed live until completion.

### kubeconfig-style CLI config

`xrunner` now mirrors the `kubectl` UX: it looks for a kubeconfig-style YAML file at `$HOME/.xrunner/config` (override via `XRUNNER_CONFIG` or `--config`). Each entry under `contexts` specifies a `server` (`host:port`) and optional `timeoutSeconds`. The CLI picks `currentContext` by default, but you can pass `--context foo`, override the endpoint with `--api-addr`, and tweak the timeout with `--timeout`. Example:

```yaml
currentContext: dev
contexts:
  dev:
    server: 127.0.0.1:50051
    timeoutSeconds: 20
  staging:
    server: staging.example.com:50051
```

For a scripted Codex Q/A validation that uses this config workflow end-to-end, see `docs/CODEX_QA_SCENARIO.md`.

## Tool manifests

`tools/` hosts OpenAI-compatible tool/function specs for downstream agents. Each tool is described under `tools/defs/*.tool.json`; run the helper to assemble an immutable bundle:

```bash
go run ./cmd/toolspec bundle \
  --out tools/bundles/bundle.v2025-12-15.json \
  --version v2025-12-15
```

Launchers should set `AGENT_TOOL_SPEC` to the on-disk bundle path. The CLI exposes `--tool-bundle` and `--tool-env-var` on `xrunner job submit` which automatically inject the env var into the job environment, ensuring API and CLI pathways share the same manifest.

## Remote testing

See `docs/TESTING.md` for an end-to-end checklist that covers restarting the remote services on `root@188.124.37.233`, submitting a smoke job through the CLI, and verifying logs/state. Use it whenever you need to validate a fresh deployment. For a fully local scenario that mirrors the Codex/OpenAI agent workflow, follow `docs/LOCAL_AGENT_TEST.md`.

## Workload types and sandboxing

`Workload` now supports three execution modes:

1. **Inline** – run the provided executable/args directly (identical to the initial prototype).
2. **docker-run** – call out to `docker run`, including bind mounts, container env vars, and interactive/TTY modes.
3. **docker-build** – use `docker buildx build`/BuildKit with optional tags, cache directives, and compose files.

Every workload can request a specific sandbox via the new `SandboxSpec`. Use CLI flags such as `--sandbox-type`, `--sandbox-config`, and `--sandbox-bind` to describe how the worker should launch the command (pure nsjail, nsjail with docker socket bind for BuildKit-in-nsjail, or “none”/`docker` to run straight on the host).

The worker daemon accepts matching flags (`--default-sandbox-type`, `--default-sandbox-bind`, etc.) so you can keep a global policy while still letting individual jobs override it.

If `nsjail` is missing and `--allow-unsafe-fallback=false`, the worker rejects jobs instead of running them unsandboxed.

## Deploying with systemd

The repository ships systemd units plus an install helper that builds the binaries, installs them under `/usr/local/bin`, and wires everything up to start on boot. Run these steps on the target machine (requires Go 1.22+, `nsjail`, and root privileges):

```bash
git clone https://github.com/antonkrylov/xrunner.git
cd xrunner
sudo ./deploy/systemd/install.sh
```

What the script does:

1. Creates the `xrunner` system user and `/var/lib/xrunner` workspace.
2. Builds `xrunner-api`, `xrunner-worker`, and the `xrunner` CLI into `/usr/local/bin`.
3. Copies `configs/default.nsjail.cfg` to `/etc/xrunner/default.nsjail.cfg`.
4. Installs `/etc/default/xrunner-{api,worker}` so you can override flags (e.g., listening addresses, sandbox behavior).
5. Drops `xrunner-worker.service` and `xrunner-api.service` into `/etc/systemd/system`, reloads the daemon, and enables both services.

Afterwards the API listens on `0.0.0.0:50051` and the worker on `0.0.0.0:50052` (adjust via `/etc/default/xrunner-*`). Use `sudo systemctl status xrunner-api.service` to confirm the deployment or `sudo journalctl -u xrunner-worker -u xrunner-api -f` to tail logs.

## TypeScript SDK notes

Generated files live under `gen/ts/xrunner/v1`. They include promise-based gRPC clients (ts-proto, `grpc-js`). Consumers can import `JobServiceClientImpl` to call the control-plane API directly once network plumbing is in place.

## Next steps

1. Swap the in-memory `Store` with a persistent database and durable log sink.
2. Extend `WorkerService` to a streaming bidirectional protocol for multi-job queues and heartbeats.
3. Flesh out `ArtifactService` to push/pull agent bundles and wire them into worker execution (only inline workloads are supported today).
