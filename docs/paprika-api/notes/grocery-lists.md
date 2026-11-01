# Grocery Lists

See the [evidence labels](../README.md#evidence-labels) and the
[sync model](sync-model.md).

Each resource reads with `GET sync/{resource}/` (Live) and writes changed objects as
a gzipped JSON array with `POST sync/{resource}/`. (Capture for items and lists)

The keys below are from live responses, and the client also uploads `deleted`, which
responses never include. (Live, Capture)

## Grocery Lists (`grocerylists`)

| Key | Type seen | Notes |
| --- | --- | --- |
| `uid` | string | An uppercase UUID or a 64-character hex value |
| `name` | string | |
| `order_flag` | integer | |
| `is_default` | boolean | One list in the sample was the default |
| `reminders_list` | string | Can be empty; a list created in the client sent `Paprika` (Capture) |

## Grocery Items (`groceries`)

| Key | Type seen | 3.3.1 sends | Notes |
| --- | --- | --- | --- |
| `uid` | string | yes | |
| `name` | string | yes | Display text |
| `order_flag` | integer | yes | |
| `purchased` | boolean | yes | Checked off |
| `recipe` | null | yes | Source recipe name (Inferred); null in the sample |
| `aisle` | string | yes | Aisle name |
| `ingredient` | string | yes | Parsed ingredient |
| `instruction` | string | yes | Parsed preparation note; can be empty |
| `quantity` | string | yes | Parsed amount; can be empty |
| `separate` | boolean | yes | Meaning unconfirmed; likely not merged with matching items |
| `aisle_uid` | string | yes | Aisle reference, a 64-character hex value |
| `list_uid` | string | yes | Grocery list reference |
| `recipe_uid` | null | no | Null in the sample |

### Parsed Fields

If a downloaded item has neither `quantity` nor `instruction`, the client parses
`name` itself to fill in the three parsed fields, and saves the result. (Client)

A new item from the client sent its text in both `name` and `ingredient`, with empty
`quantity` and `instruction`, and that was accepted. (Capture) Use that form for items
without separate parsed values. (Inferred)

### Reference Handling

The client handles bad references during download, and some outcomes are
destructive: (Client)

| Situation | Client behavior |
| --- | --- |
| Unknown `aisle_uid` | Assigns a default aisle and re-uploads the item |
| Empty `aisle_uid` | Looks up an aisle by name, then falls back to the default |
| **Unknown `list_uid`** | **Marks the item deleted** |
| Empty `list_uid` | Assigns the default list |

**Writing an item with an invalid `list_uid` can cause another device to delete
it.** Always verify the list exists before writing. (Inferred)

## Grocery Aisles (`groceryaisles`)

Fields: `uid` (a 64-character hex value in the sample), `name`, `order_flag`. (Live)

## Remembered Ingredients (`groceryingredients`)

Fields: `uid`, `name`, `aisle_uid` (can be null). (Live) This appears to remember
which aisle an ingredient belongs to. An API client can probably ignore it, at the
cost of the app not learning from API-created items. (Inferred)

The app appears to add entries here when new items are created, so test items can
leave entries behind. (Live, Inferred)

## Common Operations

All of these were captured from the client. (Capture)

| Operation | Write |
| --- | --- |
| Add an item | New `uid`, valid `list_uid` and `aisle_uid` |
| Check off an item | Full existing item with `purchased: true` |
| Remove an item | Full existing item with `deleted: true` |
| Create a list | New `uid`, `is_default: false` |
| Remove a list | Full existing list with `deleted: true` |

## Example

An illustrative write body before gzip, in the captured format. The values are made
up.

```json
[
  {
    "uid": "CCCCCCCC-1111-4222-8333-444444444444",
    "deleted": false,
    "name": "2 carrots, diced",
    "order_flag": 0,
    "purchased": false,
    "recipe": null,
    "aisle": "Produce",
    "ingredient": "carrots",
    "instruction": "diced",
    "quantity": "2",
    "separate": false,
    "aisle_uid": "DDDDDDDD1111422283334444444444DDDDDDDD1111422283334444444444DDDD",
    "list_uid": "EEEEEEEE-1111-4222-8333-444444444444"
  }
]
```
