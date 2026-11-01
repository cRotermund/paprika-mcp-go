# paprika-api-probe

Local exploratory tool that checks [the API docs](../../../docs/paprika-api/README.md)
against the live Paprika service. It is never deployed: it has no Dockerfile,
Kustomize manifests, or image publish entry.

It logs in, then only sends paced GET requests; a transport guard refuses
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
