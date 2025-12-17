# Remote E2E Testing

Use these steps to validate an xrunner deployment on a remote host. The recommended path is `xrunner ssh bootstrap`, which builds a Linux payload, installs systemd units, and starts `xrunner-worker`, `xrunner-api`, and `xrunner-remote`.

## Recommended: bootstrap + verify

Prereqs:
- SSH key auth to the host (bootstrap uses `ssh` + `scp` and runs `sudo` if you’re not root).
- Local Go toolchain if you don’t pass `--bin-dir` (bootstrap builds Linux binaries for you).

1. **Dry-run the bootstrap plan**
   ```bash
   go run ./cmd/xrunner ssh bootstrap root@YOUR_HOST --dry-run
   ```

2. **Bootstrap the host (systemd install + start services)**
   ```bash
   go run ./cmd/xrunner ssh bootstrap root@YOUR_HOST --write-config
   ```
   Expected tail output includes:
   - `[xrunner] bootstrap ok`
   - `services: worker=active api=active remote=active`

3. **Verify systemd and ports on the host**
   ```bash
   ssh root@YOUR_HOST "systemctl status xrunner-worker xrunner-api xrunner-remote --no-pager"
   ssh root@YOUR_HOST "ss -ltn | egrep ':(50051|50052|7337) ' || true"
   ```

4. **Verify the CLI → API → worker path from your workstation**
   ```bash
   xrunner job list --tenant default --api-addr YOUR_HOST:50051
   JOB_ID=$(xrunner job submit \
     --tenant default \
     --display-name smoke \
     --cmd /bin/sh \
     --arg -c \
     --arg 'echo hello-from-inline' \
     --api-addr YOUR_HOST:50051)
   xrunner job get --tenant default "$JOB_ID" --api-addr YOUR_HOST:50051
   xrunner job logs --tenant default "$JOB_ID" --api-addr YOUR_HOST:50051
   ```
   Expected: `JOB_STATE_SUCCEEDED`, exit code `0`, and `hello-from-inline` in logs.

5. **Verify bootstrap-only features (remote terminal gateway)**

   `xrunner-remote` listens on `127.0.0.1:7337` on the host (systemd default), so you typically use an SSH forward:

   ```bash
   xrunner ssh proxy --host root@YOUR_HOST
   # prints: export XRUNNER_SSH_PROXY=127.0.0.1:<port>
   export XRUNNER_SSH_PROXY=127.0.0.1:<port>
   xrunner ssh exec --cmd 'echo hello' --raw
   xrunner ssh attach --cmd htop
   xrunner ssh chat
   ```

## Bootstrap troubleshooting

Common failure modes and what to check:

- **SSH authentication fails / asks for password**
  - Bootstrap uses `ssh`/`scp` with BatchMode semantics; ensure key auth works first:
    - `ssh -o BatchMode=yes root@YOUR_HOST 'echo ok'`
  - If you need a non-default key/port, pass it through:
    - `xrunner ssh bootstrap root@YOUR_HOST --ssh-arg=-i --ssh-arg=~/.ssh/id_ed25519 --ssh-arg=-p --ssh-arg=2222`

- **Not enough privileges (sudo prompts)**
  - If you’re not root, bootstrap runs `sudo bash -se` and requests a TTY; make sure your user can `sudo` non-interactively or run as `root@...`.

- **systemd missing**
  - Bootstrap requires `systemctl` on the remote host:
    - `ssh root@YOUR_HOST 'command -v systemctl && systemctl --version'`

- **Port 7337 already in use**
  - Bootstrap aborts if `127.0.0.1:7337` is in use by a non-`xrunner-remote` process. Inspect:
    - `ssh root@YOUR_HOST "ss -ltnp | grep ':7337 ' || true"`
  - If it’s a stale `xrunner-remote`, stop it:
    - `ssh root@YOUR_HOST 'systemctl stop xrunner-remote || true; pkill -f xrunner-remote || true'`

- **Services not starting / crash-looping**
  - Check unit status + recent logs:
    - `ssh root@YOUR_HOST "systemctl status xrunner-worker xrunner-api xrunner-remote --no-pager"`
    - `ssh root@YOUR_HOST "journalctl -u xrunner-worker -u xrunner-api -u xrunner-remote -n 200 --no-pager"`
  - Verify config files installed by bootstrap:
    - `ssh root@YOUR_HOST "ls -la /etc/default/xrunner-* /etc/systemd/system/xrunner-*.service /etc/xrunner/default.nsjail.cfg"`
    - `ssh root@YOUR_HOST "sed -n '1,200p' /etc/default/xrunner-worker; echo '---'; sed -n '1,200p' /etc/default/xrunner-api; echo '---'; sed -n '1,200p' /etc/default/xrunner-remote"`

