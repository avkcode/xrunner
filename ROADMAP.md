# Roadmap

## Capacity to 100 Concurrent Jobs

• Hitting 100 simultaneous jobs needs deliberate capacity planning—here’s how I’d approach it beyond the 6-job smoke test:

- Horizontal worker pool: One xrunner-worker process can only push so far before CPU context switches, TMPDIR IO, and sandbox startup storms eat throughput. Run a fleet of workers (VMs or containers), each advertising capacity via a central queue (JetStream, Redis, etc.). The API should publish assignments and let workers RunJob pull from the queue, so adding nodes linearly increases concurrency.
- Concurrency-aware dispatch: Add an internal scheduler that tracks “running jobs per worker” using heartbeats. It should cap per-worker slots (e.g., 8) and per-tenant quotas; when the API sees >100 inflight jobs it still maintains fairness instead of fan-out to a single host.
- Resource hints enforcement: Use ResourceHints to place heavy jobs on beefier workers (GPU/large RAM) and keep lightweight ones together. That prevents a single class of workload from starving everything else.
- Sandbox warm pools: For 100 quick jobs, sandbox startup dominates. Keep warm containers/VMs or at least pre-create workspaces with cached dependencies so dispatch latency stays low.
- Robust observability: Export metrics like xrunner_jobs_running, xrunner_dispatch_latency, worker_queue_depth, and per_tenant_inflight. Alert when queue depth spikes or workers stop heartbeat so you can scale out before SLAs slip.
- Backpressure + admission control: Don’t just accept job #101 if the system is saturated. Either reject with ResourceExhausted or enqueue with an ETA, so clients can retry later. This keeps tail latencies predictable.
- Persistent stores: At 100 jobs, logs/artifacts in RAM won’t cut it. Move log streaming to JetStream/S3 and provide signed download URLs. That keeps memory flat while many jobs stream concurrently.
- Testing: Before production, script a load test (e.g., 100 sleep jobs submitted via CLI, or a chaos tool) and observe queue depth / worker CPU. Use it to tune semaphore sizes, workspace cleanup, and NATS/pandoc limits.

In short, scaling to 100 concurrent jobs is feasible, but only if you treat the control plane like a distributed scheduler: multiple workers, central queue, resource-aware dispatch, strict observability, and backpressure.

## Frontend Enablement

- Layer an HTTP/WebSocket edge: today `cmd/api/main.go` only exposes gRPC on `--listen` (default `:50051`). Wrap that server with gRPC-Gateway, Connect, or a tiny sidecar that translates REST/WS ↔ gRPC so the Tailwind SPA can call `POST /jobs`, `GET /jobs/:id`, etc., without custom transports (`cmd/api/main.go:22-115`). Keep auth/session logic at this edge (cookies or JWT) so the worker/control plane stays unchanged.
- Stream updates for animation: reuse the existing event shapes (`proto/xrunner/v1/events.proto:11-22`) and `JobService.StreamJobLogs` (`proto/xrunner/v1/jobs.proto:108-117`) but expose them as Server‑Sent Events or WebSockets. A simple fan-out service can subscribe to the store/JetStream reader in `internal/control/store` and push events like `{type:"job.updated", job}` or `{type:"log.chunk", data}` so Tailwind components and GSAP/Framer animations react instantly.
- Terminal UX support: `DockerRunWorkload` already flags `interactive`, `tty`, and `keep_container` (`proto/xrunner/v1/jobs.proto:24-37`). Surface those fields in your job templates and stream stdout/stderr into xterm.js via the WS bridge for live output. If you need bidirectional input, add a new RPC (e.g., `AttachJob` with `stream LogChunk` + `stdin bytes`) and proxy it over WebSockets to xterm.js so keystrokes travel back to the worker.
- Artifact/file viewers: workers emit artifacts through the artifact proto; expose an HTTP signer that hands the frontend short-lived download URLs so users can preview PDFs, JSON, or zipped outputs without touching gRPC directly. Cache metadata in the control store so the UI can show thumbnails, status chips, and “open in modal” animations with minimal round trips.
- Presence/backpressure cues: surface metrics the API already tracks (running jobs, dispatch latency, queue depth once you add it) through a `/stats` JSON endpoint so the frontend can show live gauges or disable buttons when limits are hit. That keeps the UX responsive and prevents users from spamming job submissions when the scheduler enforces backpressure.
- Dev workflow: run the Tailwind/Vite dev server alongside xrunner using the new HTTP edge, or embed compiled assets into a `cmd/webui` binary. Use protobuf-generated TypeScript types (`gen/ts/xrunner/v1/*.ts`) to keep frontend contracts in sync with proto evolution, eliminating manual DTOs.

## Frontend Decoupling

• Decoupling Strategy

- Contract-First APIs: Keep xrunner’s Go control plane purely gRPC (`cmd/api/main.go`). Expose a thin HTTP/WebSocket gateway (gRPC-Gateway, Connect-Web, or Envoy) in front of it so the frontend only speaks HTTPS/WebSockets and can even run from a different domain. Version those endpoints independently (e.g., `/v1/jobs`, `/v2/ws/logs`) so frontend deployments aren’t tied to backend releases.
- Generated Client SDK: Publish the TypeScript bindings under `gen/ts/xrunner/v1/*` as an npm package. The frontend consumes this SDK, so schema changes stay synchronized via semver instead of ad hoc DTOs, letting the UI live in its own repo and release cadence.
- Event Bus Bridge: Rather than direct DB access, pipe job/log events through JetStream/NATS (already supported via `--enable-jetstream`). Provide a separate “realtime edge” service that subscribes to those streams and emits WebSockets/SSE to the frontend. The UI then never talks to the worker or store directly—only to the bridge.
- Artifact Proxy Service: Stand up a lightweight artifact API that signs short-lived download URLs from object storage (S3, GCS). The frontend fetches artifacts from that proxy, keeping xrunner’s worker/storage topology hidden.
- Auth & Multi-Tenancy: Put authentication/authorization in the gateway—not in the frontend bundle or worker. Use JWT/OAuth, map claims to `tenant_id`, and pass them as headers/metadata to xrunner. This lets you rotate auth patterns without touching frontend code.
- Deployment Separation: Host the SPA (Tailwind/Vite/Next) on its own pipeline (e.g., Vercel, S3+CloudFront). Backend services deploy via your Go CI/CD. As long as APIs remain backward compatible, each side can roll independently.
- Observability API: Instead of letting the frontend scrape Prometheus, expose curated metrics/status endpoints (e.g., `/status/jobs?tenant=foo`). This keeps operational details encapsulated and lets you evolve observability tooling without breaking dashboards.
