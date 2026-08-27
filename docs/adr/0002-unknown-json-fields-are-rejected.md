# ADR 0002: Unknown JSON fields are rejected

Status: Accepted
Date: 2026-08-25

## Context

`POST /runs` decodes the request body into a `lineage.Run`. A caller might
send a field that doesn't exist on `Run` — most often a typo (`git_commmit`,
`dataset_verison`) or a stale field from a renamed struct tag. The Go JSON
decoder ignores fields it doesn't recognize by default.

## Decision

The decoder in `Server.record` (`internal/api/api.go`) calls
`DisallowUnknownFields()`:

```go
dec := json.NewDecoder(r.Body)
dec.DisallowUnknownFields()
```

An unrecognized field makes the whole request a `400`, not a partially-applied
record.

## Consequences

- A typo'd identity field (e.g. `git_commmit` instead of `git_commit`) fails
  loudly at record time instead of silently recording a run whose real
  `git_commit` is empty and whose fingerprint is therefore wrong. Silent
  acceptance would have produced a run that claims to describe an experiment
  it does not.
- Renaming or removing a field in `lineage.Run` is a breaking API change for
  any client still sending the old shape, not a soft deprecation. Field
  renames need a transition (accept both old and new for a period, or bump an
  API version) rather than being made casually.
- Clients cannot attach arbitrary extra metadata to a run record outside the
  fields the schema defines. Free-form metadata, if ever needed, has to be a
  first-class field (e.g. a `map[string]string` "tags") rather than
  passenger fields riding along in the JSON body.

## What would have to be true to revisit this

A real need for forward-compatible clients — e.g. an older server that must
accept records written by a newer client carrying fields it doesn't know
about yet. That would call for an explicit extension point (a `metadata` map)
rather than loosening this check, so "unknown field" keeps meaning "typo" and
not "field we haven't gotten around to supporting."
