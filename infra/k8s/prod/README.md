# Production Deployment

This overlay deploys the services in this repository into the
`chad-applications` namespace. It is rendered and synced by the cluster's GitOps
controller; nothing in this repository applies it automatically.

## Images

The overlay references the `latest` tag of each published image:

```text
ghcr.io/crotermund/paprika-mcp-go/paprika-mcp-go:latest
```

The images are published by `.github/workflows/build.yml` on every push to
`main`. They must exist before the overlay is synced. The
`ghcr-pull-secret` image pull secret referenced by the Deployments must exist
in the target namespace.


## Rendering locally

```sh
kubectl kustomize infra/k8s/prod
```

## Replicas and secrets

The base Deployment runs one replica with the `Recreate` strategy, and the
overlay must not change that: the write lock and the recipe cache live in the
process, so two pods could interleave writes.

The Deployment reads `MCP_TOKEN_SHA256`, `PAPRIKA_EMAIL`, and `PAPRIKA_PASSWORD` from the `paprika-mcp-go` Secret. The
cluster's secret store provisions that Secret, for example with an
`ExternalSecret`, outside this repository. The value is one or two
comma-separated SHA-256 hashes of the bearer token; the server will not start
without it. See the service README for how to generate one.

`INDEX_STORE` defaults to `none` in the base Deployment, so every start reads the whole library. To
keep a snapshot, set `INDEX_STORE` and its settings in this overlay (see the service README); the
root filesystem is read-only, so `filesystem` mode needs a mounted volume.
