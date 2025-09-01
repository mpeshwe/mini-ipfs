# mini-IPFS
mini-IPFS
=========

Tiny, hackable, content-addressed storage with a Kademlia-like DHT for provider discovery. Nodes store data in fixed-size chunks (SHA-256), announce themselves as providers, and fetch chunks from peers over HTTP using the DHT to discover providers.

Highlights
- Content-addressed chunks: SHA-256, deduplicated by hash.
- Kademlia-inspired DHT: announce providers, find providers, iterative lookup.
- Simple HTTP API: store and fetch chunks; health and status endpoints.
- Docker-ready: spin up a multi-node cluster with Docker Compose.
- Pluggable config: environment variables via Viper with sensible defaults.

Status
- Working: chunk store, provider announce, provider lookup + network fetch with integrity verification, local caching on fetch.
- Planned: file (multi-chunk) upload/download endpoints with manifests, live fetch provenance, minimal web UI to demo multi-node fetch.

Architecture (quick tour)
- internal/storage
  - `ChunkStore`: persists chunks under `data/<hh>/<hash>` with integrity checks.
  - `Chunker`: splits bytes into fixed-size chunks; will be used for whole-file support.
- internal/dht
  - `dhtNode`: maintains routing table, RPC server/client, provider store (in-memory), and implements `StoreProvider`/`FindProviders`.
  - `RoutingTable`: buckets, XOR distance, closest-K queries.
  - RPC: line-delimited JSON over TCP with message types (PING, FIND_NODE, STORE_PROVIDER, FIND_PROVIDERS).
- internal/api
  - HTTP API server (Gorilla Mux) exposing health/status and chunk endpoints.

Requirements
- Go 1.22+
- Docker and Docker Compose (for multi-node demo)

Getting Started (local)
1) Build
```
go build -o bin/mini-ipfs-node ./cmd/node
```

2) Run 3 nodes in separate terminals
```
# Terminal 1 (node1, bootstrap)
MINI_IPFS_NODE_ID=node1 \
MINI_IPFS_LOGGING_LEVEL=debug \
MINI_IPFS_NODE_ADVERTISE_HOST=127.0.0.1 \
MINI_IPFS_NODE_HTTP_ADDR=127.0.0.1:8080 \
MINI_IPFS_NODE_DHT_ADDR=127.0.0.1:7000 \
MINI_IPFS_NODE_DATA_DIR=$(pwd)/data/node1 \
./bin/mini-ipfs-node

# Terminal 2 (node2)
MINI_IPFS_NODE_ID=node2 \
MINI_IPFS_LOGGING_LEVEL=debug \
MINI_IPFS_NODE_ADVERTISE_HOST=127.0.0.1 \
MINI_IPFS_NODE_HTTP_ADDR=127.0.0.1:8081 \
MINI_IPFS_NODE_DHT_ADDR=127.0.0.1:7001 \
MINI_IPFS_DHT_BOOTSTRAP_NODES=127.0.0.1:7000 \
MINI_IPFS_NODE_DATA_DIR=$(pwd)/data/node2 \
./bin/mini-ipfs-node

# Terminal 3 (node3)
MINI_IPFS_NODE_ID=node3 \
MINI_IPFS_LOGGING_LEVEL=debug \
MINI_IPFS_NODE_ADVERTISE_HOST=127.0.0.1 \
MINI_IPFS_NODE_HTTP_ADDR=127.0.0.1:8082 \
MINI_IPFS_NODE_DHT_ADDR=127.0.0.1:7002 \
MINI_IPFS_DHT_BOOTSTRAP_NODES=127.0.0.1:7000,127.0.0.1:7001 \
MINI_IPFS_NODE_DATA_DIR=$(pwd)/data/node3 \
./bin/mini-ipfs-node
```

3) Health checks
```
curl -s http://127.0.0.1:8080/health | jq .
curl -s http://127.0.0.1:8081/health | jq .
curl -s http://127.0.0.1:8082/health | jq .
```

4) Store a chunk on node1 and fetch from node2/node3
```
HASH=$(printf 'Hello distributed world!' | \
  curl -s -X POST --data-binary @- http://127.0.0.1:8080/api/v1/chunk | \
  sed -E 's/.*"hash":"([0-9a-f]{64})".*/\1/')
echo "$HASH"

curl -sS "http://127.0.0.1:8081/api/v1/chunk/$HASH?remote=1" -o /tmp/out2
printf 'Hello distributed world!' | diff -q /tmp/out2 - || echo "mismatch"

curl -sS "http://127.0.0.1:8082/api/v1/chunk/$HASH?remote=1" -o /tmp/out3
printf 'Hello distributed world!' | diff -q /tmp/out3 - || echo "mismatch"

# Subsequent GETs without ?remote=1 now return 200 from local cache
curl -i "http://127.0.0.1:8081/api/v1/chunk/$HASH" | head -n1
```

