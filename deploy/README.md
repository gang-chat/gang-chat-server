# Deploy

CI builds `gang-server` on a Linux Gitea runner and ships it to a remote
server over SSH. The app runs in the background (no Docker) and is managed by
the scripts in this folder.

## Process management scripts

Run them on the **server**, from the deploy directory:

```bash
./start.sh            # start livekit-server + gang-server
./stop.sh             # stop both
./restart.sh          # restart both
./restart.sh gang     # restart only the app (leaves live calls untouched)
./start.sh livekit    # start only livekit
```

- PID files: `./run/<name>.pid`
- Logs: `./logs/<name>.log`
- Target is one of `all` (default), `gang`, `livekit`.

## One-time server setup

1. Create the deploy dir, e.g. `/opt/gang-chat`, owned by the deploy user.
2. Put a **Linux** `livekit-server` binary in it (the repo only ships the
   Windows `.exe`). Download it from livekit.io, name it `livekit-server`,
   `chmod +x`.
3. Create `.env` in the deploy dir with the real secrets:
   ```
   GANG_JWT_SECRET=<long random>
   GANG_DATABASE_URL=gang-chat.db
   GANG_BIND=0.0.0.0:21116
   LIVEKIT_HOST=http://localhost:7880
   LIVEKIT_API_KEY=<key>
   LIVEKIT_API_SECRET=<secret>
   ```
4. For production asset storage on S3-compatible storage, keep the real
   credentials only in this server-side `.env`:
   ```
   # Optional: S3 is selected automatically when the endpoint/bucket/credentials
   # below are present. Set GANG_STORAGE_BACKEND=local only to force local disk.
   GANG_STORAGE_BACKEND=s3
   GANG_ASSET_OBJECT_PREFIX=assets
   # Leave empty to serve private bucket assets through the API. Set only for a public bucket/CDN.
   GANG_ASSET_PUBLIC_BASE_URL=
   # Optional: controls Cache-Control max-age and Expires when GANG_ASSET_CACHE_CONTROL is unset.
   GANG_ASSET_CACHE_TTL_SECONDS=31536000
   GANG_S3_ENDPOINT=https://os.ky-z.com:9000
   GANG_S3_BUCKET=gang-chat
   GANG_S3_REGION=us-east-1
   GANG_S3_ACCESS_KEY_ID=gang-chat
   GANG_S3_SECRET_ACCESS_KEY=<secret key>
   GANG_S3_SESSION_TOKEN=
   GANG_S3_FORCE_PATH_STYLE=true
   ```
   With `GANG_ASSET_PUBLIC_BASE_URL` empty, uploads are written to S3 and asset
   reads pass through the backend using server-side credentials. Set
   `GANG_ASSET_PUBLIC_BASE_URL` to a public bucket or CDN root only when direct
   browser reads should bypass the backend. Asset responses include
   `Cache-Control`, `Expires`, `ETag`, and `Last-Modified`; set
   `GANG_ASSET_CACHE_CONTROL` to override the generated
   `public, max-age=<ttl>, immutable` policy.
5. (Optional) `deploy.env` for path overrides — see `deploy.env.example`.
6. First boot: `./start.sh all`.

After that, every push to `master` rebuilds and `./restart.sh gang`
automatically. livekit is left running across deploys; restart it manually
(`./restart.sh livekit`) when you change `livekit.yaml`.

## Required Gitea Actions secrets

Set under **Repo → Settings → Actions → Secrets**:

| Secret           | Meaning                                            |
|------------------|----------------------------------------------------|
| `DEPLOY_HOST`    | server hostname / IP                               |
| `DEPLOY_USER`    | SSH user                                           |
| `DEPLOY_PATH`    | deploy dir on the server, e.g. `/opt/gang-chat`    |
| `DEPLOY_SSH_KEY` | private SSH key (the matching pubkey is in the user's `authorized_keys`) |
| `DEPLOY_PORT`    | SSH port (optional, defaults to 22)                |
