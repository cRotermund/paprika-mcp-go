# MCP Server Specification

Status: draft for review, 2026-11-01. Only the `ping` tool, the `/mcp` and `/health`
routes, `-stdio` mode, and basic configuration exist so far.

The upstream protocol is documented in [docs/paprika-api](paprika-api/README.md).
This document covers what we build on top of it.

## Goals

Let one person read and write their Paprika **recipes, meal plans, and grocery lists**
from MCP clients, through a server reachable from the public internet.

In priority order:

1. **Do not lose or corrupt Paprika data.** Paprika offers no test environment, so we test
   on a separate account of our own, and production writes go to the real account.
2. **Do not expose the account.** Anyone who reaches the endpoint must be stopped
   before they reach a tool.
3. **Be useful to a model:** clear tools, compact results, and errors it can act on.

## Non-Goals

These are out of scope for the first release:

- Multiple users or accounts
- Photos, except preserving existing photo fields on update
- Pantry, bookmarks, menus, and menu items
- Permanently deleting recipes
- Importing recipes from a URL
- Real-time updates over Paprika's WebSocket
- Hosting our own OAuth authorization server

## Architecture

```
MCP client (Claude, Claude Code)
    |  HTTPS, bearer token
Existing AWS load balancer  (TLS termination)
    |
paprika-mcp-go  (one replica)
    /mcp      failed-auth limit -> auth -> global limit -> MCP handler -> tools
    /health   no auth, no account data
    |
    tools          inputs, outputs, annotations
    service        business rules, validation, write sequence
    mirror         recipe index, body cache, small lists, search, change detection
    indexstore     optional index snapshot: none, filesystem, or S3
    paprikaclient  login, requests, gzip multipart, pacing
    |  HTTPS, Paprika token
www.paprikaapp.com
```

### Packages

All under `src/services/paprika-mcp-go/internal/`:

| Package | Responsibility |
| --- | --- |
| `paprikaclient` | HTTP client. Knows routes, encoding, login, and errors. Holds no business rules. |
| `mirror` | Recipe index, body cache, small lists, and search. Refreshes from status counters. |
| `indexstore` | Loads and saves the index snapshot. Knows nothing about Paprika. |
| `service` | Reads from the mirror. Runs the write sequence. Validates references. |
| `mcpserver` | Registers tools and maps service results and errors to MCP. |
| `auth` | Verifies inbound bearer tokens. |
| `config` | Environment variables. |

Only `paprikaclient` calls the Paprika API. Only `mcpserver` knows about MCP.

### Transport

- Streamable HTTP at `/mcp`, using the SDK's `Stateless` and `JSONResponse` options.
  Every tool is a single request and response, so there are no sessions to lose on a
  restart and no long-lived streams to hit the load balancer's idle timeout.
- `-stdio` stays available for local use. It has no inbound auth, because only the
  local user can reach it.

### One Replica

Run exactly **one replica with the `Recreate` deployment strategy**. The write lock and
the mirror live in process, so two replicas could interleave writes. The server has no
need for high availability. The production overlay currently sets two replicas, so this
must change.

## Paprika Client

- **Login:** v1 email and password login, sent only when there is no token or after a 401.
- **Token:** held in memory only. A restart logs in again.
- **Retry on 401:** log in once and repeat the request once.
- **Login failures:** after a failed login, wait with exponential backoff, from 1 minute
  up to 1 hour, so a bad password cannot lock the account or look like an attack.
  Concurrent callers share a single login attempt.
- **Pacing:** one request at a time across the process, with at least 250 ms between
  requests, or at most 240 per minute. This stays under the one unverified report of a
  block at about 300 connections per minute.
- **Timeouts:** 30 seconds per request. After repeated timeouts, pause for 5 minutes,
  because a rate block looks like a timeout.
- **Errors:** always check for an `error` object, including on HTTP 200.
- **`User-Agent`:** an honest `paprika-mcp-go/<version>`.
- **Responses:** capped at a fixed size, to protect the process.
- **Redirects:** not followed, so the token cannot reach another host.

