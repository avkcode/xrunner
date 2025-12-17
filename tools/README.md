# Tool manifests

This directory holds OpenAI-compatible tool/function specifications that can be fed directly
into a `tools` array for the Responses/Realtime APIs.

```
tools/
  defs/        # individual tool definitions, one per file
  schema/      # json-schema for definitions + bundled artifacts
  bundles/     # immutable bundles assembled from defs/
```

## Bundle metadata

Each bundle uses the following envelope so orchestrators can validate compatibility:

```json
{
  "schema_version": "2025-01-01",
  "bundle_version": "v2025-12-15",
  "generated_at": "2025-12-15T00:00:00Z",
  "tools": [ ... ]
}
```

Agents expect the canonical bundle to be available on disk and referenced via the
`AGENT_TOOL_SPEC` environment variable. Launchers (CLI, API jobs, etc.) should set
this env var before invoking an agent binary so both paths see the exact same manifest:

```
AGENT_TOOL_SPEC=/path/to/tools/bundles/bundle.v2025-12-15.json agent-binary
```

## Development workflow

1. Edit or add files under `defs/`, following `schema/tool.schema.json`.
2. Run `go run ./cmd/toolspec bundle --out tools/bundles/bundle.vYYYY-MM-DD.json --version vYYYY-MM-DD`.
3. Commit both the updated defs and the generated bundle.

The `cmd/toolspec` helper validates schemas, enforces deterministic ordering, and
adds metadata so bundles can be cached/verified by orchestrators.
