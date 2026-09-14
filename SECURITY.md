# Security policy

## Reporting a vulnerability

Please report vulnerabilities through
[GitHub private vulnerability reporting](https://github.com/kornsour/run-ledger/security/advisories/new).
Do not open a public issue for a suspected vulnerability. Include the affected
version or commit, reproduction steps, impact, and any suggested mitigation.

Only the current default branch is supported until the project publishes a
stable release series.

## Deployment boundary

Run Ledger is a focused experiment-lineage service, not a multi-tenant identity
platform. Its security model assumes:

- TLS is terminated by a trusted ingress or reverse proxy. The server itself
  serves HTTP.
- Write and read bearer tokens are generated, stored, distributed, and rotated
  through an external secret-management system.
- The read token cannot write. The write token can read and write.
- Tokens are shared capabilities, not named user identities. The
  `submitter_claim` field is self-asserted and is not authenticated identity.
- Operators restrict network reachability and filesystem access to the DuckDB
  database according to the sensitivity of recorded experiment metadata.
- `/healthz` remains unauthenticated for probes; other operational endpoint
  exposure should be controlled at the network boundary.

The service does not currently provide tenant isolation, per-user
authorization, token issuance or rotation, TLS termination, or encryption of
the database at rest. Deployments requiring those controls must provide them
outside the service.
