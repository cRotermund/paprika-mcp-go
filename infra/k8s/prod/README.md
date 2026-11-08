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

The Deployment reads `MCP_TOKEN_SHA256`, `PAPRIKA_EMAIL`, and `PAPRIKA_PASSWORD` from the
`paprika-mcp-go` Secret. An `ExternalSecret` in the GitOps repository provisions that Secret in
`chad-applications`; nothing in this repository creates it. `MCP_TOKEN_SHA256` is one or two
comma-separated SHA-256 hashes of the bearer token; the server will not start without it. See the
service README for how to generate one.

## Index snapshot in S3

The overlay sets `INDEX_STORE=s3`, so a restart reloads the snapshot instead of reading the whole
library. The bucket and role come from the Terraform stack `apps/paprika-mcp` in the AWS
infrastructure repository (`bucket_name`, `role_arn`, and `snapshot_key` outputs); none of them is
secret. The base Deployment keeps `INDEX_STORE=none`, so local development needs no AWS.

The cluster has no EKS pod identity webhook, so the Deployment wires up AWS access itself:

- The base `ServiceAccount` is named `paprika-mcp-go`. The role's trust policy accepts only
  `system:serviceaccount:chad-applications:paprika-mcp-go` with audience `sts.amazonaws.com`, so
  renaming either one breaks the exchange.
- A read-only projected `serviceAccountToken` volume (audience `sts.amazonaws.com`, one hour,
  file `token`) is mounted at `/var/run/secrets/aws`. The kubelet refreshes it, and a projected
  volume works with the read-only root filesystem.
- `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` make the AWS SDK's default credential chain
  call `sts:AssumeRoleWithWebIdentity` through the cluster's service-account OIDC issuer. No code
  change or stored AWS key is involved.
- The role can read and write only the one snapshot key and list the bucket for that key. If the
  Terraform `snapshot_key` or bucket changes, update `INDEX_S3_KEY` and `INDEX_S3_BUCKET` in
  `kustomization.yaml`.

## Public endpoint

The public URL is `https://apis.rotorlabs.io/paprika/mcp`. TLS ends at the load balancer, and the
ingress lives in the GitOps repository. The server answers only `/mcp` and `/health`, so the
ingress must strip the `/paprika` prefix before forwarding to Service `paprika-mcp-go` port 8080.
`MCP_ALLOWED_ORIGINS` is `https://apis.rotorlabs.io` (scheme and host only, no path), which is the
`Origin` a browser-based client would send.
