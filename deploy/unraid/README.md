# VaultS3 on Unraid

[`vaults3.xml`](vaults3.xml) is an Unraid Docker template. Until VaultS3 is in
Community Applications, add it by URL:

1. **Docker** tab, **Add Container**, then paste this in **Template**:
   `https://raw.githubusercontent.com/Kodiqa-Solutions/VaultS3/main/deploy/unraid/vaults3.xml`
   (or add this repository under **Docker → Template repositories**).
2. Check the two paths. **Object data** goes on the share that should hold
   the objects; **Metadata** is small and belongs on a cache pool (SSD).
3. Leave **Admin secret key** empty to have one generated, then open the
   container's log: it is printed there once. Or set your own.
4. **Apply**, then open the WebUI (port 9000, `/dashboard/`). The same port
   is the S3 endpoint for the AWS CLI, rclone, restic and other clients.

## Why it starts as root

Docker creates the `appdata` folders owned by root, and VaultS3 runs as an
ordinary user. The template starts the container as root (`--user 0:0`)
with `PUID=99` and `PGID=100` (Unraid's `nobody:users`): the server takes
ownership of its data and metadata folders, then switches to that user for
good before it opens anything else, and cannot become root again. The
folders are walked only when their owner is not PUID:PGID, so a large data
folder is walked once, on the first start.

The template also passes `--stop-timeout 35`, so requests still running when
the container stops get the 30 seconds the server gives them.

## Back up

Back up **Data** and **Metadata** together, from the same moment (stop the
container, or snapshot both at once). Metadata without the data, or the
other way round, cannot be restored on its own.

## Getting it into Community Applications

For the maintainers: Community Applications lists templates from repositories
registered with it. Submit this repository (or a template repository that
holds this file) through the Community Applications submission process on
the Unraid forums, with a support thread for the app. The template's
`TemplateURL` and `Icon` point at `main`, so they stay current.
