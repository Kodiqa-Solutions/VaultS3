#!/bin/sh
# Run the ceph/s3-tests S3 conformance suite against a VaultS3 server.
#
# Why this exists: VaultS3 advertises 80+ S3 operations and, until this, had no
# external check of that claim. Its own tests are written against its own
# understanding of S3, so they cannot catch a misreading of the spec. s3-tests is
# the suite the rest of the object-storage world is measured against.
#
# How it is used, and why it is not simply "run everything and go green":
#
#   implemented_tests.txt is a whitelist of the tests VaultS3 is expected to
#   pass. CI runs ONLY that list, and it must stay green. That makes this a
#   regression gate from day one rather than a wall of failures nobody looks at.
#
#   The full sweep (MODE=all) runs the entire upstream suite and prints the tests
#   that now pass but are not yet on the list. Those are promotion candidates:
#   move them into implemented_tests.txt and they are protected from then on.
#
# Usage:
#   run.sh                      # gate: run the whitelist, fail on any failure
#   MODE=all run.sh             # full sweep: report promotion candidates
#   ENDPOINT=... ACCESS_KEY=... SECRET_KEY=... run.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
ENDPOINT="${ENDPOINT:-http://127.0.0.1:9000}"
ACCESS_KEY="${ACCESS_KEY:-vaults3-admin}"
SECRET_KEY="${SECRET_KEY:-vaults3-secret-change-me}"
MODE="${MODE:-gate}"
WORK="${WORK:-$(mktemp -d)}"
S3TESTS_REF="${S3TESTS_REF:-master}"

echo "s3-tests: endpoint=$ENDPOINT mode=$MODE work=$WORK"

# 1. Fetch the suite and build a runner image. Pinned by ref so a sweep is
#    reproducible; bump S3TESTS_REF deliberately, not by drifting with upstream.
if [ ! -d "$WORK/s3-tests" ]; then
  git clone --depth 1 --branch "$S3TESTS_REF" https://github.com/ceph/s3-tests.git "$WORK/s3-tests"
fi
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM python:3.11-slim
RUN apt-get update -qq && apt-get install -y -qq git >/dev/null && rm -rf /var/lib/apt/lists/*
WORKDIR /s3-tests
COPY s3-tests/ /s3-tests/
RUN pip install --no-cache-dir -q -r requirements.txt && pip install --no-cache-dir -q -e .
ENTRYPOINT ["pytest"]
DOCKER
docker build -q -f "$WORK/Dockerfile" -t vaults3-s3tests:local "$WORK" >/dev/null

# 2. Mint the extra identities the suite needs and write its config.
docker run --rm --network host -v "$HERE:/s:ro" curlimages/curl:latest \
  sh /s/provision.sh "$ENDPOINT" "$ACCESS_KEY" "$SECRET_KEY" > "$WORK/s3tests.conf"

mkdir -p "$WORK/out" && chmod 777 "$WORK/out"

run_pytest() {
  docker run --rm --network host \
    -v "$WORK/s3tests.conf:/s3-tests/s3tests.conf:ro" \
    -v "$WORK/out:/out" \
    -e S3TEST_CONF=/s3-tests/s3tests.conf \
    vaults3-s3tests:local "$@"
}

if [ "$MODE" = "all" ]; then
  echo "s3-tests: full sweep, this reports promotion candidates and does not gate"
  run_pytest s3tests/functional/test_s3.py --tb=no -q -p no:cacheprovider \
    --junitxml=/out/results.xml || true
  python3 "$HERE/report.py" "$WORK/out/results.xml" "$HERE/implemented_tests.txt"
  exit 0
fi

# Gate mode: run only the whitelist. Every one of these must pass.
SELECT=$(grep -v '^\s*#' "$HERE/implemented_tests.txt" | grep -v '^\s*$' | tr '\n' ' ')
if [ -z "$SELECT" ]; then
  echo "s3-tests: implemented_tests.txt is empty, nothing to gate on" >&2
  exit 1
fi
# shellcheck disable=SC2086
run_pytest $SELECT --tb=short -q -p no:cacheprovider --junitxml=/out/results.xml
