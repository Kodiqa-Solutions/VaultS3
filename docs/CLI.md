# Command-line tool

Managing buckets, objects, users, storage, and clusters with vaults3-cli.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

VaultS3 includes a standalone CLI binary (`vaults3-cli`) for managing the server:

```bash
# Set credentials via environment or flags
export VAULTS3_ENDPOINT=http://localhost:9000
export VAULTS3_ACCESS_KEY=vaults3-admin
export VAULTS3_SECRET_KEY=vaults3-secret-change-me

# Server version and storage capacity (used / free / total)
vaults3-cli info

# Bucket operations
vaults3-cli bucket list
vaults3-cli bucket create my-bucket
vaults3-cli bucket info my-bucket
vaults3-cli bucket delete my-bucket

# Object operations
vaults3-cli object put my-bucket docs/readme.md ./README.md
vaults3-cli object ls my-bucket                       # folder view (objects + prefixes)
vaults3-cli object ls my-bucket --prefix=docs/        # list inside a prefix
vaults3-cli object ls my-bucket --recursive           # all nested objects (paginates past 1000)
vaults3-cli object get my-bucket docs/readme.md ./downloaded.md
vaults3-cli object cp my-bucket/file.txt my-bucket/copy.txt
vaults3-cli object rm my-bucket docs/readme.md
vaults3-cli object presign my-bucket file.txt --expires=3600
vaults3-cli object verify my-bucket                   # find objects that list but cannot be read (metadata/data desync)
vaults3-cli object verify my-bucket --repair          # remove orphaned metadata for unreadable objects

# Storage maintenance
vaults3-cli storage reclaim                    # report data files no metadata refers to (dry run)
vaults3-cli storage reclaim --apply            # delete them and free the space

# IAM user operations
vaults3-cli user list
vaults3-cli user create alice --access-key=ak --secret-key=sk
vaults3-cli user attach-policy alice ReadWriteAccess
vaults3-cli user delete alice

# Replication monitoring
vaults3-cli replication status
vaults3-cli replication queue

# Cluster operations (see docs/SCALING.md)
vaults3-cli bucket durability scratch --erasure=off --replicas=1  # store this bucket once
vaults3-cli cluster status                     # members, leader, drain state
vaults3-cli cluster join node-3 10.0.0.4:7000  # add a member (against the leader)
vaults3-cli cluster drain node-2               # stop a node accepting writes (reads continue)
vaults3-cli cluster rebalance                  # move objects to their correct owner
vaults3-cli cluster decommission node-2        # guided drain + rebalance before replacing a node
vaults3-cli cluster shards                     # how object metadata is distributed across the cluster
```

Build both binaries with `make build` or just the CLI with `make cli`.


## Server subcommands

The server binary carries a few commands of its own, separate from `vaults3-cli`.

### `vaults3 diagnose`

Prints the version and which optional subsystems are switched on. That is usually
what decides whether a bug reproduces, and it is the thing a bug report most often
leaves out.

```bash
vaults3 diagnose                              # package or binary install
docker exec <container> vaults3 diagnose      # Docker, container running
docker run --rm -v /your/vaults3.yaml:/etc/vaults3/vaults3.yaml \
  eniz1806/vaults3 diagnose                   # Docker, container will not start
```

It reads the config only and never contacts the server, so it works on a machine
where the server refuses to start, which is when it is needed most. With no
`-config` it looks for `configs/vaults3.yaml` then `/etc/vaults3/vaults3.yaml`.

No secrets are printed. Keys, tokens and the cluster secret are reduced to whether
they are set, so the output can go into a public issue without redaction. Add
`-json` for a machine-readable form.

It also flags interactions worth knowing about, such as compression being a no-op
while encryption is enabled, or `cluster.secret` being empty.

### `vaults3 healthcheck`

Probes the server's own `/health` and exits 0 when it answers, 1 when it does not,
and 2 on a usage error. The container image uses it, so the image needs no shell
or HTTP client to report liveness. It follows the config, so a changed port, a
reverse-proxy base path or TLS are honoured rather than assumed.

```bash
vaults3 healthcheck -config /etc/vaults3/vaults3.yaml
```

### `vaults3 setup`

Asks a few questions, creates the directories and writes a config containing only
what you chose. See the [installation guide](INSTALL.md).

## Test with mc (MinIO Client)

```bash
mc alias set vaults3 http://localhost:9000 vaults3-admin vaults3-secret-change-me
mc mb vaults3/my-bucket
mc cp file.txt vaults3/my-bucket/
mc ls vaults3/my-bucket/
mc cat vaults3/my-bucket/file.txt
```
