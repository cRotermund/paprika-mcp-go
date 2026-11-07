# Meal Plans

See the [evidence labels](../README.md#evidence-labels) and the
[sync model](sync-model.md).

Paprika has two related features:

- **Meals** are entries on specific calendar dates. This is what "meal plan" means
  here.
- **Menus** are reusable templates, made of **menu items** placed on numbered days.
  Applying one presumably creates ordinary meals. (Inferred)

## Meals

`GET sync/meals/` returns all meals, with no date filter visible in the client.
(Live, Client) Expect to filter locally. `POST sync/meals/` writes a gzipped JSON
array of changed meals. (Capture)

To move a meal, re-send it with a new `date`. To delete one, re-send it with
`"deleted": true`. Captured meals used midnight for the time. (Capture)

### Fields

| Key | Type seen | 3.3.1 sends | Notes |
| --- | --- | --- | --- |
| `uid` | string | yes | Usually an uppercase UUID; see [identifiers](sync-model.md#identifiers) |
| `order_flag` | integer | yes | Position within a day |
| `name` | string | yes | Display name |
| `date` | string | yes | `YYYY-MM-DD HH:MM:SS`, with no time zone |
| `type` | integer | yes | Older fixed meal type; matches `original_type` on meal types |
| `recipe_uid` | string or null | yes | Linked recipe; null when there is none |
| `type_uid` | string | yes | Meal type `uid`, a 64-character hex value |
| `is_ingredient` | boolean | no | Always false in the sample |
| `scale` | null | no | Always null in the sample |

Keys and types are from live responses. (Live) The client also uploads `deleted`,
which responses never include. (Capture)

Some meals have `recipe_uid: null` but still have a `name`, so a meal need not link
to a recipe. (Live)

### Reference Handling

During a download, the client checks both references. If `recipe_uid` points to an
unknown recipe, it clears the link and re-uploads. If `type_uid` is unknown, it
falls back to a default type based on `type`, and also re-uploads. (Client)

So an invalid reference can get silently changed by another device. **Validate
references before writing.** (Inferred)

## Meal Types

`GET sync/mealtypes/` returns `uid`, `name`, `order_flag`, `original_type`, `color`,
`export_time`, and `export_all_day`. (Live)

The sampled account has four types, with `original_type` values 0 to 3. Their
`export_time` values are 0, 28800, 43200, and 64800 seconds, which are midnight,
8 AM, noon, and 6 PM. Their names are Breakfast, Lunch, Dinner, and Snacks, in
`original_type` order. (Live) Use each account's `type_uid` and `name` instead of
hard-coding this, because other accounts have not been checked.

A second, test account had **no meal types** (`GET sync/mealtypes/` returned an empty
list), and all 371 of its meals had `type_uid: null`, `recipe_uid: null`, `scale: null`,
`is_ingredient: false`, and an integer `type` (values 1 to 3 seen). (Live) So an account
can lack meal types, and its meals then rely only on the numeric `type`. A consumer should
name such a meal by the built-in defaults, 0 Breakfast, 1 Lunch, 2 Dinner, 3 Snacks, which
are the names the first account's types had for those numbers. (Inferred) A new meal for
such an account sends the number as `type` and a null `type_uid`, matching the data seen.

## Menus and Menu Items

These are out of scope for this project's first release, and meal planning does not
need them. (Live for the fields below)

- **`menus`:** `uid`, `name`, `notes`, `order_flag`, `days`
- **`menuitems`:** `uid`, `name`, `order_flag`, `day`, `menu_uid`, `recipe_uid`,
  `type_uid`, `is_ingredient`, `scale`

## Example

An illustrative write body before gzip, in the captured format. The values are made
up.

```json
[
  {
    "uid": "AAAAAAAA-1111-4222-8333-444444444444",
    "deleted": false,
    "order_flag": 0,
    "name": "Vegetable Soup",
    "date": "2030-01-15 00:00:00",
    "type": 2,
    "recipe_uid": "11111111-2222-4333-8444-555555555555",
    "type_uid": "BBBBBBBB1111422283334444444444BBBBBBBB1111422283334444444444BBBB"
  }
]
```
