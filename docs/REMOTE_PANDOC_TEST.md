# Remote Pandoc DOCX Test

This scenario validates that the remote worker (`root@188.124.37.233`) can render DOCX files via `pandoc` when driven through the xrunner API.

## Steps

1. **Ensure services are running** (already deployed under `/opt/xrunner`). Verify with:
   ```bash
   ssh root@188.124.37.233 "lsof -i :50051"   # API
   ssh root@188.124.37.233 "lsof -i :50052"   # Worker
   ```
2. **Create the inline workload script** on your local machine (the CLI sends it through stdin):
   ```bash
   cat <<'SCRIPT' > /tmp/pandoc-doc.sh
   set -euo pipefail
   DOC_NAME=${XR_DOC_NAME:-codex-pandoc-report}
   TITLE=${XR_DOC_TITLE:-"Codex Remote Pandoc Test"}
   AGENT=${XR_AGENT_NAME:-unknown}
   API=${XR_API_ENDPOINT:-unknown}
   ARTIFACT_DIR=${XR_ARTIFACT_DIR:-/tmp/xrunner-artifacts}
   mkdir -p "$ARTIFACT_DIR"
   python3 - <<'PY'
   import os, datetime, pathlib
   doc_name = os.environ.get('XR_DOC_NAME', 'codex-pandoc-report')
   title = os.environ.get('XR_DOC_TITLE', 'Codex Remote Pandoc Test')
   agent = os.environ.get('XR_AGENT_NAME', 'unknown')
   api = os.environ.get('XR_API_ENDPOINT', 'unknown')
   workspace = os.getcwd()
   hostname = os.uname().nodename
   now = datetime.datetime.now(datetime.timezone.utc).isoformat()
   content = f"""# {title}\n\nThis document was rendered via `pandoc` on {hostname} at {now}.\n\n## Summary\n\n- Agent: {agent}\n- API Endpoint: {api}\n- Workspace: {workspace}\n- Hostname: {hostname}\n\n## Table\n\n| Step | Detail |\n| --- | --- |\n| 1 | Generate markdown |\n| 2 | Convert to DOCX |\n| 3 | Upload artifact |\n"""
   pathlib.Path(doc_name + '.md').write_text(content)
   PY
   pandoc "$DOC_NAME.md" -o "$DOC_NAME.docx"
   ls -lh "$DOC_NAME".*
   sha256sum "$DOC_NAME.docx"
   STAMP=$(date +%Y%m%d-%H%M%S)
   target="$ARTIFACT_DIR/${DOC_NAME}-$STAMP.docx"
   cp "$DOC_NAME.docx" "$target"
   echo "artifact=$target"
   SCRIPT
   ```
3. **Submit the job to the remote API** using the local CLI:
   ```bash
   go run ./cmd/xrunner \
     --api-addr 188.124.37.233:50051 \
     job submit \
     --tenant default \
     --display-name pandoc-docx \
     --cmd /bin/bash \
     --arg -s \
     --stdin-file /tmp/pandoc-doc.sh \
     --env XR_AGENT_NAME=codex \
     --env XR_API_ENDPOINT=188.124.37.233:50051 \
     --env XR_DOC_NAME=codex-pandoc-remote \
     --env XR_DOC_TITLE="Codex Remote Pandoc Test" \
     --env XR_ARTIFACT_DIR=/tmp/xrunner-artifacts
   ```
4. **Validate success and capture logs:**
   ```bash
   go run ./cmd/xrunner --api-addr 188.124.37.233:50051 job get --tenant default <job-id>
   go run ./cmd/xrunner --api-addr 188.124.37.233:50051 job logs --tenant default <job-id>
   ```
   Expected log snippets:
   - `ls -lh codex-pandoc-remote.*`
   - `sha256sum codex-pandoc-remote.docx`
   - `artifact=/tmp/xrunner-artifacts/codex-pandoc-remote-<timestamp>.docx`
5. **Retrieve the artifact** (optional):
   ```bash
   scp root@188.124.37.233:/tmp/xrunner-artifacts/codex-pandoc-remote-<timestamp>.docx build/
   ```
6. **Cleanup** when finished:
   ```bash
   ssh root@188.124.37.233 "rm -f /tmp/xrunner-artifacts/codex-pandoc-remote-*.docx"
   rm -f /tmp/pandoc-doc.sh
   ```

## Latest run
- Job ID: `ef0bf420-9b6e-4ff6-88cc-dc528c6ea2ed`
- SHA256: `987a9e12826f5c3469c11d9fc081e1842078861add6858e40703c283d0702d40`
- Artifact copied locally to `build/codex-pandoc-remote-20251215-212510.docx`
