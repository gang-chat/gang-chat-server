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
3. Create `config.json` in the deploy dir with the real config and secrets.
   The server reads `config.json` from the current working directory by default.
   ```json
   {
     "bind": "0.0.0.0:21116",
     "database_url": "gang_chat:password@tcp(127.0.0.1:3306)/gang_chat?parseTime=true&charset=utf8mb4&loc=Local",
     "jwt_secret": "<long random>",
     "s3_endpoint": "https://os.ky-z.com:9000",
     "s3_bucket": "gang-chat",
     "s3_region": "us-east-1",
     "s3_access_key_id": "gang-chat",
     "s3_secret_access_key": "<secret key>",
     "s3_session_token": "",
     "s3_force_path_style": true,
     "livekit_host": "http://localhost:7880",
     "livekit_api_key": "<key>",
     "livekit_api_secret": "<secret>"
   }
   ```
4. Asset uploads always go to S3-compatible storage. Asset reads pass through
   the backend using server-side credentials. Object keys are fixed as
   `assets/<asset_id>/<filename>`, and asset responses include fixed
   `Cache-Control`, `Expires`, `ETag`, and `Last-Modified` headers.
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
