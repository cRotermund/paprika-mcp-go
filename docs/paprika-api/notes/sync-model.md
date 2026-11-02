# Sync Model

See the [evidence labels](../README.md#evidence-labels). The behavior described
here is what the client does (Client) unless marked otherwise. The server may
allow more or less than this.

## Routes

| Purpose | Request |
| --- | --- |
| Change counters | `GET sync/status/` |
| List a resource | `GET sync/{resource}/` |
| Write objects in bulk | `POST sync/{resource}/` |
| Read one recipe | `GET sync/recipe/{uid}/` |
| Write one recipe | `POST sync/recipe/{uid}/` |
| Tell other devices to sync | `POST sync/notify/` with no body |

All GET routes in the table below are confirmed. (Live) POST is confirmed for
`groceries`, `grocerylists`, `meals`, `recipe/{uid}`, and `notify`; the others come
from the client. (Capture, Client) The client uses the same name for the route and
the status counter. (Client, Live)

| Resource | Purpose |
| --- | --- |
| `recipes` | Recipe `uid` and `hash` list |
| `categories` | Recipe categories |
| `meals` | Calendar meal plan entries |
| `mealtypes` | Breakfast, lunch, and so on |
| `menus`, `menuitems` | Reusable menu templates |
| `groceries` | Grocery list items |
| `grocerylists` | Grocery lists |
| `groceryaisles` | Store aisles |
| `groceryingredients` | Remembered ingredient-to-aisle choices |
| `pantry`, `bookmarks`, `photos` | Out of scope for now; the [OpenAPI reference](../openapi.yaml) lists the photo fields |

`sync/status/` also has counters for `cookbooks`, `pantrylocations`, and
`recipesearches`, but those GET routes return 404 under this path. (Live) They and
several recipe fields suggest the API has features newer than Windows 3.3.1.
(Inferred)

## Objects

Objects in lists do not include `deleted`. Apart from recipes and photos, they do
not include `hash` either. (Live)

### Identifiers

Clients generate `uid`, not the server, so creating an object means choosing a new
`uid` and posting it. (Client)

**Do not assume a `uid` is a UUID.** Live data has three forms: (Live)

- An uppercase UUID, the most common form
- A 64-character uppercase hex value, on all sampled aisles and meal types, and on
  one grocery list
- A longer string of up to 59 characters, on some recipes, meals, and remembered
  ingredients

Treat a `uid` as an opaque string. Use an uppercase UUID for new objects, as the
client does. (Client)

## Sync Order

For each resource, the client:

1. Reads `sync/status/` once, at the start of the run.
2. Skips the download if the stored counter matches the server's. Only one of the
   client's sync types does this, probably its routine sync (Inferred); the others
   always download.
3. Downloads the list and merges it, skipping objects with local changes.
4. Removes local unmodified objects that are absent from the server's list.
5. Uploads its local changes.

After all resources, the client calls `sync/notify/`, but only if something was
uploaded and the run was not canceled.

Grocery and meal objects reference other objects, so dependency order matters:
grocery lists and aisles come before grocery items, and categories come before
recipes. Reference fields are applied in a second pass. If a referenced object
is unknown locally, the client recovers by itself. See
[grocery lists](grocery-lists.md) and [meal plans](meal-plans.md).

## Bulk Writes

Bulk POST bodies contain **only the objects that changed**, as a JSON array.

**Bulk writes add or update by `uid`; they do not replace the resource.** One-item
arrays were posted to `groceries` and `meals`, and the item counts of those resources
were unchanged once the test objects were deleted. (Capture, Live)

A successful write returns `{ "result": true }`. (Capture) Each object is sent whole;
moving a meal re-sends the same `uid` with a new `date`. (Capture)

Recipes are the exception: one POST per recipe.

After a write, the client refreshes every resource list on its next sync, without
re-reading the recipe it just uploaded. (Capture)

## Deletes

The client deletes in one of two ways:

- **Never synced:** removes it locally. The server never hears about it.
- **Already synced:** marks it `deleted`, uploads it with `"deleted": true`, then
  removes the local copy. A recipe also gets a new `hash`.

The server stops listing deleted objects: a recipe, a meal, and a grocery item posted
with `"deleted": true` no longer counted in later lists, and no listed object has a
`deleted` key. (Capture, Live)

Re-posting a deleted grocery item or meal with `"deleted": false` lists it again. A phone
that had already synced a grocery item's delete showed the item again. (Live) How long the
server retains deleted objects is unknown.
Recipes also have a separate, recoverable trash. See [recipes](recipes.md#delete).

## Hashes

An object's `hash` is a **change marker, not a content checksum**. The client
produces it by computing SHA-256 of a random UUID and writing the digest as
uppercase hex. It regenerates it on each local edit and delete: all six captured
writes of one recipe had different uppercase hashes. (Client, Capture)

This settles a conflict in our earlier sources: kappari's description of a random
token matches the client. A content-based hash may be accepted too, since a recipe's
hash only needs to differ for other devices to notice it. (Inferred) Use the client's
method.

Live recipe hashes are 64 hex characters, mostly lowercase with some uppercase.
(Live) The lowercase ones suggest that other Paprika clients generate them
differently, so compare hashes as opaque values. (Inferred)

Only recipes send `hash` when uploading. Meal and grocery objects omit it. (Capture)

The server stores whatever `hash` it is sent, in either case, and does not require a
new one. (Live) **A recipe write that does not change `hash` is not picked up by a device
that already holds the recipe:** a phone kept the old text through a sync that applied other
changes, so the device and server then disagree. A later edit with a new `hash` did reach
the phone, though it needed an app restart to reappear in the trash view. (Live) Always send
a new `hash`.

## Conflicts

**The protocol has no conflict detection that the client takes part in.** Uploads
carry no version or expected hash. Locally modified objects are protected from
the download, then uploaded over whatever the server has, so the last write wins.

The server does not reject stale writes: an upload carrying an older `hash` replaced
newer content and restored that older `hash`. (Live) A consumer that wants protection
must compare hashes itself, immediately before writing.

## Counters

`sync/status/` returns a JSON object mapping each resource name to a number. (Live)
These count changes, not objects: each counter seen was at least as large as its
list, and often much larger. (Live)

The server adds one per written object: nine captured meal writes raised `meals` by
nine, and six recipe writes raised `recipes` by six. (Capture, Live) A consumer can
use these counters to avoid unnecessary downloads.

## Notify

`POST sync/notify/` has no body and returns `{ "result": true }`. The client called it
after each sync that uploaded something. (Capture)

## Telemetry

The client also posts usage details to `POST /api/v2/statistics/`, including an
install ID and the account email. (Capture) API consumers do not need this.

## Real-Time Updates

The client also opens a WebSocket connection. (Client) Kappari's configuration
shows `wss://www.paprikaapp.com/ws/sync/`. We do not need this; the status counters
are enough.

## Implications for an API Client

1. Read the latest copy of an object immediately before modifying it.
2. Send the complete object, including keys you do not model. The server kept omitted
   fields when tested (Live), but clients replace their local copy from what they download,
   and this behavior is undocumented.
3. Generate a new `hash` when writing a recipe.
4. Call `sync/notify/` after a batch of writes.
5. Pace requests, and treat unexplained timeouts as a possible rate block.
