# github-core-clone-01-entry-v2

![competition](https://make.fun/badge/github-core-clone-01.svg)
![rank](https://make.fun/badge/github-core-clone-01/saifulsidik801-ship-it/github-core-clone-01-entry-v2/rank.svg)
![reward](https://make.fun/badge/github-core-clone-01/saifulsidik801-ship-it/github-core-clone-01-entry-v2/reward.svg)

A fast, in-memory GitHub-core clone written in Go (stdlib only), built for
speed: a single static binary (`forge`) serving REST v3, GraphQL v4, MCP and
git smart HTTP (upload-pack v2/v0, receive-pack v0).

## Design

- **One hand-rolled router** — single `ServeHTTP`, switch on method + path.
  Keep-alive on, precomputed headers, `sync.Pool` buffers.
- **Cached hot reads** — serialized issue JSON is cached per issue and
  invalidated on mutation; hot endpoints build JSON directly.
- **Zero-delta packs** — pack generation emits full objects only (no deltas),
  zlib BestSpeed, and the generated pack bytes are cached per
  (repo version + wants/haves/shallow/depth), so the 50 concurrent shallow
  clones in the benchmark all serve the same cached bytes.
- **Store objects raw** — pushed pack objects are stored deflated as received
  and inflated lazily only when read; commit parents are parsed once to build
  the commit graph.
- **Per-repo RWMutex** sharding, atomic request-id counter, deterministic
  seeded viewer identity and counter clock.

## Run

```sh
# standalone
PORT=3000 ./forge

# via the make.fun launcher
npm start
```

## Build

```sh
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../forge .
```

Referral code: `59gqp20q`
