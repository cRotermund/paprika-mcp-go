# Recipes

See the [evidence labels](../README.md#evidence-labels) and the
[sync model](sync-model.md).

## List

`GET sync/recipes/` returns `uid` and `hash` for each recipe, with no other
detail. (Live)

```json
{ "result": [ { "uid": "11111111-2222-4333-8444-555555555555", "hash": "abc123..." } ] }
```

Trashed recipes are in this list: a sampled recipe with `in_trash: true` came from
it. (Live) Filter them out when showing a user their recipes.

To keep a local copy current, fetch only the recipes whose `hash` changed.
Hundreds of recipes mean hundreds of single-recipe requests on the first load.

## Read

`GET sync/recipe/{uid}/` returns one complete recipe. (Live)

## Create or Update

`POST sync/recipe/{uid}/`, with the recipe object as gzipped JSON in the `data`
part. The same request covers both create and update; the client chooses the `uid`
for a new recipe. (Capture)

The client always sends the complete object, and a new random `hash` after any
edit. See [hashes](sync-model.md#hashes). (Capture)

A new recipe without a photo sends `null` for `photo`, `photo_large`, `photo_hash`, and
`image_url`, an empty `categories` array, and empty strings for blank text fields.
(Capture) The Windows client writes line breaks as `\r\n`. (Capture)

Uploading a photo adds a second part to this request. Photos are out of scope, but
an update must keep the existing photo fields unchanged.

## Fields

These keys come from live responses for five sampled recipes. (Live) "3.3.1 sends"
shows whether Paprika for Windows 3.3.1 includes the field in its uploads. (Capture)

| Key | Type seen | 3.3.1 sends | Notes |
| --- | --- | --- | --- |
| `uid` | string | yes | |
| `created` | string | yes | `YYYY-MM-DD HH:MM:SS`, with no time zone |
| `hash` | string | yes | Change marker |
| `name` | string | yes | |
| `description` | string | yes | Can be empty |
| `ingredients` | string | yes | Multiline text |
| `directions` | string | yes | Multiline text |
| `notes` | string | yes | Can be empty |
| `nutritional_info` | string | yes | Can be empty; multiline text |
| `prep_time` | string | yes | Free text |
| `cook_time` | string | yes | Free text |
| `total_time` | string | yes | Free text |
| `difficulty` | string | yes | Free text |
| `servings` | string | yes | Free text |
| `rating` | integer | yes | Values 0 and 4 seen; 0 to 5 per the local schema |
| `scale` | null | yes | |
| `source` | string | yes | |
| `source_url` | string | yes | |
| `on_favorites` | boolean | yes | |
| `is_pinned` | boolean | yes | |
| `in_trash` | boolean | yes | App trash |
| `photo` | string | yes | Preserve |
| `photo_large` | null | yes | Preserve; likely a string when set (Inferred) |
| `photo_hash` | string or null | yes | Preserve |
| `image_url` | string or null | yes | Preserve |
| `categories` | array of strings | yes | Category `uid` values, not names |
| `photo_url` | string | no | Server-provided download link |
| `on_grocery_list` | boolean | no | |
| `prep_minutes` | null | no | Newer than 3.3.1 |
| `cook_minutes` | null | no | Newer than 3.3.1 |
| `total_minutes` | null | no | Newer than 3.3.1 |
| `servings_min` | null | no | Newer than 3.3.1 |
| `servings_max` | null | no | Newer than 3.3.1 |
| `nutrition` | null | no | Newer than 3.3.1 |
| `metadata_version` | null | no | Newer than 3.3.1 |
| `cookbook_uid` | null | no | Newer than 3.3.1 |

The client also uploads `deleted`, which responses never include. A `null` type
means every sample was null, so the real type is still unknown. "Newer than 3.3.1"
is an inference from the 3.3.1 client not using the field. (Inferred)

The server accepts uploads that omit the fields 3.3.1 does not send. (Capture) It accepted
integer `servings_min`, `servings_max`, and `prep_minutes` values, returned them on read,
and kept them after a later upload omitted them. (Live) This suggests a 3.3.1 client does
not erase them, but the other newer fields were not tested.

**Round-trip every field you read.** Whether another client's data survives in every
newer field is not proven. (Inferred)

## Categories

Set categories by `uid`. `GET sync/categories/` returns `uid`, `name`, `order_flag`,
`parent_uid`, and `cookbook_uid`. (Live) Categories nest through `parent_uid`.

On download, the client replaces a recipe's categories with the received list, so
sending a partial list would remove the others on other devices. (Client, Inferred)

## Delete

There are two levels, and they differ greatly:

| Operation | How | Recoverable |
| --- | --- | --- |
| Move to trash | Set `in_trash: true`, with a new `hash` (Capture) | Yes, in the app (Inferred) |
| Permanent delete | Set `deleted: true`, with a new `hash` (Capture) | Unknown; assume no |

Use the trash for anything an API consumer deletes. (Inferred)

## Recipe Import From a URL

The client has a route that sends a URL and page content to the server, probably
for recipe parsing. (Client, Community) Its details are unknown and it is not
needed for basic recipe writes.
