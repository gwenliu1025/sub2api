# Sub2API Docker Image

Sub2API is an AI API Gateway Platform for distributing and managing AI product subscription API quotas.

## Quick Start

```bash
docker run -d \
  --name sub2api \
  -p 8080:8080 \
  -e DATABASE_URL="postgres://user:pass@host:5432/sub2api" \
  -e REDIS_URL="redis://host:6379" \
  ghcr.io/gwenliu1025/sub2api:0.2.7
```

## Docker Compose

```yaml
version: '3.8'

services:
  sub2api:
    image: ghcr.io/gwenliu1025/sub2api:0.2.7
    ports:
      - "8080:8080"
    environment:
      - DATABASE_URL=postgres://postgres:postgres@db:5432/sub2api?sslmode=disable
      - REDIS_URL=redis://redis:6379
    depends_on:
      - db
      - redis

  db:
    image: postgres:18-alpine
    environment:
      - POSTGRES_USER=postgres
      - POSTGRES_PASSWORD=postgres
      - POSTGRES_DB=sub2api
    volumes:
      - postgres_data:/var/lib/postgresql/data

  redis:
    image: redis:8-alpine
    volumes:
      - redis_data:/data

volumes:
  postgres_data:
  redis_data:
```

## Startup and Database Recovery

Sub2API runs database migrations while starting. PostgreSQL may still be
recovering briefly after a host or Docker daemon restart. The application
retries transient PostgreSQL startup and connection errors with bounded
exponential backoff, then continues startup when the database is ready.
Permanent errors such as invalid credentials, migration checksum mismatches,
SQL errors, and incompatible data fail immediately.

The Compose deployment also checks PostgreSQL readiness with both `pg_isready`
and a simple SQL query. `depends_on: condition: service_healthy` helps order a
fresh Compose start, but application-level retries are still required when
Docker restores existing containers after a host restart.

## Environment Variables

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `DATABASE_URL` | PostgreSQL connection string | Yes | - |
| `REDIS_URL` | Redis connection string | Yes | - |
| `PORT` | Server port | No | `8080` |
| `GIN_MODE` | Gin framework mode (`debug`/`release`) | No | `release` |

## Supported Architectures

- `linux/amd64`
- `linux/arm64`

## Tags

- `0.2.7` - fork 的精确发布版本；不提供浮动别名

## Links

- [GitHub Repository](https://github.com/gwenliu1025/sub2api)
- [Documentation](https://github.com/gwenliu1025/sub2api#readme)
