# Embedding service, bulk tagging and duplicates

This page covers the shared embedding server, the agent tagging workflow
(review queue claims, tag modes, provenance) and duplicate clusters. It
extends the "Semantic search" section of the README.

## One embedding server per machine

Semantic search needs the EmbeddingGemma 2 server (`modon embed-server`).
Every `modon serve` (one per MCP client) and every CLI call shares a single
server:

- `modon embed-server` holds a lock (`embed-server.lock` in the cache
  directory) for its whole life. A second one exits at once, so concurrent
  starters never load the model twice. It also refuses to start when
  something already answers at `embedding.endpoint`.
- Clients with `embedding.autostart: true` reuse a running server. When none
  answers they start the systemd user unit (if installed), otherwise a
  detached `modon embed-server` that outlives the client. Meanwhile they
  fall back to keyword search, then pick the server up within seconds.
- Health checks expire: a healthy server is re-checked after 10 s, a down
  one after 3 s, and `index_stats` always probes. A dead or restarted
  server is noticed without restarting clients.

### systemd user unit

```sh
modon service install          # write ~/.config/systemd/user/modon-embed.service,
                               # then systemctl --user enable --now
modon service install --replace  # first stop an ad-hoc embed-server that holds the lock
modon service status           # systemctl status + a live health check (device, OOM count)
modon service print            # print the unit without installing
modon service uninstall
```

The unit runs `modon embed-server` with `Restart=always`. Paths under your
home directory are written with systemd's `%h`, and the directory holding
`uv` is put on `PATH`. It looks like this:

```ini
[Service]
ExecStart=%h/.local/bin/modon embed-server
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=always
RestartSec=30
Nice=5
```

To run without systemd (no user session, macOS): `modon embed-server
--detach` starts it in the background (log: `embed_server.log` in the cache
directory).

### Sharing the GPU

The server is built to coexist with games and editors on the same GPU:

- **Out of memory during a request:** it empties the CUDA cache and retries
  with half the batch, down to one input; if that still fails, the request
  runs on CPU and the model stays there for two minutes. It moves back to
  the GPU once enough memory is free. Every step is logged and counted
  (`oom_events` in `modon service status` and `index_stats`).
- **Idle:** after `embedding.idle_offload_seconds` (default 600; negative =
  never) without requests the model leaves the GPU; the next request brings
  it back.
- **Errors:** 400 means bad input (not retried), 503 means the GPU stayed
  out of memory. Clients retry 503s and lost connections with backoff
  (restarting a vanished server when autostart is on) and split a failing
  batch in halves, so one unreadable image fails alone instead of the whole
  batch.

## Bulk tagging with several agents

Agent-written metadata is the best search signal: descriptions, and agent
categories and tags, are embedded into the text vectors. The review queue
lets several agents tag a large library in parallel without overlap.

### 1. Claim a batch

MCP `list_unclassified`:

```json
{"worker": "agent-3", "limit": 25, "one_per_cluster": true, "lease_seconds": 900}
```

CLI: `modon unclassified --worker agent-3 --limit 25 --one-per-cluster --lease 15m`

- `worker`: any id unique to the agent. The returned assets are claimed
  until the lease ends; other workers (and plain listings) skip them, so
  parallel agents get disjoint batches. Claims are taken in one write
  transaction across processes.
- `one_per_cluster`: one representative per duplicate cluster; its copies
  are listed in `duplicates` and claimed too.
- `offset`: plain paging when not claiming. `remaining` counts unclaimed
  assets still in review.
- `release: true` (CLI `--release`) gives back the worker's unfinished
  claims. Expired claims return to the pool by themselves.

### 2. Write metadata

MCP `update_asset_metadata`:

```json
{"id": "local:42", "description": "Wooden market crate with rope handles",
 "category": "container", "tags": ["crate", "wooden", "market"],
 "tags_mode": "replace", "remove_tags": ["fbx"], "apply_to_duplicates": true}
```

CLI: `modon update --category container --tags crate,wooden,market --tags-mode replace --duplicates local:42`

- `tags_mode`: `merge` (default) adds to the current tags; `replace` makes
  `tags` the complete list (an empty list clears it). `remove_tags` deletes
  tags afterwards, case-insensitively. Multi-word tags become
  `wooden-crate` (still matched by both words).
- `ids`: apply the same edit to several assets.
- `apply_to_duplicates`: also edit every copy in each asset's cluster.
- `needs_review` clears once there is a description, or an agent category
  plus agent tags; `reviewed: true/false` forces it. Updating an asset
  releases its claim, and its vectors are refreshed right away (only that
  asset is re-embedded).

### Provenance

Each metadata field records who wrote it, shown as `provenance` on assets:
`agent` (update_asset_metadata / `modon update`), `auto` (zero-shot
`modon embed --classify --apply`) or absent (importer heuristics).
Re-importing a folder refreshes heuristic values but never overwrites agent
values; an agent's tag list is kept verbatim, so removed tags stay removed.
Zero-shot values are kept until the importer has a value of its own.

## Duplicate clusters

Libraries often ship each model several times: FBX and OBJ exports of one
mesh, or the same prop in several packs. modon groups them:

- same normalized name (case and separators ignored, variant numbers
  kept) **and** the same pack (the folder named like `POLYGON - X Pack`,
  or the model's folder minus `FBX`/`OBJ`/`Source` plumbing), or
- same normalized name and near-identical preview vectors (cosine >= 0.965)
  across packs.

Clusters are recomputed after every full `modon embed` and import, or with
`modon dedupe`. `search_assets` collapses each cluster into its best-ranked
member, which carries `cluster_id` and `duplicates`; pass
`expand_duplicates: true` (CLI `--expand`) for every copy.
`get_asset_details` lists the copies of an asset.

## Search ranking notes

Local search fuses three ranked lists (keyword, text vector, preview image
vector) with reciprocal rank fusion, plus a lexical boost:

- Keyword candidates are ranked by **IDF-weighted query coverage**: rare
  query words count more, matches in the name count more than matches in
  tags or descriptions, whole words more than prefixes, and the full query
  as a phrase in the name earns a bonus.
- The squared coverage is added to the fused score, so an asset naming a
  rare query word competes with assets only the vector signals favour.
  Keyword-only partial matches get a quarter of the boost.
- Names are indexed split ("WallLantern" also matches "wall lantern"), and
  the pack name is keyword-searchable.

Heuristic categories, tags and pack names are deliberately *not* embedded
into the text vectors: on a 24.5k-asset library they lowered precision
(descriptive queries 0.69 -> 0.63, synonym queries 0.65 -> 0.50).
