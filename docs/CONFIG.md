# CLI Configuration

The `xrunner` CLI now supports a kubeconfig-style YAML file so you can manage multiple control-plane endpoints just like `kubectl`.

## File location

1. `--config` flag has highest priority.
2. `XRUNNER_CONFIG` environment variable overrides the default path.
3. Otherwise the CLI loads `$HOME/.xrunner/config`.

## Format

```yaml
currentContext: dev
contexts:
  dev:
    server: 127.0.0.1:50051
    timeoutSeconds: 20
  staging:
    server: staging.internal:50051
```

- `currentContext` mirrors kubeconfig semantics; if omitted, pass `--context`.
- Each context requires a `server`. `timeoutSeconds` overrides the default 15 s.

## Override order

1. Flags (`--api-addr`, `--timeout`, `--context`).
2. Config file values.
3. Environment (`XRUNNER_API_ADDR` for the endpoint).
4. Built-in defaults (`localhost:50051`, 15 s).

## Generating configs

Control-plane deployments can expose an endpoint that returns this YAML. Clients download it and place it under `$HOME/.xrunner/config`, then run:

```bash
xrunner --context staging job list
```

That makes the CLI feel much closer to `kubectl`, including per-environment profiles and zero command-line overrides once configured.
