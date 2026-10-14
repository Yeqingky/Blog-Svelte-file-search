# Repository Instructions

## Project

This repository contains a standalone Go HTTP service that exposes allowlisted file search over a Meilisearch index. The service uses only the Go standard library.

## Development

- Run focused checks with `go test ./...` and `go build ./...`.
- Copy `.env.example` to an untracked `.env` and configure the service before running `go run .`.
- Keep search credentials out of source files, examples, logs, and test fixtures.
- Keep configured service and browser origins on `example.com` in committed content.
- Search results must remain scoped to configured `SHARE_<sid>` paths and must not expose indexed physical parent paths.
- Docker build context must exclude `.env*`, tests, and local docs; credentials must not enter image layers.
- GHCR publishing uses the Actions secret `GH_PKG`, publishes `sha-<short>` on `main`, and `vX.Y.Z` plus `latest` for version tags.

## Runtime

- `/search` is the public search endpoint; `/healthz` probes Meilisearch.
- The default listener is loopback-only. A local reverse proxy may set `X-Real-IP`; the service trusts that header only when the TCP peer is loopback.
- Configured roots and their hashes are normalized and precomputed at startup. File roots and roots with no indexed direct children produce warnings.
- Production Compose uses the GHCR image; `docker-compose-dev.yml` builds locally. Both inject the untracked `.env`, listen on `0.0.0.0:8099`, and publish host port `8099` on all interfaces; apply firewall or proxy restrictions as needed.
