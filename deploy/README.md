# Deploying VaultS3

| Method | Path | Best for |
|--------|------|----------|
| **Helm chart** | [`helm/vaults3/`](./helm/vaults3/) | Configurable, production installs |
| **Plain manifests** | [`k8s/quickstart.yaml`](./k8s/quickstart.yaml) | One-command single-node try-out |
| **Docker / Compose** | [`../docs/INSTALL.md`](../docs/INSTALL.md#docker) | Single host, no Kubernetes |

## Helm (recommended)

```bash
helm install vaults3 ./helm/vaults3 \
  --namespace vaults3 --create-namespace \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

Full configuration reference: [`helm/vaults3/README.md`](./helm/vaults3/README.md).

## Plain manifests (no Helm)

```bash
kubectl apply -f k8s/quickstart.yaml
kubectl -n vaults3 rollout status statefulset/vaults3
```

Edit the `Secret` in `k8s/quickstart.yaml` to change the admin secret key first.

## What gets deployed

Both methods deploy a **StatefulSet** with:

- One container, port `9000` (S3 API + `/dashboard/` + `/metrics` + `/health` + `/ready`)
- Admin credentials from a **Secret** (env `VAULTS3_ACCESS_KEY` / `VAULTS3_SECRET_KEY`)
- `vaults3.yaml` from a **ConfigMap** at `/etc/vaults3/`
- Two **PersistentVolumeClaims**: `/data` (objects) and `/metadata` (BoltDB)
- Liveness (`/health`) and readiness (`/ready`) probes
- Non-root securityContext (UID/GID 1000)

See [`docs/SCALING.md`](../docs/SCALING.md) for redundancy (erasure coding vs
clustering) and capacity planning.
