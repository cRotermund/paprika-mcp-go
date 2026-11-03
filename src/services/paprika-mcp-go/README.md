# paprika-mcp-go

MCP server for Paprika recipe data, served over streamable HTTP at `/mcp` (or stdio with `-stdio`). It reads recipes, meal plans, and grocery lists from your Paprika account, and can change them when writes are enabled.

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
| `PORT` | HTTP mode | TCP port the HTTP server listens on. |
| `LOG_LEVEL` | no | `DEBUG`, `INFO` (default), `WARN`, or `ERROR`. |
| `MCP_TOKEN_SHA256` | HTTP mode | One or two comma-separated SHA-256 hashes (64 hex characters each) of the bearer token. The server refuses to start without it; there is no way to disable authentication over HTTP. Two hashes allow a token rotation. Not used with `-stdio`. |
| `MCP_ALLOWED_ORIGINS` | no | Comma-separated extra `Origin` values (such as `https://example.com`) accepted on `/mcp`. Requests with no `Origin` header are always allowed. Any other `Origin` is rejected with 403. |
| `PAPRIKA_EMAIL`, `PAPRIKA_PASSWORD` | yes | The Paprika account. Required in both HTTP and `-stdio` modes. Secrets: never logged or returned. Use a separate test account while developing. |
| `PAPRIKA_WRITES_ENABLED` | no | `true` or `false` (default). The write kill switch: write tools are registered only when this is `true`. |
| `INDEX_STORE` | no | Where to keep the recipe index snapshot: `none` (default), `filesystem`, or `s3`. An unknown mode, or a mode missing its settings, stops startup. |
| `INDEX_FILE_PATH` | `filesystem` | Snapshot file path, on a writable volume. |
| `INDEX_S3_BUCKET`, `INDEX_S3_KEY`, `INDEX_S3_ENDPOINT` | `s3` | Bucket (required), object key (default `paprika-mcp-go/recipe-index.json.gz`), and an endpoint override for an S3 emulator. AWS credentials and region come from the SDK's default chain. |

## Routes

- `GET /health` returns `{"status":"healthy"}` and backs the Kubernetes probes.
- `/mcp` serves MCP over streamable HTTP (stateless, JSON responses) and requires `Authorization: Bearer <token>`. `/health` does not depend on Paprika.
- Every other path returns 404.

In `-stdio` mode there is no authentication, because only the local user can reach the process.

## Authentication

Clients send a bearer token. The server stores only its SHA-256 hash. Generate a token and the hash to configure:

```sh
TOKEN=$(openssl rand -hex 32)                       # give this to the MCP client
printf %s "$TOKEN" | sha256sum | cut -d" " -f1      # set this as MCP_TOKEN_SHA256
```

On macOS use `shasum -a 256` in place of `sha256sum`. Hash the token exactly as the client will send it, with no trailing newline (`printf %s` and `openssl rand` output via `$(...)` both avoid one).

To rotate, set `MCP_TOKEN_SHA256` to `<old-hash>,<new-hash>`, roll the server, switch clients to the new token, then drop the old hash.

The token and the `Authorization` header are never logged. A failed authentication logs only the client IP and the reason.

## Request limits

Checks run on `/mcp` in this order:

1. `Origin` validation.
2. Failed-authentication limit: 10 failures per minute per client IP, after which that IP gets 429 until the minute ends. The client IP is the rightmost `X-Forwarded-For` hop (the one the load balancer appends), else the socket address, so the service must only be reachable through the load balancer.
3. Bearer-token authentication.
4. Global limit for authenticated requests: 10 per second with bursts of 20.
5. 1 MiB request body limit and a 90 second request timeout (each Paprika call has its own 20 second timeout).

## Tools

The read tools are always available. List tools take `limit` (default 25, at most 100) and `cursor` (the `next_cursor` of the previous page).

| Tool | Purpose |
| --- | --- |
| `ping` | Health check. |
| `search_recipes` | Search by words in the name, category, source, and ingredients; filter by `category`, `favorite`, `min_rating`, `include_trashed`. |
| `get_recipe` | One full recipe by `uid`, including its `hash`. Recipe text is untrusted data. |
| `list_categories` | The category tree, flattened depth first. |
| `list_meals` | Meals between `start_date` and `end_date` (both required, `YYYY-MM-DD`). |
| `list_meal_types` | The account's meal types. An account with none gets the four built-in types, flagged `builtin`. |
| `list_grocery_lists` | Lists with item counts. |
| `list_grocery_items` | Items of one list (default list if omitted); purchased items need `include_purchased`. |
| `list_grocery_aisles` | Aisle names. |

### Write tools

These exist only when `PAPRIKA_WRITES_ENABLED=true`; otherwise they are not registered at all. Every write runs under one process-wide lock, re-reads the current object from Paprika, changes only what was asked for, keeps all other fields, sends the whole object, and then asks Paprika to notify other devices. A timeout on a write is reported as an unknown outcome and is never retried. Each write logs an audit line with the tool, resource, `uid`, field names, and outcome, but no values.

| Tool | Purpose | Hints |
| --- | --- | --- |
| `create_recipe` | New recipe; categories by name or `uid`. | write |
| `update_recipe` | Change only the given fields; optional `expected_hash` refuses the update if the recipe changed. `categories` replaces the whole set. | write, idempotent |
| `trash_recipe` | Move to the Paprika trash (no permanent delete). | destructive |
| `restore_recipe` | Take out of the trash. | write, idempotent |
| `add_meals` | 1 to 50 meals in one POST: `date`, `meal_type` name, and `recipe_uid` or `name`. | write |
| `update_meal` | Move, retype, rename, or re-link a meal. | write, idempotent |
| `delete_meal` | Remove a meal. May not be undoable; the result holds the meal as it was. | destructive |
| `add_grocery_items` | 1 to 50 items in one POST, to a list (default list if omitted). | write |
| `update_grocery_item` | Rename, move to an aisle, or check off. | write, idempotent |
| `delete_grocery_item` | Remove an item; the result holds the item as it was. | destructive |
| `create_grocery_list` | New empty list. | write |
| `delete_grocery_list` | Remove an empty list; the default list and lists with items are refused. | destructive |

Batch tools report per-item failures and skip those items. Meal types come from the account; an account with none accepts Breakfast, Lunch, Dinner, and Snacks and sends only the numeric type.

Tool errors are tool results with `isError` set and text that says what to do next.

## Recipe index

Paprika has no search API, so the server keeps a small index entry per recipe (no directions or notes) and about 50 recent recipe bodies in memory. At startup it loads the snapshot if `INDEX_STORE` has one, compares it with Paprika's recipe list, and reads only what changed, in the background. Without a snapshot the first start reads every recipe at about four per second, and `search_recipes` answers "still loading" with progress until that ends; `get_recipe` works meanwhile. Reads check Paprika's change counters at most once a minute. A snapshot older than 30 days since the last full read is served but every recipe is read again in the background. Details are in [docs/mcp-server-spec.md](../../../docs/mcp-server-spec.md).

On Linux and macOS the filesystem-mode snapshot file is created with mode `0600`. On Windows, file modes do not apply, so the file inherits the access list of its folder; keep `INDEX_FILE_PATH` in a folder only you can read.

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
