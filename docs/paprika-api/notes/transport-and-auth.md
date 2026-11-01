# Transport and Authentication

See the [evidence labels](../README.md#evidence-labels).

## Base URL

`https://www.paprikaapp.com/api/{version}/`, where `{version}` is `v1` or `v2`.
(Kappari)

The client sends all requests under the version matching its current credential
type. (Client)

## Authentication

### Recommended: v1 Password Login

`POST /api/v1/account/login/` with form fields `email` and `password` returns:

```json
{ "result": { "token": "<token>" } }
```

The token is accepted on v2 sync routes as `Authorization: Bearer <token>`.
No license data is needed. (Live) A URL-encoded form works, and so does a custom
`User-Agent`. (Live)

A wrong password returns HTTP 200 with
`{ "error": { "message": "Incorrect password.", "code": 0 } }`. (Live)

### v2 Login

`POST /api/v2/account/login/` takes `email`, `password`, and the two license
fields `data` and `signature`. It rejects password-only logins from non-mobile
clients. (Kappari) We do not need this route.

### Basic Authentication

The client still has a v1 mode that sends HTTP Basic credentials with
`email:password`. (Client) We should not use it: a token avoids sending the
password on every request.

### Token Expiry

The client treats HTTP 401 as an authentication failure. (Client) The token's
lifetime is unknown. Plan to log in again on 401 and retry once. (Inferred)

## Headers

| Header | Value | Evidence |
| --- | --- | --- |
| `Authorization` | `Bearer <token>` | Live |
| `User-Agent` | A custom value works for login and reads. Writes are untested. | Live |
| `Expect` | Not sent by the client on sync requests, so `100-continue` is not needed | Capture |
| `Accept-Encoding` | `gzip, deflate`; the client decompresses both, and the server gzips responses to it | Client, Capture |

## Request Bodies

Reads are GET requests with no body. Writes are POST requests with
`multipart/form-data`. (Client)

The object or array being written is JSON, compressed with gzip, and attached
as a binary part named `data` with a filename. The part has no `Content-Type` header.
(Capture)

```
Content-Type: multipart/form-data; boundary="<uuid>"

--<uuid>
Content-Disposition: form-data; name=data; filename=file; filename*=utf-8''file

<gzip-compressed JSON bytes>
--<uuid>--
```

### Multipart Formatting

Field names have no quotes. (Capture) The client builds its forms with .NET's
standard multipart class, so this looks like a .NET framing habit, not a deliberate
server rule. (Client, Inferred) Try Go's standard `mime/multipart` first, and only
hand-craft the framing if the server rejects it. See
[open questions](open-questions.md#transport).

## Responses

Successful responses are JSON with the payload under `result`. (Live)

```json
{ "result": true }
```

Errors use an `error` object, with or without an error status. (Live)

| Case | Example |
| --- | --- |
| HTTP 200 | Wrong password: `{ "error": { "message": "Incorrect password.", "code": 0 } }` |
| HTTP 404 | Unknown route: `{ "error": { "message": "Not found.", "code": 0 } }` |

Always check for an `error` object, even on HTTP 200. The client also treats 401 and
503 as special cases. (Client)

## Timeouts and Rate Limits

The client uses a 200-second request timeout. (Client) There is no documented
rate limit. One community comment reports a temporary IP block after about 300
connections per minute, seen as timeouts instead of an HTTP error. (Community)
Keep requests sequential and paced, as the client's own recipe sync is.
