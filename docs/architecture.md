# Architecture

## Overview

An MCP server exposing Paprika recipe data to MCP clients.

## Components

| Component | Location | Responsibility |
| --- | --- | --- |
| paprika-mcp-go | `src/services/paprika-mcp-go/` | MCP server for Paprika recipe data, served over streamable HTTP at `/mcp` (or stdio with `-stdio`). |
| paprika-api-probe | `src/tools/paprika-api-probe/` | Local exploratory tool that checks `docs/paprika-api` against the live Paprika API. Not deployed. |

## Conventions

- Each service is its own Go module, joined by `go.work` for local development.
- Services are configured only through environment variables. `.env` files are
  a local convenience and are never part of an image or the repository.
- Every service exposes `GET /health`; Kubernetes liveness and readiness
  probes use it.
- Images are built from the repository root with a per-service Dockerfile, run
  as non-root on a distroless base, and are published to GHCR on every push to
  `main`.

## Deployment

Kubernetes manifests live under `infra/k8s/`:

- `base/` holds the Deployment and Service for each deployed service.
- `local/` retargets images to `localhost/` and sets `LOG_LEVEL=DEBUG`.
- `prod/` uses the `latest` GHCR images, and sets replicas and pull policy. See
  `infra/k8s/prod/README.md`.

## Planned Work

The design for the full Paprika MCP server, including security and the delivery plan, is in
[mcp-server-spec.md](mcp-server-spec.md). The upstream protocol is documented in
[paprika-api](paprika-api/README.md).
