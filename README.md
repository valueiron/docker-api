# docker-api

A production-ready Go REST API that exposes a simplified wrapper around the Docker Engine API. Supports container lifecycle management, images, volumes, networks, system info, and per-container resource metrics from a single distroless binary with no CLI dependency.

## Stack

| Component | Choice |
|-----------|--------|
| Language | Go 1.24 |
| Router | gorilla/mux |
| Docker SDK | github.com/docker/docker/client v26 |
| Runtime image | gcr.io/distroless/static-debian12:nonroot |

## Project structure

```
docker-api/
├── main.go                   # Entry point, router, graceful shutdown, logging middleware
├── handlers/
│   ├── health.go             # GET /health
│   ├── containers.go         # Container lifecycle + logs endpoints
│   ├── metrics.go            # GET /containers/{id}/metrics
│   ├── images.go             # Image management endpoints
│   ├── volumes.go            # Volume management endpoints
│   ├── networks.go           # Network management endpoints
│   └── system.go             # GET /system/info, GET /system/disk
├── openapi.json              # OpenAPI 3.0 specification
├── go.mod
├── Dockerfile                # Multi-stage: golang:1.24-alpine → distroless/static
├── docker-compose.yml
└── README.md
```

## Endpoints

### Health

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness probe — returns `{"status":"ok"}` |

### Containers

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/containers` | List all containers (running and stopped) |
| `GET` | `/containers/{id}` | Inspect a container (full Docker ContainerJSON) |
| `DELETE` | `/containers/{id}` | Remove a container (`?force=1` to force) |
| `POST` | `/containers/start/{id}` | Start a container by ID or name |
| `POST` | `/containers/stop/{id}` | Stop a container by ID or name |
| `POST` | `/containers/restart/{id}` | Restart a container by ID or name |
| `GET` | `/containers/{id}/logs` | Tail logs (`?tail=100&since=...&until=...`) |
| `GET` | `/containers/{id}/metrics` | Resource usage snapshot (CPU, memory, network I/O, block I/O, PIDs) |

### System

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/system/info` | Docker daemon and host info (version, OS, driver, memory, CPUs, etc.) |
| `GET` | `/system/disk` | Disk usage for images, containers, volumes, and build cache |

### Images

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/images` | List all images |
| `GET` | `/images/{id}` | Inspect an image by ID or name:tag |
| `DELETE` | `/images/{id}` | Remove an image (`?force=1` to force) |
| `POST` | `/images/pull` | Pull an image (`?image=name:tag`) |

### Volumes

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/volumes` | List all volumes |
| `POST` | `/volumes` | Create a volume |
| `POST` | `/volumes/prune` | Remove unused volumes |
| `GET` | `/volumes/{name}` | Inspect a volume |
| `DELETE` | `/volumes/{name}` | Remove a volume (`?force=1` to force) |

### Networks

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/networks` | List all networks |
| `POST` | `/networks` | Create a network |
| `POST` | `/networks/prune` | Remove unused networks |
| `GET` | `/networks/{id}` | Inspect a network |
| `DELETE` | `/networks/{id}` | Remove a network |
| `POST` | `/networks/{id}/connect` | Connect a container to a network |
| `POST` | `/networks/{id}/disconnect` | Disconnect a container from a network |

Full schema, response shapes and error codes are documented in `openapi.json`.

## Running locally

```bash
# Fetch dependencies and generate go.sum
go mod tidy

# Run the server (requires a running Docker daemon)
go run .
```

The server listens on `:8080` by default. Override with the `PORT` environment variable:

```bash
PORT=9090 go run .
```

## Building and running with Docker

```bash
# Build the image
docker build -t docker-api .

# Run the API (mount Docker socket; --user 0:0 for socket access)
docker run -d --name docker-api -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --user 0:0 \
  docker-api

# View logs
docker logs -f docker-api

# Stop and remove
docker stop docker-api && docker rm docker-api
```

Or use Docker Compose:

```bash
docker compose up --build -d
docker compose logs -f
docker compose down
```

## Example requests

### Health

```bash
curl -s http://localhost:8080/health
# {"status":"ok"}
```

### Containers

```bash
# List all containers
curl -s http://localhost:8080/containers | jq .

# Inspect a container
curl -s http://localhost:8080/containers/my-container | jq .

