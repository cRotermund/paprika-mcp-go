# paprika-api-probe

Local exploratory tool that checks [the API docs](../../../docs/paprika-api/README.md)
against the live Paprika service. It is never deployed: it has no Dockerfile,
Kustomize manifests, or image publish entry.

By default it logs in, then only sends paced GET requests; a transport guard refuses
anything else. It reads each sync resource and a few full recipes.

The report lists keys, types, counts, numeric values, and string shapes, such as
`uuid-upper` or a masked date. It does not contain the text of your recipes, meals,
or groceries, or your email, password, token, or any `uid`.

## Setup

```sh
cp .env.example .env
# Fill in PAPRIKA_EMAIL and PAPRIKA_PASSWORD in .env.
go run . -out probe-report.local
```

Use `-env-file` to read credentials from another file, such as a test account's.

## Write Checks

`-write-checks` answers questions about how the server treats writes, and is meant for a
dedicated test account. It is the only mode besides `-cleanup` that can write, and it:

- Refuses to run unless `-confirm-account` repeats the configured email, and unless the account
  has 25 recipes or fewer or `-allow-real-account` is given.
- Saves meals, groceries, and related lists to a `.local` backup first. The backup contains
  personal text, so do not share it. Recipes are not backed up.
- POSTs only to `groceries`, `grocerylists`, `meals`, `recipe/{uid}`, and `notify`, and only for
  objects it created, named `MCP-TEST`. It records each one in a `.local` state file first.
- Sends one paced request at a time with no retries, and stops at the first unexpected response.
  It never tests rate limits.

```sh
go run . -env-file .env.sandbox -write-checks -confirm-account <email> -out write-checks-report.local
```

The objects stay in place so that their results can be checked in the app. `-inspect` re-reads
them without writing, to show what the app changed. Remove them with:

```sh
go run . -env-file .env.sandbox -cleanup -confirm-account <email> -out cleanup-report.local
```

Cleanup leaves the test recipe in the trash, to be emptied in the app.
