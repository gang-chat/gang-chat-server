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
4. For production asset storage on Tencent COS, keep the real credentials only
   in this server-side `.env`:
   ```
   # Optional: COS is selected automatically when the COS bucket/credentials
   # below are present. Set GANG_STORAGE_BACKEND=local only to force local disk.
   GANG_STORAGE_BACKEND=
   GANG_ASSET_OBJECT_PREFIX=assets
   GANG_ASSET_PUBLIC_BASE_URL=https://<bucket-name-with-appid>.cos.<region>.myqcloud.com
   # Optional: controls Cache-Control max-age and Expires when GANG_ASSET_CACHE_CONTROL is unset.
   GANG_ASSET_CACHE_TTL_SECONDS=31536000
   GANG_COS_BUCKET=<bucket-name-with-appid>
   GANG_COS_REGION=<region, e.g. ap-shanghai>
   GANG_COS_SECRET_ID=<secret id>
   GANG_COS_SECRET_KEY=<secret key>
   GANG_COS_OBJECT_ACL=public-read
   ```
   With `GANG_ASSET_PUBLIC_BASE_URL` set, uploads are written to COS and API
   payloads return COS/CDN URLs. Asset reads do not pass through the backend and
   no local asset cache is kept. Replace the COS endpoint with a CDN root when
   CDN is enabled. Asset responses include `Cache-Control`, `Expires`, `ETag`,
   and `Last-Modified`; set `GANG_ASSET_CACHE_CONTROL` to override the generated
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
