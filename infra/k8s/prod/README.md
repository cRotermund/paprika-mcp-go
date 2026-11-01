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