- **Worker fails due to nsjail**
  - The default systemd env enables `nsjail`. Quick diagnostic is to temporarily run unsandboxed:
    - `ssh root@YOUR_HOST "sed -i 's/^XRUNNER_WORKER_SANDBOX_TYPE=.*/XRUNNER_WORKER_SANDBOX_TYPE=none/' /etc/default/xrunner-worker && systemctl restart xrunner-worker"`
  - Then re-check `journalctl` output and iterate on the nsjail config for that host.

To re-run bootstrap cleanly, stop services first:
```bash
ssh root@YOUR_HOST "systemctl disable --now xrunner-worker xrunner-api xrunner-remote || true"
```

## Legacy: manual remote process management

If you’re not using `xrunner ssh bootstrap` yet, you can still run the daemons manually (update paths/flags for your environment):

```bash
ssh root@YOUR_HOST "pkill -f xrunner-worker || true"
ssh root@YOUR_HOST "nohup /opt/xrunner/xrunner-worker \
    --listen :50052 \
    --workspace /var/lib/xrunner/workspaces \
    --default-sandbox-type none \
    --nsjail-config /opt/xrunner/default.nsjail.cfg \
    > /var/log/xrunner-worker.log 2>&1 &"
ssh root@YOUR_HOST "nohup /opt/xrunner/xrunner-api \
    --listen :50051 \
    --worker localhost:50052 \
    > /var/log/xrunner-api.log 2>&1 &"
```

If jobs fail, inspect logs:
- `ssh root@YOUR_HOST "tail -n 120 /var/log/xrunner-worker.log"`
- `ssh root@YOUR_HOST "tail -n 120 /var/log/xrunner-api.log"`

## Remote terminal (xrunner-remote)

`xrunner-remote` is a small gRPC daemon you can run on the remote host that provides:

- Remote PTY streaming (`xrunner ssh attach …`)
- Structured session events (`xrunner ssh exec --jsonl …`)
- Remote file operations (used by some agent workflows)

### Install / start xrunner-remote on the remote host

Preferred: use `xrunner ssh bootstrap`, which installs `xrunner-remote` via systemd alongside `xrunner-api` and `xrunner-worker`.

Manual install (if you only want the remote gateway): build a Linux binary locally and copy it over:

```bash
GOOS=linux GOARCH=amd64 go build -o /tmp/xrunner-remote ./cmd/xrunner-remote
ssh root@YOUR_HOST "mkdir -p /root/.xrunner/bin"
scp /tmp/xrunner-remote root@YOUR_HOST:/root/.xrunner/bin/xrunner-remote
ssh root@YOUR_HOST "chmod +x /root/.xrunner/bin/xrunner-remote"
ssh root@YOUR_HOST "nohup /root/.xrunner/bin/xrunner-remote --listen 127.0.0.1:7337 --upstream 127.0.0.1:50051 --workspace-root / --unsafe-allow-rootfs=true >/root/.xrunner/remote.log 2>&1 &"
```

### Persistent proxy (fast UX)

Keep one SSH forward alive and reuse it across commands:

```bash
xrunner ssh proxy --host root@188.124.37.233
# prints: export XRUNNER_SSH_PROXY=127.0.0.1:<port>
```

Then (in the same shell):

```bash
xrunner ssh exec --cmd 'echo hello' --raw
xrunner ssh attach --cmd htop
xrunner ssh chat
```

### Durable tmux sessions + replay

Attach to a named tmux session (survives network disconnects):

```bash
xrunner ssh attach --resume ops
```

Replay the last N lines before attaching:

```bash
xrunner ssh attach --resume ops --replay 200
```

Replay only (plain text or JSON):

```bash
xrunner ssh attach --resume ops --replay 200 --replay-only
xrunner ssh attach --resume ops --replay 200 --replay-only --replay-json
```

### Codex-style interactive loop (no TUI)

`xrunner ssh chat` is a simple REPL that streams structured events from `SessionService` while you type commands:

```bash
xrunner ssh chat
# inside:
#   /help
#   !echo hello
#   /shell uname -a
#   /exit
```

To keep state (cwd, history, long-running processes) in one durable remote shell, pair it with tmux:

```bash
xrunner ssh attach --resume ops
xrunner ssh chat --resume ops
```

In this mode, `!cmd` and `/shell cmd` are sent into the tmux session and `xrunner` polls `tmux capture-pane` to print the resulting output.

### Tooling prerequisites (Ubuntu)

If `tree`/`htop`/terminfo are missing, use `deploy/runbooks/bootstrap_terminal_ubuntu.yaml` (edit the `host:` field first):

```bash
xrunner runbook apply deploy/runbooks/bootstrap_terminal_ubuntu.yaml --confirm
```

## Tests

- Local unit/integration tests: `go test ./...`
- Remote E2E test (requires SSH access + key auth):

```bash
XRUNNER_E2E_SSH_HOST=root@YOUR_HOST go test ./internal/e2e -run TestE2E_SSHRemoteHost
```
