# Open Questions

What must be proven before relying on these docs. Each question lists the safest
way to answer it.

On 2026-11-01, the read-only probe (`src/tools/paprika-api-probe`) settled the read
routes and keys, and a traffic capture settled the client's write formats. What is left
is mostly how the server treats requests that differ from the client's.

## Test Methods

1. **Read-only probe:** GET requests after login, reporting structure but no personal text.
2. **Traffic capture:** watch the Windows client through a local proxy during a few
   throwaway edits. It may refuse to work through a proxy.
3. **Controlled write:** clearly named throwaway objects, checked in the Paprika
   app afterward. Back up meal plans and grocery lists first.

## Read Questions

| Question | Status | Method |
| --- | --- | --- |
| Route spelling and keys for each resource | Settled, except `cookbooks`, `pantrylocations`, and `recipesearches`, which have counters but return 404 | None needed |
| Date formats | Settled | None needed |
| Do trashed recipes appear in lists? | Yes | None needed |
| Which `original_type` means which meal type? | On the one account checked: 0 Breakfast, 1 Lunch, 2 Dinner, 3 Snacks. Still do not hard-code | None needed |
| What types do fields that are always null have? | Open | Probe an account that uses them, or capture |
| What do non-UUID `uid` values mean, and do new objects need them? | Open | Capture |

## Write Questions

Our own write checks ran once on 2026-11-02 against `MCP-TEST` objects. "Server" answers
come from re-reading through the API, and say nothing about what a client app then does.

| Question | Server answer (Live) | Still open |
| --- | --- | --- |
| Are fields sent as a partial object kept or blanked? | Kept, on a grocery item | Still send the whole object |
| Does the server accept and keep fields 3.3.1 does not send? | Yes: `servings_min`, `servings_max`, and `prep_minutes`, sent as integers | Other newer fields |
| Are newer recipe fields kept when an upload omits them? | Yes, for `servings_min`, `servings_max`, and `prep_minutes` | Other newer fields |
| Does the server reject stale writes? | No. A write carrying an old `hash` replaced the newer content and restored the old `hash` | |
| Does a write without a new `hash` reach other devices? | The server stores the edit and keeps the `hash` | Answered: no. A phone that already held the recipe kept the old text, in a sync that did apply other changes |
| Must `hash` have a certain case? | No. A lowercase value was stored as sent | |
| Can a `deleted` object be restored? | Yes, for a grocery item and a meal: re-posting with `deleted: false` lists it again. A phone that had already synced the delete showed the grocery item again | Restoring a meal on a device that synced its delete |
| Are unknown or empty references rejected? | No. An empty or unknown `aisle_uid` and an unknown `list_uid` are stored as sent. See [how the app reacted](grocery-lists.md#reference-handling) | |
| Do line breaks survive? | Yes. `\n` and `\r\n` are stored as sent, and both displayed as line breaks on a phone | Display on the Windows client |

Settled by the capture and write checks: bulk POSTs add or update by `uid`, a bulk POST of
several objects is applied in full, meals and groceries omit `hash`, `sync/notify/` needs no
body, and counters rise by one per written object, including within a bulk POST.

## Transport

| Question | Status or method |
| --- | --- |
| Is a custom `User-Agent` accepted? | Settled: yes, for login, reads, and writes |
| Does the server gzip responses? | Settled: yes, for the client's requests |
| Does Go's default multipart formatting work on sync routes? | Settled: yes, on `groceries`, `grocerylists`, `meals`, and `recipe/{uid}`; `notify` has no body |
| Does gzip apply to every bulk write? | Settled for groceries, grocery lists, meals, and recipes; the other routes come from the client and were not captured |
| How long do tokens last? | Observe over time. An invalid token returns HTTP 401 with "Invalid or expired credentials." and code 0 |
| What are the rate limits? | Never test this, on any account. A Paprika-side bucket of about 1 request per second sustained with a burst of about 8 to 10, with requests over it held rather than queued (Inferred). On 2026-11-07, on the real account, a background recipe load at about 3.4 requests per second stalled on its 11th request after about 3 s (status, list, 8 recipes, then the next request hung past the 20 s timeout), twice in a row, and recovered within 80 s; a probe at 1 request per second made about 21 requests without a stall (Live). The client now defaults to 60 requests per minute with a burst of 8, treats a timeout as throttling, and logs request counts at each stall to tune from |
| What do the numeric error codes mean? | Observe; only code 0 seen so far |
| Do grocery requests ever hang? | Yes (Live): on 2026-11-02 the test account saw `grocerylists` and `groceries` GET and POST requests sometimes hang for 30 s, about 6 times in 10 minutes at about 1 call per second. Writes that timed out had not landed when re-read |

## Could Decrypting the Client's Strings Help?

Decrypting the client's string table is no longer needed for routes or keys, because
the probe answered those without it. This project has not done it.
