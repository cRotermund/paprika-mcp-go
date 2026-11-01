# Paprika Cloud Sync API

Unofficial notes on the HTTP API that Paprika Recipe Manager 3 uses to sync with
its cloud service. Paprika does not publish this API and it can change without
notice. This project is not affiliated with Hindsight Labs, LLC.

This research is a personal, non-commercial effort to connect my own Paprika data to my
home-based ecosystem. It was not done to resell anything or to make any money.

Baseline: Paprika for Windows 3.3.1, analyzed 2026-11-01.

## Contents

- [OpenAPI reference](openapi.yaml): every route and schema, for browsing in a viewer.
  The pages below explain the behavior behind them.
- [Transport and authentication](notes/transport-and-auth.md)
- [Sync model](notes/sync-model.md): counters, reads, writes, deletes, and conflicts
- [Recipes](notes/recipes.md)
- [Meal plans](notes/meal-plans.md)
- [Grocery lists](notes/grocery-lists.md)
- [Open questions](notes/open-questions.md): what must be proven before relying on this

## Status

Login, read routes, and JSON keys were **verified live** on 2026-11-01 with the
read-only probe in `src/tools/paprika-api-probe`.

Writes from Paprika for Windows 3.3.1 were **captured** on 2026-11-01 for recipes,
meals, grocery items, and grocery lists. **This project's own code has not written
to the API yet,** so its request formatting is untested.

## Evidence Labels

Every claim that matters carries one of these labels:

| Label | Meaning | Trust |
| --- | --- | --- |
| **Capture** | Seen in real traffic from the Windows client, with the server's response and later reads. | High for those exact requests. |
| **Client** | Seen in how the Windows client behaves in its compiled code. | High for what the client does. Says little about what the server requires. |
| **Schema** | Column names in the client's local database. | Good lead for JSON keys, but not proof. |
| **Kappari** | Documented by the [kappari](https://github.com/johnwbyrd/kappari) project. | Good; based on captured traffic. |
| **Community** | A [community gist](https://gist.github.com/7386ec363badfdeaad05a418b9a1f30a) or package notes. | Lead only. |
| **Inferred** | Our reasoning from the above. | Must be tested. |
| **Live** | Verified against the real API by this project. | Highest. Confirms this account's data, which may not show every possible value. |

## How Keys Were Established

The client stores its strings, including route names and JSON keys, in an
encrypted table. We did not decrypt it. The client's code shows how many fields it
uploads, their types, and which fields share a key. The names come from live
responses and captured uploads.

## Sources

These notes started from [kappari](https://github.com/johnwbyrd/kappari), and claims
labeled Kappari refer to
[commit `86c5a55`](https://github.com/johnwbyrd/kappari/tree/86c5a55457930f27d5d0171c61c67fb16ce7ed96).
It is also the place to look for what these notes leave out because this project does
not need it, such as license decryption, license-based login, and the local database.

## Rules for This Directory

- Describe the protocol in our own words.
- Do not add decompiled code, kappari's AGPL-licensed code or text, decrypted
  strings, license keys, tokens, or personal data.
- Examples use made-up values.
- When a test confirms or disproves a claim, update its label here.