Docker Compose (multi-node demo)
1) Build image and start the cluster
```
docker build -f build/Dockerfile -t mini-ipfs:dev .
docker compose -f build/compose.yml up -d
```

2) Logs & health
```
docker compose -f build/compose.yml logs -f node1 node2 node3
curl -s http://localhost:8081/health | jq .
curl -s http://localhost:8082/health | jq .
curl -s http://localhost:8083/health | jq .
```

3) Store and fetch (host → containers)
```
HASH=$(printf 'Hello distributed world!' | \
  curl -s -X POST --data-binary @- http://localhost:8081/api/v1/chunk | \
  sed -E 's/.*"hash":"([0-9a-f]{64})".*/\1/')

curl -sS "http://localhost:8082/api/v1/chunk/$HASH?remote=1" -o /tmp/out2
printf 'Hello distributed world!' | diff -q /tmp/out2 - || echo "mismatch"

curl -sS "http://localhost:8083/api/v1/chunk/$HASH?remote=1" -o /tmp/out3
printf 'Hello distributed world!' | diff -q /tmp/out3 - || echo "mismatch"
```

HTTP API
- `GET /health` – service health + storage stats
- `GET /api/v1/status` – node configuration snapshot & storage stats
- `POST /api/v1/chunk` – store raw chunk (request body as bytes)
  - Response: `{ "hash": "<sha256>", "size": <int> }`
- `GET /api/v1/chunk/{hash}` – get chunk by hash (hex)
  - Query: `remote=1` to allow searching the network via DHT + HTTP
  - Response: `200 application/octet-stream` on success; `404` if not found locally (without `remote=1`)

Configuration
Configuration is loaded via environment variables with the `MINI_IPFS_` prefix. Defaults are set in code.

Key settings
- Node
  - `MINI_IPFS_NODE_ID` – unique ID label for logs and DHT identity seed (auto-generated if empty)
  - `MINI_IPFS_NODE_HTTP_ADDR` – HTTP bind address (e.g., `:8080`, `127.0.0.1:8080`)
  - `MINI_IPFS_NODE_DHT_ADDR` – DHT bind address (e.g., `:7000`, `127.0.0.1:7000`)
  - `MINI_IPFS_NODE_ADVERTISE_HOST` – hostname/IP to advertise to peers (strongly recommended in container networks)
  - `MINI_IPFS_NODE_DATA_DIR` – data directory (default `./data`)
- DHT
  - `MINI_IPFS_DHT_K` – bucket size / result size (default 20)
  - `MINI_IPFS_DHT_ALPHA` – lookup parallelism (default 3)
  - `MINI_IPFS_DHT_BOOTSTRAP_NODES` – comma-separated `host:port` list
- Storage
  - `MINI_IPFS_STORAGE_CHUNK_SIZE` – bytes per chunk (default 1 MiB)
  - `MINI_IPFS_STORAGE_MAX_STORAGE_BYTES` – disk cap (default 10 GiB)
- Logging
  - `MINI_IPFS_LOGGING_LEVEL` – e.g., `debug`, `info`
  - `MINI_IPFS_LOGGING_FORMAT` – `json` or `console`

Notes
- DHT identity derives from `NODE_ID` (or falls back to bind address). Ensure each node has a distinct `NODE_ID`.
- Nodes advertise a reachable DHT address using `NODE_ADVERTISE_HOST`; in Docker Compose this is the service name (already wired in `build/compose.yml`).
- When a node fetches a chunk via the network, it caches it locally; subsequent GETs (without `?remote=1`) return 200 from cache.

Roadmap
- File endpoints (`/api/v1/file`): chunk whole files, store a manifest, reconstruct on fetch.
- “Announce on cache”: make fetched+cached nodes become providers automatically (configurable).
- Simple web UI: upload a file, fetch by manifest hash, and visualize chunk provenance (which nodes served which chunks).

Troubleshooting
- Port in use: change `HTTP_ADDR`/`DHT_ADDR` or stop the other process.
- "no providers found": ensure bootstrap nodes are set and nodes have completed bootstrap (check logs), and that the provider node has announced (POST logged “Provider record stored”).
- HTTP reachability: if cross-host or container-to-container, set `NODE_ADVERTISE_HOST` to a resolvable host.

Contributing
PRs/issues welcome. The goal is clarity over completeness—keep it small, readable, and demo-friendly.
