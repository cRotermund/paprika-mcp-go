# Agent Guidance

This file is the operating policy and navigation index for AI coding
assistants working in this repository. Human setup and contribution prose
lives in the documents it points to; do not duplicate it here.

## Read First

- Project purpose, components, and current behavior: [`README.md`](README.md).
- Local setup, branches, commits, pull requests, testing, and review
  expectations: [`CONTRIBUTING.md`](CONTRIBUTING.md).
- System design: [`docs/architecture.md`](docs/architecture.md).
- Container-based local workflow: [`docs/local-dev.md`](docs/local-dev.md).

Read the relevant document before making changes. Treat those documents as the
source of truth rather than copying their contents into this file.

## Project Context

An MCP server exposing Paprika recipe data to MCP clients.

## Tech Stack (Non-Negotiable)

| Layer         | Technology                              |
| ------------- | --------------------------------------- |
| Services      | Go (Dockerized, distroless)             |
| Configuration | Environment variables, `.env` via dotenv |
| Deployment    | Kubernetes + Kustomize                  |
| CI/CD         | GitHub Actions, GHCR                    |
| Index storage | Optional: filesystem or S3 (`aws-sdk-go-v2`) |
| S3 emulation  | SeaweedFS, tests only                   |

**Do not introduce** languages, frameworks, or infrastructure tools outside
this list without explicit approval.

## Directory Conventions

```
src/services/<name>/     <- Each service gets its own Go module and directory
src/tools/<name>/        <- Local developer tools, never deployed; each is its own Go module listed in go.work
src/pkg/                 <- Shared Go libraries (create as needed)
infra/k8s/base/          <- Common Kustomize manifests
infra/k8s/local/         <- Local development overlay
infra/k8s/prod/          <- Production overlay
docs/                    <- Architecture and development guides
tests/                   <- Integration and E2E tests
.github/workflows/       <- CI and image publishing
```

Every Go module is listed in [`go.work`](go.work). When creating a new service,
include at minimum: code, tests, a Dockerfile, a README with local setup
instructions, Kustomize base manifests, and CI and publish matrix entries.

## Code Style

Reference [`.editorconfig`](.editorconfig) at all times:

- **Go:** tabs, indent 4, LF line endings. Follow standard Go conventions
  (`gofmt`, grouped imports: stdlib, third-party, internal). Use the standard
  `testing` package and prefer table-driven tests.
- **YAML/JSON:** spaces, indent 2, LF.
- **All files:** no trailing whitespace, end with a final newline, no
  commented-out code.

## Git Conventions

- **Default branch:** `main`. Never commit directly to it.
- **Branches:** `<type>/<issue-number>/<short-description>`, branched from
  `main`. Types: `feat`, `fix`, `docs`, `chore`, `refactor`,
  `test`.
- **Commits:** Conventional Commits, each referencing an issue:
  ```
  feat(resolves #1): description
  fix(#12): description
  docs(#3): description
  ```
- **PRs:** One logical change per PR. Must pass CI. Requires review.

Details and examples are in [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Agent Operating Rules

- Preserve user changes in the worktree; never revert unrelated work.
- Use focused edits and keep each change limited to one logical purpose.
- Add or update tests for changed behavior.
- Do not stage, commit, push, or open a pull request unless the user
  explicitly requests it.
- Before reporting completion, run the applicable validation from
  `CONTRIBUTING.md` and disclose any environment limitations.

## What to Do

- Write idiomatic Go that follows existing patterns.
- Containerize every service: include a Dockerfile.
- Keep infrastructure declarative (Kustomize overlays in `infra/k8s/`).
- Read configuration from environment variables; keep `.env.example` files
  current when configuration changes.
- Expose `GET /health` on every service and wire it to Kubernetes probes.
- Log with `log/slog` structured output.
- Write tests alongside implementation code.

## What NOT to Do

- Do not add new languages or frameworks without asking.
- Do not commit secrets, `.env` files, or IDE configs (see
  [`.gitignore`](.gitignore)).
- Do not leave commented-out code in commits.
- Do not introduce infrastructure changes outside the Kustomize workflow.
- Do not add dependencies without a clear justification tied to the project's
  goals.
- Do not add secrets, private data, generated binaries, or machine-specific
  paths to the repository.

## Documentation Ownership

- Update `README.md` for human users and current behavior.
- Update `CONTRIBUTING.md` for contributor workflow and review expectations.
- Update `docs/` for architecture and development guides.
- Update this file only for agent-specific navigation or operating rules.
