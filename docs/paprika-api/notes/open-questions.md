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
| Which `original_type` means which meal type? | Open; do not hard-code | Read meal type names |
| What types do fields that are always null have? | Open | Probe an account that uses them, or capture |
| What do non-UUID `uid` values mean, and do new objects need them? | Open | Capture |

## Write Questions

| Question | Current belief | Method |
| --- | --- | --- |
| Are fields sent as a partial object kept or blanked? | Assume blanked; send the whole object | Controlled write on a throwaway object |
| Does the server accept and keep fields 3.3.1 does not send? | Unknown; needed for round-tripping | Controlled write |
| Are newer recipe fields kept when an upload omits them? | Unknown; matters for older clients too | Controlled write setting one, then re-reading |
| Does the server reject stale writes? | No | Controlled write |
| Does a write without a new `hash` reach other devices? | No, for recipes | Controlled write |
| Must `hash` have a certain case? | Both cases exist in live data, so probably not | Controlled write |
| Can a `deleted` object be restored? | Assume no | Controlled write on a throwaway object |

Settled by the capture: bulk POSTs add or update by `uid`, meals and groceries omit
`hash`, `sync/notify/` needs no body, and counters rise by one per written object.

## Transport

| Question | Status or method |
| --- | --- |
| Is a custom `User-Agent` accepted? | Settled for login and reads; test on writes with a controlled write |
| Does the server gzip responses? | Settled: yes, for the client's requests |
| Does Go's default multipart formatting work on sync routes? | Controlled write |
| Does gzip apply to every bulk write? | Settled for groceries, grocery lists, meals, and recipes; the other routes come from the client and were not captured |
| How long do tokens last, and what does an expired token return? | Observe over time |
| What are the rate limits? | Do not test deliberately; stay well below the reported 300 connections per minute |
| What do the numeric error codes mean? | Observe; only code 0 seen so far |

## Could Decrypting the Client's Strings Help?

Decrypting the client's string table is no longer needed for routes or keys, because
the probe answered those without it. This project has not done it.
