# Sub2API Docker Release

The official image for this fork is published only through GitHub Releases.
Each release has one matching multi-platform image:

```text
GitHub Release: v0.1.150
Docker image:   ghcr.io/gwenliu1025/sub2api:0.1.150
```

There is no `latest` tag and no architecture-specific tag. The same version
tag works on `linux/amd64` and `linux/arm64`.

## Deploy With Compose

Use one of the provided Compose files and copy `.env.example` to `.env`.
Keep `SUB2API_IMAGE` on an exact published version.

```bash
cp .env.example .env
docker compose up -d
```

## Management-Panel Updates

Docker updates require the host updater from `deploy/updater/` to be installed
on the target host. It owns Docker access; the application receives only its
Unix socket.

After the updater is installed, the management panel follows two steps:

1. **Prepare Image** reads a release from `gwenliu1025/sub2api`, pulls the
   matching `ghcr.io/gwenliu1025/sub2api:X.Y.Z` image, and verifies it without
   interrupting service.
2. **Restart To Switch** recreates only `sub2api` on that prepared image,
   checks health, and restores the previous exact image automatically if the
   switch fails.

Both the GitHub Release and its same-version GHCR image must exist. A Release
asset alone is not sufficient for a Docker update.

See `deploy/updater/README.md` for host-updater installation and recovery
details.
