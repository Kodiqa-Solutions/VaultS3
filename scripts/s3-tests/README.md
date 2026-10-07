# S3 conformance testing

VaultS3 advertises 80+ S3 operations. Its own test suite is written against its
own reading of the S3 specification, so it cannot catch a misreading. This runs
[`ceph/s3-tests`](https://github.com/ceph/s3-tests), the suite the rest of the
object-storage world is measured against, against a real VaultS3 server.

Adopting it found two real bugs on the first run that got far enough to execute:

- `ListObjectVersions` returned nothing for a bucket that had never had
  versioning enabled. S3 lists those objects with the version id `null`. Tools
  use that call to enumerate a bucket before emptying it, so a caller saw an
  empty bucket, deleted nothing, and then could not delete the bucket either.
- Deleting the `null` version of such an object removed its metadata but left the
  bytes on disk, because the version-aware delete looks under `.vs/` and those
  objects live at the ordinary path. That is an orphan nothing references, and it
  is why the bucket stayed undeletable.

## How it is used

`implemented_tests.txt` is a whitelist of the tests VaultS3 is expected to pass.
CI runs only that list and every entry must pass, which makes the suite a
regression gate from day one rather than a wall of failures nobody reads.

```sh
# gate: run the whitelist, fail on any failure (what CI does on a PR)
scripts/s3-tests/run.sh

# full sweep: run everything, report which tests could be promoted
MODE=all scripts/s3-tests/run.sh
```

Point it somewhere else with `ENDPOINT`, `ACCESS_KEY` and `SECRET_KEY`.

## Growing the whitelist

Run a full sweep. It prints **promotion candidates**: tests that pass but are not
yet gated. Move them into `implemented_tests.txt` and they are protected from
then on. It also prints **regressions**: whitelisted tests that stopped passing,
which is the only thing a sweep fails over.

Never add a test that does not pass. An aspirational entry turns the gate red and
trains everyone to ignore it.

## The baseline

Captured 2026-08-24 against a single node with default settings:

| | |
|---|---:|
| collected | 838 |
| passing, and now gated | 192 |
| failing | 155 |
| errored | 492 |

Grown on 2026-10-07 to **317 gated tests**, including 15 for object tagging,
against 5.0.1 plus the tagging and object lock fixes that followed. That was the
first sweep to run to the end. Before it, every sweep stopped part way: two
tests leave a lock the suite's own cleanup cannot remove, after which every
later test failed in setup with `BucketNotEmpty` or "still has objects locked".
That hid about 300 results per sweep. `run.sh` now skips the two tests that set
a legal hold or retention in a bucket without object lock (VaultS3 allows that
on purpose, AWS does not), and the fixes made the rest clean up. Each new entry
passed three times on fresh servers, and the whole list passes together in
about 70 seconds.

At 192 tests the gate took about 13 seconds locally and about 28 seconds on a
GitHub runner, inside a job of roughly two minutes. The tests outside the whitelist are a mix of
features VaultS3 does not implement (ACL-based authorization, several SSE-KMS
variants, RGW-specific behaviour) and genuine gaps worth fixing. The sweep is how
they get triaged, one promotion at a time.

## Files

| | |
|---|---|
| `run.sh` | Runs the suite, in gate or sweep mode. POSIX sh, uses Docker. |
| `provision.sh` | Mints the extra IAM users the suite needs and writes its config. VaultS3 generates access keys itself, so the config cannot be a static file. |
| `report.py` | Summarises a run, names promotion candidates and regressions. |
| `implemented_tests.txt` | The whitelist CI gates on. |
