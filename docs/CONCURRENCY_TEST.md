# Concurrency Smoke Test

This test fires multiple inline jobs at the remote deployment (`188.124.37.233`) to verify the worker executes 6 jobs concurrently. Each job logs a start/finish timestamp and sleeps for 6 seconds so overlapping execution is obvious.

## 1. Prepare workload script

```bash
cat <<'SCRIPT' > /tmp/concurrency-job.sh
set -euo pipefail
ID=${XR_CONCURRENCY_ID:-unknown}
DURATION=${XR_DURATION_SECONDS:-6}
HOST=$(hostname)
START=$(date -Iseconds)
echo "[$START][$HOST][$ID] starting"
sleep "$DURATION"
END=$(date -Iseconds)
echo "[$END][$HOST][$ID] finished"
SCRIPT
```

## 2. Submit 6 jobs rapidly

```bash
cat <<'SCRIPT' > /tmp/run_concurrent.sh
set -euo pipefail
cd /Users/antonkrylov/work/xrunner
JOB_IDS=()
for i in $(seq 1 6); do
  echo "Submitting job $i"
  OUTPUT=$(go run ./cmd/xrunner --api-addr 188.124.37.233:50051 job submit \
    --tenant default \
    --display-name concurrent-$i \
    --cmd /bin/bash --arg -s \
    --stdin-file /tmp/concurrency-job.sh \
    --env XR_CONCURRENCY_ID=$i \
    --env XR_DURATION_SECONDS=6 \
    --env XR_AGENT_NAME=codex)
  echo "$OUTPUT"
  JOB_ID=$(printf "%s\n" "$OUTPUT" | awk '/^Job/ {print $2}')
  JOB_IDS+=("$JOB_ID")
  sleep 0.2
done
printf "All job IDs: %s\n" "${JOB_IDS[*]}"
SCRIPT
bash /tmp/run_concurrent.sh
```

> Adjust `XR_DURATION_SECONDS` and the number of loop iterations to exercise heavier concurrency (e.g., 10 jobs sleeping for 20s).

## 3. Verify results

```bash
# List most recent jobs
go run ./cmd/xrunner --api-addr 188.124.37.233:50051 job list --tenant default

# Inspect logs for a couple of jobs to confirm overlapping timestamps
go run ./cmd/xrunner --api-addr 188.124.37.233:50051 job logs --tenant default <job-id>
```

On 2025-12-15, the following IDs succeeded with overlapping windows:

- `b55b4c64-6119-4ba3-ab88-988e4f6e4233`
- `cf0c696e-b60d-41a5-80c3-af7f4f7eb207`
- `bb0456ce-b50d-401d-9299-fa5c936ef759`
- `2d2253d9-e3e8-48bb-a8c4-f69ef540b9e7`
- `e5a168f7-59bd-4447-8e7d-3a001a1bfe08`
- `6a4a5530-f76b-4dfd-8a8f-38f78b489a04`

Sample log excerpt showing concurrent execution:

```
[2025-12-15T21:38:28+03:00][Archimedes][1] starting
[2025-12-15T21:38:34+03:00][Archimedes][1] finished
...
[2025-12-15T21:38:31+03:00][Archimedes][6] starting
[2025-12-15T21:38:37+03:00][Archimedes][6] finished
```

Because each job ran for 6 seconds and the start times differ by ~3 seconds, at least 3 jobs overlapped continuously. Increase the loop count or sleep duration for heavier load.

## Preparing for Higher Concurrency

To sustain 10+ simultaneous jobs with predictable latency:

1. **Worker concurrency controls:** Add flags to `cmd/worker` (e.g., `--max-parallel-jobs`) and enforce them via a semaphore inside `worker.Server`. Excess jobs queue locally instead of overloading the host.
2. **Workspace isolation:** Mount tempfs or per-job volumes so concurrent jobs dont contend on `/var/lib/xrunner/workspaces`. Consider garbage-collecting old directories asynchronously.
3. **Resource hints & scheduling:** Honor `ResourceHints` in the worker to select different pools (CPU vs GPU). For multi-worker clusters, have the API dispatch jobs into a queue (JetStream/NATS) and run multiple worker agents (scaled via systemd/k8s) behind a load balancer.
4. **Backpressure + metrics:** Expose Prometheus counters for queue depth, running jobs, and per-tenant concurrency. The API should reject new submissions or delay dispatch if the worker is saturated.
5. **Timeout-aware contexts:** Propagate job deadlines (`PolicyOverrides.MaxExecutionSeconds`) into worker contexts so hung workloads free capacity automatically.
6. **Sharding:** For 10+ concurrent tasks, run separate workers per workload class (e.g., `pandoc`, `docker-build`) to avoid toolchain contention and simplify sizing.

Following these steps ensures todays concurrency smoke test scales into a reliable multi-tenant deployment.
