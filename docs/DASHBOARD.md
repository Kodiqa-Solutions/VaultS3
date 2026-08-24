# Web dashboard

The built-in React UI at /dashboard/.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

The built-in dashboard is available at `http://localhost:9000/dashboard/`. Login with your admin credentials. Features:

- Bucket browser, list, create, delete buckets
- Bucket detail, view/edit policies and quotas
- File browser, list, upload (drag & drop files and folders), download, delete objects with folder navigation, multi-select with bulk delete and bulk zip download, copy-to-clipboard for S3 URIs and keys
- Access key management, create, list, revoke S3 API keys
- IAM management, users, groups, policies CRUD with attach/detach operations
- Audit trail, filter by user, bucket, time range with auto-refresh
- Search, full-text search across all buckets by key, content type, tags
- Notifications, view webhook notification configurations
- Replication, peer status cards, pending queue table
- Lambda triggers, status overview, trigger table with event filtering
- Backups, status cards, history table, manual trigger button
- Activity log, real-time S3 operation feed with auto-refresh
- Storage stats, logical size, VaultS3's measured on-disk footprint and total filesystem usage side by side (with a per-directory and per-node breakdown), per-bucket breakdown, runtime metrics, auto-refresh toggle (30s)
- Migrate, import buckets from any S3-compatible source with live progress and a Cancel button for in-flight jobs
- Version indicator, the running version is shown at the bottom of the sidebar, with an "update available" hint linking to releases
- Dark/light theme, toggle with system preference detection
- Language, English, German, French, Simplified Chinese, detected from the browser and switchable in the top bar
- Responsive layout, mobile-friendly with collapsible sidebar
- JWT-based authentication (24h tokens)

The dashboard is embedded into the binary, no separate web server needed.

### Language

The dashboard picks a language from the browser on first load and falls back to
English. Change it with the selector in the top bar, and the choice is stored per
browser, so different people using the same server can each read it in their own
language. There is no server-side setting.

Shipping today: **English, Deutsch, Francais, and simplified Chinese**. The
non-English files were drafted without a native-speaker review, so corrections
are welcome.

**Adding a language takes one JSON file and no code**: copy
`web/src/i18n/locales/en.json`, translate the values, and add one entry to
`LOCALES` in `web/src/i18n/index.tsx`. Any key you leave out falls back to
English, so a partial translation is fine to send. See
[Translating the dashboard](../CONTRIBUTING.md#translating-the-dashboard) for the
full steps and the test that checks a locale file.

Server-side output (S3 API error codes, log lines) is English only.

### Screenshots

| Buckets | File Browser |
|:---:|:---:|
| ![Buckets](../assets/screenshots/dashboard-buckets.png) | ![File Browser](../assets/screenshots/dashboard-file-browser.png) |

| Bucket Detail | Search |
|:---:|:---:|
| ![Bucket Detail](../assets/screenshots/dashboard-bucket-detail.png) | ![Search](../assets/screenshots/dashboard-search.png) |

| Dark Mode | Settings |
|:---:|:---:|
| ![Dark Mode](../assets/screenshots/dashboard-home-dark.png) | ![Settings](../assets/screenshots/dashboard-settings.png) |
