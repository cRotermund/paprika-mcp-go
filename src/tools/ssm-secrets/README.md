# ssm-secrets

Local developer tool that writes the production secrets for paprika-mcp-go to
AWS SSM Parameter Store. It is never deployed: it has no Dockerfile, Kustomize
manifests, or image publish entry. It shells out to the AWS CLI, passes values
through a short-lived `0600` JSON file rather than the command line, and never
prints them.

The full procedure, including rotation and verification, is in
[docs/secrets-bootstrap-rotation.md](../../../docs/secrets-bootstrap-rotation.md).

## Usage

Run from this directory. The default `-env-file` is
`../../../infra/k8s/prod/secrets.env` (gitignored; copy `secrets.env.example`).

```sh
go run . token                    # generate a bearer token and its hash
go run . bootstrap -dry-run       # validate and list parameter names
go run . bootstrap                # create; fails if a parameter exists
go run . rotate                   # overwrite
```

Flags for `bootstrap` and `rotate`: `-env-file`, `-region` (default
`us-east-1`), `-profile`, `-dry-run`, and `-only` (comma-separated variable
names).

## Tests

```sh
go vet ./... && go test ./...
```
