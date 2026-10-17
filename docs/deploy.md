# Deployment (one VPS)

The server runs two containers: `postgres` and `lexai`. Files are in Cloudflare R2. Cloudflare sits in front of the site.

## 1. Cloudflare

1. **R2:**
   - Create the bucket `lexai-files`.
   - Create an API token with Object Read & Write on that bucket.
   - Note the account id. The S3 endpoint is `<account_id>.r2.cloudflarestorage.com`.
2. **Public PDFs (optional):** connect a custom domain such as `files.example.pl` to `lexai-files`. Original PDFs are public documents, and Cloudflare caches them. Raw downloads are stored under the same `raw/` prefix. If you'd rather not expose them, keep the bucket private and leave `LEXAI_FILES_BASE_URL` empty.
3. **Backups bucket:** create `lexai-backup` for Postgres backups. Ideally also copy files weekly to a second provider (`rclone sync`).
4. **Site:**
   - Proxy the domain through Cloudflare (orange cloud), with SSL mode **Full (strict)**.
   - Create an **Origin CA certificate** (SSL/TLS → Origin Server). Save it as `certs/origin.pem` and `certs/origin.key` on the server.

## 2. Server

The plan starts with 8 vCPU, 32 GB RAM and 0.5 TB NVMe.

```sh
# as root, once
apt install -y docker.io docker-compose-v2 ufw
ufw default deny incoming
ufw allow OpenSSH
ufw allow 443/tcp
# Note: Docker-published ports bypass ufw. To accept 443 only from Cloudflare
# IP ranges, add the rules to the DOCKER-USER iptables chain instead.
ufw enable

git clone https://github.com/lluzak/lex-ai.git && cd lex-ai
cp .env.example .env     # fill in: domain, R2 keys, POSTGRES_PASSWORD, PG_SHARED_BUFFERS=8GB, PG_EFFECTIVE_CACHE_SIZE=22GB
mkdir certs && $EDITOR certs/origin.pem certs/origin.key
docker compose up -d --build
docker compose logs -f lexai
```

`lexai` runs migrations on start, then serves on :443 (mapped from container port 8443), with the workers and the nightly delta job included.

Start the historical import once:

```sh
docker compose exec lexai lexai backfill sejmeli -pub DU,MP -from 1918
```

The import is limited by `LEXAI_SOURCE_RPS` (polite request rate to the source) and is resumable: jobs live in Postgres.

## 3. Releases

```sh
git pull && docker compose up -d --build lexai
```

The restart takes about a second. Migrations run before the server starts.

## 4. Backups (to do)

Postgres is the only state on the server.

- **Planned:** wal-g continuous WAL archiving plus a nightly base backup to `lexai-backup` on R2. That needs a Postgres image with wal-g; not wired up yet.
- **Until then:** a nightly `pg_dump` uploaded to R2 is the minimum:

  ```sh
  docker compose exec -T postgres pg_dump -U lexai -Fc lexai > lexai-$(date +%F).dump
  rclone copy lexai-$(date +%F).dump r2:lexai-backup/pg_dump/
  ```

- **Test a restore every month.**
