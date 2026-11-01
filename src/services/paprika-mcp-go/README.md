# paprika-mcp-go

MCP server for Paprika recipe data, served over streamable HTTP at `/mcp` (or stdio with `-stdio`).

## Quick start

```sh
cp .env.example .env
go run .                              # listens on :8080
```

See [CONTRIBUTING.md](../../../CONTRIBUTING.md) for the full development
environment, including the container-based workflow with Kubernetes.

## Configuration

Configuration comes from environment variables. For local runs, values in a
`.env` file in the working directory are loaded first; real environment
variables take precedence over `.env`.

| Variable | Required | Description |
| --- | --- | --- |
| `PORT` | yes | TCP port the HTTP server listens on. |
| `LOG_LEVEL` | no | `DEBUG`, `INFO` (default), `WARN`, or `ERROR`. |

## Routes

- `GET /health` returns `{"status":"healthy"}` and backs the Kubernetes probes.
- `/mcp` serves MCP over streamable HTTP. It currently has only a `ping` tool.

## Building

```sh
CGO_ENABLED=0 go build -o server .
```

## Docker

The build context is the repository root:

```sh
docker build -f src/services/paprika-mcp-go/Dockerfile -t paprika-mcp-go .
docker run --env-file src/services/paprika-mcp-go/.env -p 8080:8080 paprika-mcp-go
```
