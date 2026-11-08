# paprika-mcp-go

An [MCP](https://modelcontextprotocol.io) interface that lets an AI agent work with the
Paprika 3 recipe ecosystem, including recipes, meal plans, and grocery lists.

**Status:** in planning. The API research is done, and the server is an early skeleton.
See the [design spec](docs/mcp-server-spec.md).

## Repository Contents

```
src/services/paprika-mcp-go/   The MCP server
src/tools/paprika-api-probe/   Local tool for checking our API docs; never deployed
src/tools/ssm-secrets/         Local tool that writes production secrets to AWS SSM; never deployed
docs/paprika-api/              Unofficial Paprika API reference and research notes
docs/mcp-server-spec.md        Design of the MCP server
docs/                          Other architecture and development guides
infra/k8s/                     Kubernetes manifests (Kustomize)
.github/workflows/             CI and image publishing
```

To contribute, read [CONTRIBUTING.md](CONTRIBUTING.md). AI coding assistants should start
with [AGENTS.md](AGENTS.md).

## Purpose and Credits

This is a personal project to enhance my home ecosystem and agentic tooling. The reverse
engineering behind it was not done to resell anything or to make any money.

Paprika's API is unofficial and undocumented, and this project is not affiliated with
Hindsight Labs, LLC. Our research started from
[kappari](https://github.com/johnwbyrd/kappari), and we built from and upon its findings.
This repository does not include kappari's AGPL-licensed code or text.

Licensed under the [MIT License](LICENSE).
