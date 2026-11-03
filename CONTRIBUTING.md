# Contributing

## Technology Rules

Only these technologies are permitted without prior discussion:

- **Go** for services
- **Docker** for containers (every service must be containerized)
- **Kubernetes** as the deployment target
- **Kustomize** for Kubernetes configuration management
- **GitHub Actions** and **GHCR** for CI and image publishing

Anything outside this list requires approval via an issue or discussion. This
keeps the stack small, maintainable, and cheap to run.

## Getting Started

### Prerequisites

| Tool    | Version | Install Link                                              | Required For          |
| ------- | ------- | --------------------------------------------------------- | --------------------- |
| Go      | >= 1.26 | [go.dev/dl](https://go.dev/dl/)                 | Services              |
| Docker  | latest  | Or a compatible runtime such as Podman                    | Container builds      |
| kubectl | latest  | [kubernetes.io](https://kubernetes.io/docs/tasks/tools/)  | Kubernetes deployments |

`kubectl kustomize` and `kubectl apply -k` provide Kustomize support, so a
separate `kustomize` install is optional.

If you only need Path A below, Go is enough.

Clone the repository:

```sh
git clone https://github.com/cRotermund/paprika-mcp-go.git
cd paprika-mcp-go
```

### Two Development Workflows

#### Path A: Run services directly on your machine

Best for debugging and fast iteration. No containers, no Kubernetes.

```sh
cd src/services/paprika-mcp-go
cp .env.example .env
go run .                              # listens on :8080
```

Services read configuration from environment variables. A `.env` file in the
working directory is loaded for local runs; real environment variables take
precedence. `.env` files are gitignored and must never be committed.

#### Path B: Run in containers with Kubernetes

Matches the production environment. See [docs/local-dev.md](docs/local-dev.md)
for the full walkthrough. In short, from the repository root:

```sh
docker build -f src/services/paprika-mcp-go/Dockerfile -t localhost/paprika-mcp-go:latest .
kubectl apply -k infra/k8s/local
kubectl port-forward deployment/paprika-mcp-go 8080:8080
```

### Environmental Quirks

- **Line endings:** `.editorconfig` and `.gitattributes` enforce LF. If you
  see CRLF warnings on Windows, run `git config core.autocrlf input`.
- **Shell:** Commands in this guide use POSIX shell syntax (Git Bash or WSL2).
  In PowerShell, replace `PORT=9090 go run .` with `$env:PORT=9090; go run .`.
- **Container runtime:** Docker and Podman both work with the provided
  Dockerfiles.

### Troubleshooting

| Problem                                | Likely Cause                              | Fix                                                        |
| -------------------------------------- | ----------------------------------------- | ---------------------------------------------------------- |
| `ErrImagePull` / `ImagePullBackOff`    | Image not available in the cluster cache. | Rebuild the image and load it into the cluster (see docs/local-dev.md). |
| `CrashLoopBackOff`                     | Missing configuration or a code error.    | `kubectl logs deployment/<name>`; check required env vars. |
| `go run .` fails with a missing package | Not running from the service directory.  | `cd` into the service directory first.                     |
| Port already in use                    | Another process or `port-forward` is running. | Stop it, or change `PORT` / the local port.            |

## Development Workflow

### Branches

Branch from `main`. Direct commits and pushes to
`main` are not allowed. Name branches:

```
<type>/<issue-number>/<short-description>
```

`<type>` is one of `feat`, `fix`, `docs`, `chore`, `refactor`, or `test`.
Examples:

- `feat/1/add-health-endpoint`
- `fix/12/handle-missing-config`

### Commits

Use [Conventional Commits](https://www.conventionalcommits.org/). Every commit
must reference an issue:

```
feat(resolves #1): add the health endpoint
fix(#12): handle missing PORT configuration
docs(#3): add local development guide
```

Keep commits focused. One logical change per commit.

| Prefix     | When to use                                          |
| ---------- | ---------------------------------------------------- |
| `feat`     | New feature or capability                            |
| `fix`      | Bug fix                                              |
| `docs`     | Documentation only                                   |
| `chore`    | Maintenance: dependencies, build scripts, tooling, CI |
| `refactor` | Code restructuring that does not change behavior     |
| `test`     | Adding or updating tests                             |

#### Issue Linking

| Syntax         | Effect                                                              |
| -------------- | ------------------------------------------------------------------- |
| `(#12)`        | Links to the issue without closing it. Use for one step of several. |
| `(closes #12)` | Closes the issue when merged to `main`. Use when the commit or PR fully resolves it. |

Accepted closing keywords are `close`, `closes`, `closed`, `fix`, `fixes`,
`fixed`, `resolve`, `resolves`, and `resolved`. Pick one and be consistent in
the PR.

```
# Multi-commit feature: only the final commit closes the issue
feat(#5): scaffold the new endpoint
feat(#5): add request validation
feat(resolves #5): document the endpoint

# Single-commit fix: closes immediately
fix(closes #8): correct the status code on not-found
```

### Pull Requests

1. Create a branch from `main`.
2. Make changes, following the conventions below.
3. Open a PR against `main` with a clear description of what and
   why.
4. CI must pass and at least one approving review is required before merge.

## Code Style

An [`.editorconfig`](.editorconfig) is provided. Editors should respect it
automatically.

| Language    | Indent | Tabs/Spaces |
| ----------- | ------ | ----------- |
| Go          | 4      | Tabs        |
| JSON / YAML | 2      | Spaces      |
| All files   | LF     | n/a         |

Additional guidelines:

- **Go:** Follow standard Go conventions. Use `gofmt`. Group imports (stdlib,
  third-party, internal).
- **Long lists:** When a parameter, argument, or field list does not fit
  comfortably on one line (it already wraps, or the line exceeds 100 columns,
  counting a tab as 4), put every item on its own line with a trailing comma
  and the closing bracket on its own line. Never wrap several items per line.
  Short lists that fit on one line stay as they are. This applies to function
  and method parameters, call arguments, and struct, slice, and map literals,
  including table-driven test rows that wrap. A longer file is fine; the goal
  is lists that are easy to scan and make ordering clear.

  ```go
  func (t *tools) getRecipe(
  	ctx context.Context,
  	_ *mcp.CallToolRequest,
  	in getRecipeInput,
  ) (*mcp.CallToolResult, getRecipeOutput, error) {
  ```

- **General:** No commented-out code in commits. Trim trailing whitespace. End
  files with a newline.

## Testing

Write tests for new functionality. Use table-driven tests with the standard
`testing` package. Run the checks CI runs, for every module in the workspace:

```sh
gofmt -l .
for dir in $(go list -m -f '{{.Dir}}'); do (cd "$dir" && go vet ./... && go test ./...); done
kubectl kustomize infra/k8s/local > /dev/null
kubectl kustomize infra/k8s/prod > /dev/null
```

## Adding A Service

1. Create `src/services/<name>/` with a Go module, `main.go`, `internal/`, a
   Dockerfile, a README, and `.env.example`.
2. Add the module to [`go.work`](go.work).
3. Add base manifests under `infra/k8s/base/` and register them in its
   `kustomization.yaml`; extend the `local` and `prod` overlays.
4. Add the service to the matrices in `.github/workflows/ci.yml` and
   `.github/workflows/build.yml`.
5. Add it to the Repository Contents list in `README.md`.

## Directory Structure

```
src/
├── services/
│   └── paprika-mcp-go/            # Go service
├── tools/
│   └── paprika-api-probe/         # Local developer tool, never deployed
└── pkg/                        # Shared Go libraries (create as needed)
docs/                           # Architecture and development guides
infra/
└── k8s/
    ├── base/                   # Common Kustomize base
    ├── local/                  # Local development overlay
    └── prod/                   # Production overlay
tests/                          # Integration / E2E tests
.github/
└── workflows/                  # CI and image publishing
```

## Finding Work

Check the [issues tab](https://github.com/cRotermund/paprika-mcp-go/issues) for
open work. If you want to tackle something not listed, open an issue to discuss
it first.

## Questions?

Open a discussion or comment on a relevant issue.
