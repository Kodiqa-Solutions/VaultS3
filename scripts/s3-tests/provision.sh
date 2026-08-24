#!/bin/sh
# Provision the extra users ceph/s3-tests needs and print an s3tests.conf.
#
# The suite wants several independent identities (alt, tenant, iam) so it can
# check that one user cannot reach another's buckets. VaultS3 mints access keys
# itself and does not let a caller choose the key ID, so the config cannot be
# a static file: it has to be generated from whatever keys the server returns.
#
# Usage: provision.sh <endpoint> <admin-access-key> <admin-secret-key>
# POSIX sh on purpose: this runs inside minimal images (busybox ash) that have
# no bash, so no here-strings and no arrays.
set -eu

ENDPOINT="${1:?endpoint, e.g. http://127.0.0.1:9000}"
ADMIN_KEY="${2:?admin access key}"
ADMIN_SECRET="${3:?admin secret key}"

api() { curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

TOKEN=$(curl -sS -X POST "$ENDPOINT/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"accessKey\":\"$ADMIN_KEY\",\"secretKey\":\"$ADMIN_SECRET\"}" |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || { echo "provision: could not log in to $ENDPOINT" >&2; exit 1; }

# make_user <id> -> "accessKey secretKey"
make_user() {
  local uid="$1"
  api -X POST "$ENDPOINT/api/v1/iam/users" -d "{\"userId\":\"$uid\"}" >/dev/null 2>&1 || true
  api -X POST "$ENDPOINT/api/v1/keys" -d "{\"userId\":\"$uid\"}" |
    sed -n 's/.*"accessKey":"\([^"]*\)".*"secretKey":"\([^"]*\)".*/\1 \2/p'
}

set -- $(make_user s3tests-alt);    ALT_KEY="${1:-}";    ALT_SECRET="${2:-}"
set -- $(make_user s3tests-tenant); TENANT_KEY="${1:-}"; TENANT_SECRET="${2:-}"
set -- $(make_user s3tests-iam);    IAM_KEY="${1:-}";    IAM_SECRET="${2:-}"

if [ -z "$ALT_KEY" ] || [ -z "$TENANT_KEY" ] || [ -z "$IAM_KEY" ]; then
  echo "provision: the server did not mint one or more access keys" >&2
  exit 1
fi

HOST=$(printf '%s' "$ENDPOINT" | sed -E 's#^https?://##; s#:.*##')
PORT=$(printf '%s' "$ENDPOINT" | sed -E 's#.*:([0-9]+).*#\1#')

cat <<CONF
[DEFAULT]
host = $HOST
port = $PORT
is_secure = False
ssl_verify = False

[fixtures]
bucket prefix = s3t-{random}-
iam name prefix = s3-tests-
iam path prefix = /s3-tests/

[s3 main]
display_name = main
user_id = $ADMIN_KEY
email = main@example.invalid
api_name = us-east-1
access_key = $ADMIN_KEY
secret_key = $ADMIN_SECRET

[s3 alt]
display_name = alt
user_id = s3tests-alt
email = alt@example.invalid
access_key = $ALT_KEY
secret_key = $ALT_SECRET

[s3 tenant]
display_name = tenant
user_id = s3tests-tenant
email = tenant@example.invalid
access_key = $TENANT_KEY
secret_key = $TENANT_SECRET
tenant = testx

[iam]
display_name = iam
user_id = s3tests-iam
email = iam@example.invalid
access_key = $IAM_KEY
secret_key = $IAM_SECRET

[iam root]
access_key = $ADMIN_KEY
secret_key = $ADMIN_SECRET
user_id = RGW11111111111111111
email = iamroot@example.invalid

[iam alt root]
access_key = $ALT_KEY
secret_key = $ALT_SECRET
user_id = RGW22222222222222222
email = iamaltroot@example.invalid
CONF
