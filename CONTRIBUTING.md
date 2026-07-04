# Contributing to VaultS3

Thanks for your interest in improving VaultS3! This guide covers how to build,
test, and submit changes.

## Getting started

VaultS3 is a Go server with a React (Vite) dashboard, shipped as a single binary.

**Prerequisites**

- Go **1.25+**
- Node.js **20.19+** (the dashboard builds with Vite 8 / Rolldown)
- `make`

**Build**

```bash
make build      # builds the React dashboard, then the server + CLI binaries
./vaults3       # starts the server on :9000 (dashboard at /dashboard/)
make cli        # build just the CLI (./vaults3-cli)
```

## Project layout

```
internal/
  s3/           S3 API handlers (PutObject, ListObjectsV2, multipart, …)
  api/          Dashboard REST API
  storage/      On-disk object engine
  versioning/   Object versioning, delete markers
  erasure/      Reed-Solomon erasure coding + background healer
  cluster/      Raft metadata, consistent-hash ring, failure detector
  replication/  Active-active (vector clocks) + one-way push
  iam/          Access keys, policies, OIDC/LDAP
  ...
web/            React dashboard (Vite)
docs/           Operator docs (see docs/SCALING.md)
```

## Running tests

```bash
go test ./...                       # all Go unit tests
go test ./internal/erasure/ -v      # a single package
go test -race ./internal/cluster/   # with the race detector
```

The dashboard:

```bash
cd web && npm install && npm run build
```

Please add tests for any behavior change. The data-durability subsystems
(`erasure`, `cluster`, `replication`) are held to a higher bar, new logic there
should come with fault-injection coverage (corrupt a shard and heal, lose a
node, partition two sites and check convergence). See the existing
`*_test.go` files in those packages for the pattern.

## Submitting changes

1. Fork and create a feature branch off `main`.
2. Keep changes focused. One logical change per PR.
3. Run `go test ./...` and `npm run build` before opening the PR.
4. Use clear, present-tense commit messages (e.g. `Fix versioned ListObjectsV2 empty result`).
5. Update `README.md` and any relevant docs when you change user-facing behavior.
6. Open the PR against `main` and describe **what** changed and **why**.

## Contributor License Agreement

One lightweight, one-time step keeps VaultS3's licensing clean. It runs
automatically on your pull request.

- **CLA (sign once, ever).** On your first PR, the **CLA Assistant** bot asks you
  to sign the [Contributor License Agreement](.github/CLA.md) by commenting:
  > I have read the CLA Document and I hereby sign the CLA

  This grants Kodiqa Solutions the right to keep VaultS3 open source **and** offer
  commercial editions, it's what makes the open-core model possible. You keep full
  ownership of your contributions. One signature covers all your future PRs.

## Reporting bugs

Open an issue with: VaultS3 version (`./vaults3 -version`), OS/arch, your config
(redact secrets), steps to reproduce, and what you expected vs. what happened.

## Security

**Do not** open public issues for vulnerabilities. Follow the disclosure process
in [SECURITY.md](SECURITY.md).

## License

By contributing you agree your contributions are licensed under the project's
[AGPL-3.0](LICENSE) license.
