#!/bin/sh
set -e

# A dedicated unprivileged account. Created only if missing, so an upgrade never
# disturbs an existing one or the ownership of data already on disk.
if ! getent group vaults3 >/dev/null 2>&1; then
    groupadd --system vaults3 2>/dev/null || addgroup -S vaults3 2>/dev/null || true
fi
if ! getent passwd vaults3 >/dev/null 2>&1; then
    useradd --system --gid vaults3 --home-dir /var/lib/vaults3 \
        --shell /sbin/nologin --comment "VaultS3 object storage" vaults3 2>/dev/null \
    || adduser -S -G vaults3 -h /var/lib/vaults3 -s /sbin/nologin vaults3 2>/dev/null || true
fi

for d in /var/lib/vaults3/data /var/lib/vaults3/metadata /var/log/vaults3; do
    mkdir -p "$d"
done
chown -R vaults3:vaults3 /var/lib/vaults3 /var/log/vaults3 2>/dev/null || true
chmod 0750 /var/lib/vaults3 /var/log/vaults3 2>/dev/null || true

# The config carries credentials once an operator sets them, so it is not world
# readable.
if [ -f /etc/vaults3/vaults3.yaml ]; then
    chown root:vaults3 /etc/vaults3/vaults3.yaml 2>/dev/null || true
    chmod 0640 /etc/vaults3/vaults3.yaml 2>/dev/null || true
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'MSG'

VaultS3 installed.

  Start it:   systemctl enable --now vaults3
  Dashboard:  http://127.0.0.1:9000/dashboard/
  Config:     /etc/vaults3/vaults3.yaml
  Data:       /var/lib/vaults3

No credentials are shipped. On first start VaultS3 generates an admin secret and
prints it once in the service log, so capture it now:

  journalctl -u vaults3 --no-pager | head -40

MSG
