// Package paprikaclient is the only code in this service that talks to the
// Paprika cloud sync API. It knows routes, encoding, login, pacing, and errors,
// and holds no business rules.
//
// # Login
//
// The client logs in with the v1 email and password route only when it has no
// token or after a 401. The token lives in memory only. After a 401 it logs in
// once and repeats the request once; a second 401 is [ErrUnauthorized].
// A rejected login starts an exponential backoff (1 minute doubling to 1 hour by
// default), during which every call fails fast with [ErrUnauthorized] and no
// request reaches Paprika. Transport failures and 5xx responses during login
// are [ErrTemporary] and do not start the backoff. Concurrent callers that need
// a token share a single login attempt.
//
// # Rate limiting and timeouts
//
// One request is in flight at a time across the process. Every request,
// logins included, first waits for a slot in an in-memory sliding-window
// limiter: at most [DefaultRequestLimit] starts in any trailing
// [DefaultRequestWindow]. Requests are interactive by default; a context
// marked with [Background] makes a request background, and it starts only if
// [BackgroundReserve] slots would remain free. Paprika's own limit is
// unpublished and the defaults sit near an inferred one, so the client never
// probes for it.
//
// Each request has a 20 second timeout. A timeout is treated as a throttle
// signal: the limiter stays closed for a cooldown ([DefaultStallCooldown]), and
// a warning records how many requests started in the window and how the
// connection looked (reused, idle time, protocol, requests on the connection).
// The same connection fields are on the debug line for every request. After 3
// consecutive timeouts the client also pauses for 5 minutes, and every call
// returns [ErrTemporary] without contacting Paprika until the pause ends.
//
// # Errors
//
// Every response is checked for an "error" object, including on HTTP 200.
// Callers match errors with [errors.Is] and [errors.As]:
//
//   - [ErrUnauthorized]: login is failing or the token keeps being refused. This
//     is a configuration problem, not something a caller can retry away.
//   - [ErrTemporary]: HTTP 503 or 429, a timeout, or a pause. Retry later.
//   - [ErrNotFound]: HTTP 404.
//   - [ErrOutcomeUnknown]: a POST failed in transit or timed out, so the write
//     may or may not have been applied. The client never retries a POST after
//     this, except once after a 401, which proves the request was rejected.
//   - [ErrResponseTooLarge], [ErrUnexpectedResponse]: the reply was unusable.
//   - [*APIError]: Paprika answered with an error code and message. The message
//     comes from upstream and should not be shown to end users verbatim.
//
// # Safety
//
// Responses are capped at 32 MiB after decompression. Redirects are never
// followed, so the token cannot reach another host. Plain HTTP is accepted only
// for loopback hosts. Credentials, tokens, and request or response bodies are
// never logged.
//
// # Objects
//
// Paprika wants whole objects back, including fields this service does not
// model. [Object] keeps every field as raw JSON so a read-modify-write cycle
// round-trips unknown fields exactly, including number formatting. Read methods
// return [Entry] values holding both the raw [Object] and a typed view.
package paprikaclient