## Local Mirror

The API has no search and no filters, so the server needs its own view of the data. It
does not need every recipe body in memory.

| Data | Held in memory | Source of a miss |
| --- | --- | --- |
| Recipe index | One small entry per recipe | Full recipe read, once per changed `hash` |
| Recipe bodies | About the 50 most recently used, as raw JSON | `sync/recipe/{uid}/` |
| Categories, meals, meal types, grocery lists, grocery items, aisles, remembered ingredients | Whole lists | One bulk request |

- **Index entry:** `uid`, `hash`, name, category `uid` values, rating, favorite flag, trash
  flag, source, time fields, and a deduplicated set of ingredient search tokens. Entries
  hold no directions, notes, nutrition, or photo fields.
- **Body cache:** an entry is valid only if its `hash` matches the index, so a changed
  recipe can never be served from it. Writes never use it, because the
  [write sequence](#write-sequence) re-reads from the API. That read also keeps fields this
  server does not model.
- **Freshness:** before serving a read, check `sync/status/` if the last check is more than
  60 seconds old. Re-download only resources whose counter changed.
- **Recipes:** compare each `hash` from `sync/recipes/`, and fetch only the recipes that
  changed. Removed ones leave the index. Compare hashes as opaque strings.
- **Search:** case-insensitive token matching over name, categories, source, and
  ingredients, with name matches ranked first. Words that appear only in directions or
  notes are not searchable. This needs no new dependency.
- **`/health` is independent of the mirror and of Paprika,** so a Paprika outage does not
  restart the pod.

### Startup

The API returns no names without reading each recipe, so an empty index means reading every
recipe once: about 75 seconds for 300 recipes, or 21 minutes for 5,000, at the pacing above.
An [index snapshot](#index-snapshot) avoids most of this.

1. Load the snapshot, if there is one. Search works from this point.
2. Read `sync/recipes/` once, and compare every `uid` and `hash` with the index.
3. In the background, fetch new and changed recipes, and remove the ones that are gone.

Without a snapshot, recipe search returns a retryable "still loading" error with progress
until step 3 finishes. `get_recipe` reads directly from the API until then.

### Index Snapshot

The snapshot is a gzip-compressed JSON file holding the index, a format version, and the
time it was saved. It is only an optimization, and the comparison at startup corrects it.
Only the recipe index is saved, because the other resources take one request each to reload.

`INDEX_STORE` selects where it lives. Nothing about the mirror changes between modes.

| Mode | Behavior | Intended for |
| --- | --- | --- |
| `none` | No snapshot. Every start is a full load. **Default.** | Trying the server, or small libraries |
| `filesystem` | A file at `INDEX_FILE_PATH`, created with mode `0600`. Saved to a temporary file in the same directory, synced, then renamed over the old one. | Local development, or a mounted volume |
| `s3` | One object at `INDEX_S3_BUCKET` and `INDEX_S3_KEY`, replaced by each PUT. | Deployments with S3 available, with no volume tied to a node or zone |

Rules for every mode:

- **Interface:** `Load` and `Save` only. An absent snapshot is a normal result, not an error.
- **Saving:** in the background, only after the index changes, and at most every 5 minutes.
  The server also tries once during a graceful shutdown.
- **Failed saves** are logged and retried at the next interval. They never fail a tool call.
- **Unusable snapshots** that are corrupt, unreadable, or in another format version are logged
  and ignored, and the server does a full load. Adding a field to the index therefore only
  costs one full load.
- **Expiry:** the comparison at startup assumes every edit changes a recipe's `hash`. We
  captured this from the Windows client only, and a missed edit would otherwise survive
  restarts. A snapshot more than 30 days old still serves searches, but every recipe is read
  again in the background.
- **Bad configuration stops startup:** an unknown mode, or a mode missing its required
  settings, is a startup error, so that a typo cannot silently cause full loads.
- **S3 client:** uses `aws-sdk-go-v2` and its default credential chain, so a deployment can
  supply a role or keys without code changes. `INDEX_S3_ENDPOINT` points it at an emulator.

## Write Sequence

Every write follows the same steps, one at a time under a process-wide lock:

1. **Validate input** before any network call.
2. **Read the current object from the API,** not the mirror. For bulk resources this means
   downloading the list and finding the `uid`.
3. **Apply only the requested changes** to the raw object.
4. **Validate references** against current data.
5. **Generate values:** an uppercase UUID for a new `uid`, as the official client does, and a
   new `hash` for any recipe.
6. **POST only the changed objects.** Bulk routes receive an array, and recipes go one at a
   time.
7. **Call `sync/notify/`** so other devices pick up the change.
8. **Update the mirror,** including the index entry and body cache, and return the resulting
   object.
9. **Write an audit log entry.**

### Rules

| Rule | Reason |
| --- | --- |
| Always send the whole object | Omitted fields may be blanked |
| Treat `uid` as an opaque string | Live data has non-UUID values |
| Never retry a POST automatically after a timeout | The outcome is unknown. Report that and let the caller re-read. |
| Verify a grocery `list_uid` exists | Another device may mark the item deleted if it does not |
| Verify `recipe_uid` and `type_uid` on meals | Other devices may silently change them |
| Keep `categories` complete on recipe updates | A partial list removes the others |
| Do not touch photo fields | Photos are out of scope |

### Conflicts

The official client does no conflict detection, so the last write should win unless the
server rejects stale writes, which [check W4](#write-checks) will test. To narrow the risk, `update_recipe` accepts
an optional `expected_hash`, and fails if the current hash differs. `get_recipe` returns
the hash so a client can pass it back. Meals and grocery items have no hash, so they rely
on the re-read in step 2 only.

### Deletes

| Resource | Behavior |
| --- | --- |
| Recipes | `trash_recipe` sets `in_trash`. `restore_recipe` clears it. No permanent delete. |
| Meals and grocery items | Sends `deleted: true`. Assume this cannot be undone unless check W6 shows otherwise. The tool result includes the full object from before the delete, so it can be re-created as a new object. |
| Grocery lists | Refuses the default list and any list that still has items. |

## Tools

All list tools accept `limit` and `cursor`, and default to compact summaries. Inputs and
outputs use typed schemas.

### Recipes

| Tool | Annotation | Purpose |
| --- | --- | --- |
| `search_recipes` | read-only | Query with filters for category, favorite, minimum rating, and `include_trashed`. Trashed recipes are hidden by default. |
| `get_recipe` | read-only | Full recipe, including `hash` |
| `list_categories` | read-only | Category tree |
| `create_recipe` | write | New recipe. Category names are resolved to `uid` values. |
| `update_recipe` | write | Changes only the given fields. Optional `expected_hash`. |
| `trash_recipe` | destructive | Moves to trash |
| `restore_recipe` | write | Removes from trash |

### Meal Plans

| Tool | Annotation | Purpose |
| --- | --- | --- |
| `list_meals` | read-only | By `start_date` and `end_date`, which are required |
| `list_meal_types` | read-only | Names and `uid` values for the account |
| `add_meals` | write | One or more meals, each with a date, a meal type by name, and either `recipe_uid` or `name` |
| `update_meal` | write | Move, rename, or re-link |
| `delete_meal` | destructive | Removes a planned meal |

Meal types are resolved from the account's own list, never from hard-coded numbers. Dates
are `YYYY-MM-DD` in tool inputs, and are stored as midnight. Paprika dates have no time
zone, so none is applied.

### Grocery Lists

| Tool | Annotation | Purpose |
| --- | --- | --- |
| `list_grocery_lists` | read-only | Lists, with item counts |
| `list_grocery_items` | read-only | By list, with `include_purchased`. Purchased items are hidden by default. |
| `list_grocery_aisles` | read-only | Aisle names |
| `add_grocery_items` | write | One or more items. Defaults to the default list. |
| `update_grocery_item` | write | Rename, move aisle, or check off |
| `delete_grocery_item` | destructive | Removes an item |
| `create_grocery_list` | write | New list |
| `delete_grocery_list` | destructive | Removes an empty, non-default list |

Item text goes in both `name` and `ingredient`, with empty parsed fields, unless the
caller provides them. This matches what the app sends. Aisles are resolved in this order:
the given aisle name, then the remembered ingredient, then the fallback chosen by
[check W5](#write-checks).

### Batch Writes

`add_meals` and `add_grocery_items` accept at most 50 items. They send one POST per batch
if [check W9](#write-checks) passes, and one POST per item otherwise. Results name any item
that failed, and report a timeout as an unknown outcome.

### Annotations and Untrusted Content

Tools set `readOnlyHint` and `destructiveHint` so clients can ask before writing.

Recipe text often comes from the web, and any of it can contain instructions aimed at a
model. Results mark these fields as data in their descriptions, and clients should keep
confirmation on for write tools. Server-side limits in this document apply regardless of
what a model is told.

## Security

### Threat Model

The endpoint is public and holds a path to a real account. We defend against:

| Threat | Defense |
| --- | --- |
| Unauthenticated internet traffic | Bearer token required before any MCP processing |
| Stolen MCP token | Rotation, no Paprika access beyond the tools, no permanent recipe delete, audit log, write kill switch |
| Token guessing | 256-bit random token, rate limiting |
| Paprika credential leak | Stored only in the secret store, never logged, never returned, never sent anywhere else |
| Prompt injection through recipe text | Annotations, client confirmation, narrow tools, no generic API tool |
| Browser-based attacks on `/mcp` | `Origin` validation, no cookies, bearer tokens only |
| Oversized or abusive requests | Body limits, rate limits, batch limits, timeouts |
| Exposed index snapshot | Holds only index fields and no credentials. A private bucket with default encryption and an IAM policy limited to one object, or a `0600` file. |

### Inbound Authentication

**Decision: use a generated static bearer token. OAuth may be revisited later.**

| Option | Assessment |
| --- | --- |
| Static bearer token | Simple and auditable for one person. Claude connectors support fixed request headers, and so does Claude Code. Long-lived, so it must be rotated manually. **Chosen.** |
| OAuth 2.1 with an external identity provider | Better: short-lived tokens and revocable sign-in. Requires a provider. **Deferred.** |
| Our own authorization server | Too much security-sensitive code to own in a recipe tool. **Rejected.** |

Implementation:

- Use the SDK's `auth.RequireBearerToken` with a verifier, so OAuth can later replace the
  verifier without touching the tools.
- The server stores **a SHA-256 hash of the token,** not the token, and compares in
  constant time. Generate at least 32 random bytes.
- `MCP_TOKEN_SHA256` accepts up to two comma-separated hashes, so a token can be rotated
  without downtime.
- The MCP token is never forwarded to Paprika, and the Paprika token is never returned to
  a client. They are unrelated credentials.
- The server refuses to start in HTTP mode without a configured token. There is no
  "auth disabled" setting.

If OAuth is added, the verifier must check signature, issuer, audience, expiry, and an
allow-listed subject, and the server must publish protected resource metadata with the
SDK's `ProtectedResourceMetadataHandler`.

### Write Kill Switch

`PAPRIKA_WRITES_ENABLED` defaults to **false**. When it is false, write tools are not
registered at all, so a read-only deployment has no write path.

### Request Handling

- **Order:** authentication runs first, before request bodies are parsed.
- **Rate limits:** failed authentication is limited per client IP, and successful requests
  are limited globally. The client IP comes from the load balancer's forwarding header,
  which is trusted only because the service is reachable solely through it.
- **Limits:** 1 MiB request bodies, and per-request timeouts.
- **`Origin`:** a request with an unknown `Origin` header is rejected. Requests with none,
  which is normal for server-side clients, are allowed.
- **Routes:** only `/mcp` and `/health` exist. `/health` returns a fixed status and no
  details.

### Logging and Audit

- **Never logged:** passwords, tokens, `Authorization` headers, and request or response
  bodies.
- **Every write logs** the tool, resource, `uid`, field names changed, and outcome, but no
  field values. Recipe text and account email stay out of logs.
- **Every failed authentication logs** the client IP and reason.
- Tool errors returned to clients do not include internal details or upstream bodies.

### Deployment Hardening

- Distroless non-root image, as the repository's Dockerfile already does.
- A read-only root filesystem, with all capabilities dropped, as the base Deployment already does.
- Paprika credentials and the token hash come from an `ExternalSecret`, with secret values
  outside Git.
- Consider restricting the load balancer's listener or adding AWS WAF rate rules. This
  belongs to the homelab infrastructure, so it is outside this repository.

## Configuration

| Variable | Required | Secret | Default | Description |
| --- | --- | --- | --- | --- |
| `PORT` | yes | no | | HTTP port |
| `LOG_LEVEL` | no | no | `INFO` | Log level |
| `PAPRIKA_EMAIL` | yes | yes | | Paprika account |
| `PAPRIKA_PASSWORD` | yes | yes | | Paprika password |
| `MCP_TOKEN_SHA256` | HTTP only | yes | | One or two token hashes |
| `PAPRIKA_WRITES_ENABLED` | no | no | `false` | Registers write tools |
| `MCP_ALLOWED_ORIGINS` | no | no | empty | Extra allowed `Origin` values |
| `INDEX_STORE` | no | no | `none` | Snapshot mode: `none`, `filesystem`, or `s3` |
| `INDEX_FILE_PATH` | `filesystem` only | no | | Snapshot file path, on a writable volume |
| `INDEX_S3_BUCKET` | `s3` only | no | | Snapshot bucket |
| `INDEX_S3_KEY` | no | no | `paprika-mcp-go/recipe-index.json.gz` | Snapshot object key |
| `INDEX_S3_ENDPOINT` | no | no | AWS | Endpoint override for an S3 emulator |
| `AWS_REGION` and other AWS SDK settings | `s3` only | if keys are used | | Read by the SDK's default chain |

The root filesystem is read-only, so `filesystem` mode needs a mounted writable volume.

The current memory limit of 128 MiB should be enough now that recipe bodies are not all held
in memory. Measure this at M3, including with a synthetic library of 5,000 recipes.

## Errors

Tool errors state what happened and what to do next:

| Situation | Tool result | Retry guidance |
| --- | --- | --- |
| Mirror still loading | Progress message | Retry shortly |
| Unknown `uid` or reference | Names the bad value | Look it up first |
| `expected_hash` mismatch | Reports the conflict | Re-read and re-apply |
| Paprika unavailable or rate blocked | Temporary failure | Retry later |
| Paprika login failing | Server configuration problem, with no detail | Operator must act |
| Write timeout | Outcome unknown | Re-read before retrying |
| Writes disabled | Tools are absent | None |

## Write Checks

The [API docs](paprika-api/notes/open-questions.md) leave questions that only our own code can
answer. Each check below is a **gate for the write tools**, run against throwaway
`MCP-TEST` objects on the [test account](#test-account) and confirmed in the Paprika app.

These run from a new `-write-checks` mode in `paprika-api-probe`, which stays read-only
unless that flag is given. In that mode it only sends POST requests for objects that it
created itself, with names starting `MCP-TEST`. It also refuses to run unless both of these
hold, so that a wrong `.env` cannot reach the real account:

- The `-confirm-account` flag repeats the configured email exactly.
- The account holds no more than 25 recipes, unless `-allow-real-account` is also given.

**First run:** the account owner chose to run these once on the real account, on 2026-11-02,
using test objects only. Server results are in the
[API docs](paprika-api/notes/open-questions.md#write-questions). All later regression runs use
the test account.

| Check | Server result | Decision |
| --- | --- | --- |
| W1 | Accepted | Use the standard library and our own `User-Agent` |
| W2 | Omitted fields were kept | Keep the whole-object rule, because this is undocumented |
| W3 | Newer fields were accepted and kept | Round-trip all fields |
| W4 | Stale writes were accepted | Last write wins, so keep `expected_hash` |
| W5 | Empty and unknown aisles were stored, and a phone showed both under Miscellaneous | Resolve to an existing aisle ourselves, because the phone rewrote the unknown one |
| W6 | Restoring works on the server, and a phone that had synced the delete showed the item again | Undo is possible for grocery items. Not added to the first release's tools until a meal is tested the same way |
| W7 | `\n` and `\r\n` are both stored as sent, and both displayed as line breaks on a phone | Send `\n`, and check display on the Windows client with the test account |
| W8 | `sync/notify/` accepted, and the writes reached a phone | Keep the sequence. We did not separate this from the phone's own sync |
| W9 | Applied in full | Send batches in one POST |

| # | Check | If it passes | If it fails |
| --- | --- | --- | --- |
| W1 | Go's standard multipart format and our `User-Agent` are accepted on writes | Use the standard library | Hand-build the framing, then try a client-style `User-Agent` only if needed |
| W2 | Omitting a field from an upload | Learn whether it is kept or blanked | The whole-object rule covers both outcomes |
| W3 | The server accepts and keeps newer recipe fields such as `servings_min` | Round-trip all fields | Send the 3.3.1 fields only, and document possible loss |
| W4 | A stale recipe write is accepted | Confirms last write wins, as designed | Handle the server's rejection as a conflict |
| W5 | An item with an empty or unknown `aisle_uid` | Choose the aisle fallback | Require an existing aisle |
| W6 | Re-posting a `deleted` object with `deleted: false` | Add undo for meals and items | Deleted stays final |
| W7 | Line breaks sent as `\n` display properly (the Windows client sends `\r\n`) | Send `\n` | Send `\r\n` |
| W8 | A write with `sync/notify/` reaches another device | Keep the sequence | Investigate before release |
| W9 | A bulk POST with several objects is applied in full (one-item arrays are already confirmed) | Send batches in one POST | Send one POST per item |

## Testing

The layers below answer different questions. The fake shows that our code matches our
understanding of Paprika. The captured formats and the test account show whether that
understanding is right.

### Test Account

A separate Paprika account, used only for testing and holding only synthetic data, is the one
real backend we test against.

- **Used for:** W1 to W9, manual live runs, and checking each write tool in the app.
- **Credentials:** kept only in a gitignored local file, separate from any real account's. The
  address and password never enter the repository or CI.
- **Not assumed:** that it behaves exactly like an older account with years of data. Live probe
  results from the real account remain the evidence for real data shapes.
- **Pacing:** test tools use the server's 250 ms pacing, because rate limits might apply per IP
  and affect the real account too.
- **No load tests, ever.** Nothing in this project stress-tests Paprika's servers or probes for
  rate limits, on any account. Scale tests use the fake server.
- **Keep the accounts apart in the app:** W8 and the app checks need Paprika signed in to the
  test account. Do this on a device or profile with no real data, because we have not tested
  what the app does with local recipes when it switches accounts.

### Test Layers

- **Unit tests** cover each package, in the repository's table-driven style.
- **A fake Paprika server** built with `httptest` serves synthetic data and records every
  request. Tests assert gzip, multipart, the whole-object rule, new hashes, `notify` calls,
  and that write tools are absent when disabled. It also produces errors on HTTP 200, 401s,
  503s, and timeouts.
- **Fake behavior follows the write checks.** It accepts stale writes and unknown references,
  and keeps omitted fields, as the real server did. Code must still be safe if an omitted field
  is blanked, because that behavior is undocumented.
- **Format tests** compare our requests with synthetic fixtures shaped like the captured client
  traffic, covering structure, field set, and encoding.
- **Index store tests** run one shared table of cases against `filesystem` and `s3`: absent,
  round trip, overwrite, corrupt data, an old format version, and a snapshot of 5,000 entries.
  `filesystem` uses a temporary directory.
- **S3 protocol tests** run the real `aws-sdk-go-v2` client over HTTP against
  [SeaweedFS](https://github.com/seaweedfs/seaweedfs), with authentication turned on, so that
  request signing, checksums, and S3 error codes are all exercised. They also cover a missing
  bucket and wrong credentials.
  - They live beside the code in `internal/indexstore`, because Go does not allow `tests/` to
    import an `internal` package. An `integration` build tag selects them, and they fail rather
    than skip if the emulator is unreachable.
  - A separate CI job starts a pinned SeaweedFS image and runs them on every pull request.
    Contributors can run the same image locally.
- **Failure injection** uses a small `httptest` server for what an emulator cannot easily
  produce, such as timeouts and 500 responses during a save.
- **Real bucket check:** before M3 is done, the same S3 tests run once against the production
  bucket with a throwaway key. Emulator differences remain, such as path-style addressing, which
  is used only when `INDEX_S3_ENDPOINT` is set. IAM policy and bucket encryption can only be
  proven against AWS.
- **Tool tests** use the SDK's in-memory transports, as `TestPing` already does.
- **Auth tests** cover missing, wrong, and rotated tokens, and that no tool runs unauthenticated.
- **Log tests** assert secrets and recipe text never appear in log output.
- **Fixtures are synthetic.** Nothing captured from a real account enters the repository.
- **CI never contacts Paprika.** Live checks run by hand, against the test account.
- **Regression testing never uses the real account.** Apart from the one first run of the write
  checks, its next writes come at M6, through ordinary use.

## Delivery Plan

Each milestone is one or more pull requests, and is independently useful.

| # | Milestone | Done when |
| --- | --- | --- |
| M1 | Inbound auth, limits, and hardening | `/mcp` rejects unauthenticated requests, with `ping` still working when authenticated |
| M2 | Paprika client, read side | Login, reads, pacing, and retries pass against the fake server and one manual run on the test account |
| M3 | Mirror, index store, and read tools | All three snapshot modes pass their tests, and all read tools work against the test account, deployed read-only, with the production overlay changed to one replica and `Recreate` |
| M4 | Write checks | W1 to W9 are complete, including app confirmation, and repeatable on the test account, the API docs and this spec are updated with results, and the probe's docs describe its new mode, with GET-only remaining the default |
| M5 | Write tools | Recipes, then meals, then groceries, each checked in the app on the test account with `MCP-TEST` objects |
| M6 | Production release | Switched to the real account read-only and checked, a fresh export of meal plans and grocery lists saved to a private local file, then writes enabled, token rotated, and audit logs reviewed after first use |

**Deploying read-only at M3 comes first on purpose.** It proves auth, hosting, and the client
before any path to changing account data exists.

## Risks

| Risk | Impact | Response |
| --- | --- | --- |
| Paprika changes its unofficial API | Tools break, or writes misbehave | Errors surface clearly, the kill switch stops writes, and the probe can recheck the API |
| Paprika's terms restrict this use | Account or legal exposure | **Read the terms before the repository goes public.** They have not been reviewed. |
| A newer app stores data in fields 3.3.1 lacks | Possible data loss on update | Raw JSON round trip from a fresh read, gated by W3 |
| Another client edits a recipe without changing its `hash` | Stale search results or cached body | Snapshot expiry after 30 days. Writes are unaffected, because they re-read from the API. |
| A bad write reaches every device | Data damage | One object per write, no permanent recipe delete, and recipe backups |
| A model acts on injected instructions | Unwanted edits | Narrow tools, client confirmation, audit log |

## Open Decisions

1. **Secret store:** the name and kind of the store for the `ExternalSecret`, plus the
   installed External Secrets Operator version.
2. **Hostname:** the public hostname and path for `MCP_ALLOWED_ORIGINS` and the ingress, and
   whether an ingress belongs in this repository's overlay or your infrastructure.
3. **Production snapshot:** the bucket name, and how the pod gets AWS credentials.
