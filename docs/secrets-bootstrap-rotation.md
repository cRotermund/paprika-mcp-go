# Production Secret Bootstrap And Rotation SOP

This procedure provisions the runtime secrets for the production deployment.
Values stay in a local, gitignored env file and are written to AWS Systems
Manager Parameter Store as `SecureString` parameters. The `ExternalSecret`
in the GitOps repository (`chad-k8s-gitops`, `apps/paprika-mcp-go`) reads them
through the External Secrets Operator and builds the `paprika-mcp-go` Secret
in `chad-applications`. Values are never committed to Git or placed in a
manifest.

## Parameters

The tool [`src/tools/ssm-secrets`](../src/tools/ssm-secrets/README.md) writes
these parameters. The External Secrets Operator can read anything under
`/kubernetes/appsecrets/`.

| Parameter | Secret key | Purpose |
| --- | --- | --- |
| `/kubernetes/appsecrets/chad-applications/paprika-mcp-go/mcp-token-sha256` | `MCP_TOKEN_SHA256` | One or two SHA-256 hashes of the bearer token |
| `/kubernetes/appsecrets/chad-applications/paprika-mcp-go/paprika-email` | `PAPRIKA_EMAIL` | Paprika account email |
| `/kubernetes/appsecrets/chad-applications/paprika-mcp-go/paprika-password` | `PAPRIKA_PASSWORD` | Paprika account password |

## Prerequisites

- Go and the AWS CLI are installed, and the AWS CLI is authenticated with an
  identity allowed to write the `/kubernetes/appsecrets/` path in `us-east-1`
  (and to use the KMS key, if one is customer-managed).
- Use the real Paprika account's credentials, since production reads the real
  library. Never use a stress or probe tool against it; see the service README.
- The GitOps ExternalSecret exists in the cluster. Order does not matter: it
  reports an error until the parameters exist, then recovers at its next
  refresh (hourly).

## Prepare the local secret file

From the repository root:

```sh
cp infra/k8s/prod/secrets.env.example infra/k8s/prod/secrets.env
git check-ignore infra/k8s/prod/secrets.env
```

The second command must print the path. If it prints nothing, stop.

Generate a bearer token and its hash:

```sh
cd src/tools/ssm-secrets
go run . token
```

Give the token to your MCP client. It is shown once and stored nowhere. Put
the hash in `MCP_TOKEN_SHA256`, and set `PAPRIKA_EMAIL` and `PAPRIKA_PASSWORD`
in `infra/k8s/prod/secrets.env`. Never paste these values into command-line
arguments, issues, pull requests, or logs.

## Bootstrap

Validate first. This prints parameter names and writes nothing:

```sh
cd src/tools/ssm-secrets
go run . bootstrap -dry-run
```

Then create the parameters. Bootstrap refuses to overwrite an existing one:

```sh
go run . bootstrap
```

Add `-profile NAME` or `-region REGION` when needed, and `-only VAR[,VAR]` to
sync a subset (`MCP_TOKEN_SHA256`, `PAPRIKA_EMAIL`, `PAPRIKA_PASSWORD`). The
tool prints parameter names only.

## Rotate

Update `secrets.env`, then overwrite the parameters:

```sh
go run . rotate
```

To rotate the bearer token without downtime, use two hashes:

1. Run `go run . token`, then set `MCP_TOKEN_SHA256=<old-hash>,<new-hash>` and
   run `go run . rotate -only MCP_TOKEN_SHA256`.
2. Restart the Deployment (below), then switch clients to the new token.
3. Set `MCP_TOKEN_SHA256=<new-hash>`, rotate again, and restart.

The pod reads the Secret as environment variables at start, so it only sees a
change after a restart. After the ExternalSecret refresh interval, or sooner
if you force a sync, confirm it and restart:

```sh
kubectl get externalsecret paprika-mcp-go -n chad-applications
kubectl rollout restart deployment/paprika-mcp-go -n chad-applications
kubectl rollout status deployment/paprika-mcp-go -n chad-applications
```

## Verify

Check metadata only; never print values:

```sh
aws ssm get-parameter \
  --name /kubernetes/appsecrets/chad-applications/paprika-mcp-go/mcp-token-sha256 \
  --query 'Parameter.{Name:Name,Type:Type,Version:Version}'
```

The type must be `SecureString`. After the deployment is up, an unauthenticated
request to the public endpoint should return 401:

```sh
curl -i https://apis.rotorlabs.io/paprika/mcp
```

A 404 means the ingress is not stripping the `/paprika` prefix. See
[infra/k8s/prod/README.md](../infra/k8s/prod/README.md).

## Cleanup

Delete `infra/k8s/prod/secrets.env` when you no longer need it. If you keep it
for future rotation, keep it on an encrypted disk. Revoke a superseded bearer
token by dropping its hash from `MCP_TOKEN_SHA256`.

## Failure handling

- `ParameterAlreadyExists` on bootstrap: use `rotate`, after confirming the
  file holds the intended values.
- `AccessDenied`: check the operator identity, region, parameter path, and KMS
  permissions.
- ExternalSecret not `Ready`: check its status and the operator logs without
  printing generated Secret data.
- Pod fails at start with a missing hash: the Secret lacks `MCP_TOKEN_SHA256`,
  so the parameter is missing or the ExternalSecret has not synced.
- Clients get 401 after a rotation: the pod has not restarted, or the client
  token does not match a stored hash.
