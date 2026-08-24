---
name: Bug report
about: Report a problem with VaultS3
title: ''
labels: bug
assignees: ''
---

**Describe the bug**
A clear and concise description of what went wrong.

**To reproduce**
Steps to reproduce the behavior:
1. Start VaultS3 with '...'
2. Run '...' (S3 client command, dashboard action, or API call)
3. See error

**Expected behavior**
What you expected to happen instead.

**Diagnosis**
<!--
Run this and paste the whole output. It prints the version and which optional
subsystems are on, which is usually what decides whether a bug reproduces.
It reads the config only, prints no secrets, and works even if the server will
not start.

  vaults3 diagnose                              # package or binary install
  docker exec <container> vaults3 diagnose      # Docker, container running
  docker run --rm -v /your/vaults3.yaml:/etc/vaults3/vaults3.yaml \
    eniz1806/vaults3 diagnose                   # Docker, container will not start

Add -config <path> if your config is somewhere unusual.
-->

```

```

**Environment**
- Install method: <!-- package (deb/rpm/apk) / binary / Docker (eniz1806/vaults3) / from source -->
- Client: <!-- aws-cli, s3cmd, mc, rclone, SDK + version, or the dashboard -->
<!-- Version, OS and arch come from `vaults3 diagnose` above, no need to repeat them. -->

**Logs**
<!-- Relevant server log output. REDACT secrets, keys, and private bucket data. -->

```

```

**Additional context**
Anything else that might help. Object sizes and concurrency matter for anything
performance or memory related, and `vaults3 diagnose` already covers which
subsystems are enabled.