# Start a container
curl -s -X POST http://localhost:8080/containers/start/my-container
# {"id":"my-container","status":"started"}

# Stop a container
curl -s -X POST http://localhost:8080/containers/stop/my-container
# {"id":"my-container","status":"stopped"}

# Restart a container
curl -s -X POST http://localhost:8080/containers/restart/my-container
# {"id":"my-container","status":"restarted"}

# Remove a container (add ?force=1 to remove a running container)
curl -s -X DELETE http://localhost:8080/containers/my-container
# {"id":"my-container","status":"removed"}

# Tail the last 50 lines of logs
curl -s "http://localhost:8080/containers/my-container/logs?tail=50"
```

### Metrics

```bash
# One-shot resource snapshot (CPU %, memory, network I/O, block I/O, PIDs)
curl -s http://localhost:8080/containers/my-container/metrics | jq .
# {
#   "container_id": "abc123",
#   "name": "/my-container",
#   "cpu_percent": 0.42,
#   "mem_usage_mib": 34.5,
#   "mem_limit_mib": 512,
#   "mem_percent": 6.74,
#   "net_rx_mib": 0.002,
#   "net_tx_mib": 0.001,
#   "block_read_mib": 0,
#   "block_write_mib": 0,
#   "pids": 8
# }
```

Memory reports RSS (page cache excluded), matching `docker stats`.

### System

```bash
# Docker daemon and host info
curl -s http://localhost:8080/system/info | jq .

# Disk usage breakdown
curl -s http://localhost:8080/system/disk | jq .
```

### Images

```bash
# List images
curl -s http://localhost:8080/images | jq .

# Pull an image
curl -s -X POST "http://localhost:8080/images/pull?image=alpine:latest"
# {"status":"pulled","image":"alpine:latest"}

# Inspect an image
curl -s http://localhost:8080/images/alpine:latest | jq .

# Remove an image
curl -s -X DELETE http://localhost:8080/images/alpine:latest
# {"id":"alpine:latest","status":"removed"}
```

### Volumes

```bash
# List volumes
curl -s http://localhost:8080/volumes | jq .

# Create a volume
curl -s -X POST http://localhost:8080/volumes \
  -H "Content-Type: application/json" \
  -d '{"name":"my-volume","driver":"local"}'

# Inspect a volume
curl -s http://localhost:8080/volumes/my-volume | jq .

# Remove a volume
curl -s -X DELETE http://localhost:8080/volumes/my-volume

# Prune unused volumes
curl -s -X POST http://localhost:8080/volumes/prune | jq .
```

### Networks

```bash
# List networks
curl -s http://localhost:8080/networks | jq .

# Create a network
curl -s -X POST http://localhost:8080/networks \
  -H "Content-Type: application/json" \
  -d '{"name":"my-net","driver":"bridge"}'

# Inspect a network
curl -s http://localhost:8080/networks/my-net | jq .

# Connect a container to a network
curl -s -X POST http://localhost:8080/networks/my-net/connect \
  -H "Content-Type: application/json" \
  -d '{"container":"my-container"}'

# Disconnect a container from a network
curl -s -X POST http://localhost:8080/networks/my-net/disconnect \
  -H "Content-Type: application/json" \
  -d '{"container":"my-container","force":false}'

# Remove a network
curl -s -X DELETE http://localhost:8080/networks/my-net

# Prune unused networks
curl -s -X POST http://localhost:8080/networks/prune | jq .
```

## Health probe

The compiled binary doubles as its own container health check client via the `--healthcheck` flag, so no `curl` or `wget` is needed in the distroless runtime image:

```
HEALTHCHECK CMD ["/docker-api", "--healthcheck"]
```

## Security considerations

- **Non-root user:** The container runs as UID 65532 (`nonroot`).
- **Socket mount:** `/var/run/docker.sock` is mounted so the API can talk to the Docker daemon.
- **Minimal image:** The distroless runtime contains only the statically compiled binary — no shell, no package manager, no libc.
- **Static binary:** Built with `CGO_ENABLED=0`; no shared library dependency.
- **For stricter production deployments** consider fronting the Docker socket with [Tecnativa docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) to allow-list only the specific API calls this service requires.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | TCP port the server listens on |
| `DOCKER_HOST` | (from env) | Docker daemon socket (`unix:///var/run/docker.sock`) |
