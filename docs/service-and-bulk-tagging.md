# Embedding service, bulk tagging and duplicates

This page covers the shared embedding server, the agent tagging workflow
(review queue claims, suggestions, the tag vocabulary, batch writes,
provenance and reclassification) and duplicate clusters. It
extends the "Semantic search" section of the README.

## One embedding server per machine

Semantic search needs the EmbeddingGemma 2 server (`modon embed-server`).
Every `modon serve` (one per MCP client) and every CLI call shares a single
server:

- `modon embed-server` holds a lock (`embed-server.lock` in the cache
  directory) for its whole life. A second one exits at once, so concurrent
  starters never load the model twice. It also refuses to start when
  something already answers at `embedding.endpoint`.
- The server runs only while it is used. Clients with
  `embedding.autostart: true` reuse a running server; when none answers and
  a call needs embeddings (search, similar, suggest, embedding), they start
  the systemd user unit (if installed), otherwise a detached
  `modon embed-server` that outlives the client, and wait up to 60 s for it
  (the model loads in ~15 s; the first run installs torch and falls back to
  keyword search meanwhile). `modon serve` itself never starts it.
- **Idle exit:** after `embedding.idle_exit_seconds` (default 900; negative
  = never) without embedding requests the server shuts down, freeing its
  RAM. Health checks do not count as use.
- Health checks expire: a healthy server is re-checked after 10 s, a down
  one after 3 s, and `index_stats` always probes. A dead or restarted
  server is noticed without restarting clients.

### systemd user unit

```sh
modon service install          # write ~/.config/systemd/user/modon-embed.service
                               # (not enabled: started on demand, stops when idle)
modon service install --replace  # first stop an ad-hoc embed-server that holds the lock
modon service status           # systemctl status + a live health check (device, OOM count)
modon service print            # print the unit without installing
modon service uninstall
```

The unit runs `modon embed-server` with `Restart=on-failure`, so a crash is
restarted but an idle exit is not. It has no `[Install]` section and is never
started at login; reinstalling disables units from older versions that
were. Paths under your
home directory are written with systemd's `%h`, and the directory holding
`uv` is put on `PATH`. It looks like this:

```ini
[Service]
ExecStart=%h/.local/bin/modon embed-server
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=on-failure
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
- `release: true` (CLI `--release`, with `--json` for
  `{"worker", "released"}`) gives back the worker's unfinished claims.
  Expired claims return to the pool by themselves.
- `with_suggestions: true` (CLI `--with-suggest`, JSON output) inlines
  the step-2 suggestions in each asset as `suggestions`, so one call
  returns a claimed batch ready to tag. `suggest_top` (CLI
  `--suggest-top`) sets the list length. Without an embedding server
  the batch still comes back, with `suggest_error`.

### 2. Read the suggestions

MCP `suggest_metadata` (`{"ids": ["local:42", "local:43"], "top_k": 3}`),
CLI `modon suggest [--top N] local:42 local:43`. Per asset:

- `categories`, `styles`: zero-shot matches of the preview image and
  text document against the category and style labels, with a
  `margin` between the first two categories (`confident` above 0.05).
- `tags`: the best terms of each facet of the recommended vocabulary
  (below), with their `facet`. Zero-shot scores are small numbers that
  only rank; materials and conditions read from a thumbnail are hints.
- `neighbor_tags`, `neighbor_categories`: what agents already wrote on
  the eight most similar agent-tagged assets (`neighbors`, by preview
  and text similarity). `score` is the similarity-weighted share of
  neighbours with the label (1.0 = all of them), `count` how many. These
  get better as more of the library is tagged and reuse its own words;
  a copy of an already tagged model shows up as a neighbour with
  similarity near 1.

Confirm candidates against the preview (`get_preview`) before writing
them; the suggestions are signal, not answers.

### Recommended tag vocabulary

`modon suggest --vocabulary` prints it as JSON. Beyond the object nouns
(from the name and what the preview shows), tag along these facets:

| facet | about | examples |
| --- | --- | --- |
| material | dominant visible material | wood, metal, rusty-metal, stone, concrete, brick, glass, fabric, plastic, rope |
| theme | era, genre or setting | medieval, fantasy, sci-fi, modern, industrial, military, post-apocalyptic, western, nautical, office |
| condition | wear and state | clean, worn, dirty, damaged, broken, rusty, burnt, overgrown |
| use | what the piece is in a scene | prop, wall, floor, ceiling, roof, stairs, debris, signage, lighting, vegetation, weapon-part, vehicle-part, character-part, pickup, icon |
| kitbash | role when assembling scenes | hero-prop, set-dressing, modular-piece, trim, detail, filler, background, landmark, clutter |

A good tag set has the object, its parts or variants, one or two
materials, the theme, a use and a kitbash role, e.g. for a crate:
`crate, wooden, rope-handles, wood, medieval, worn, prop, container,
set-dressing, clutter`. Use lowercase; multi-word tags are hyphenated.
Write a one-sentence description too: it is embedded and makes search
by meaning work for the asset.

### 3. Write metadata

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

### Batches

Writing a batch in one go is much faster than one call per asset: every
edit is stored, then all touched assets are re-embedded together (32
documents per embedding request) instead of one request per call.

MCP: `update_asset_metadata` with `items`, a list of independent edits
with the same fields as above:

```json
{"items": [
  {"id": "local:42", "description": "Wooden market crate with rope handles",
   "category": "container", "tags": ["crate", "wood", "medieval", "set-dressing"],
   "tags_mode": "replace", "apply_to_duplicates": true},
  {"id": "local:57", "category": "wall", "tags": ["modular-piece", "brick"]}
]}
```

CLI: `modon update --batch edits.jsonl` (or `--batch -` for stdin), one
JSON object per line with the same field names; blank lines and `#`
comments are skipped and a JSON array works too. Unknown fields are
rejected with their line number. A failing edit (unknown id, nothing to
update) is reported with its position and the rest still apply; the
command then exits 1. `--json` prints the per-asset result and the
embedding stats.

A typical worker loop:

```sh
modon unclassified --worker agent-3 --limit 25 --one-per-cluster --with-suggest > batch.json
# ... look at previews, write edits.jsonl ...
modon update --batch edits.jsonl --json
modon unclassified --worker agent-3 --release --json   # when done
```

### Provenance

Each metadata field records who wrote it, shown as `provenance` on assets:
`agent` (update_asset_metadata / `modon update`), `auto` (zero-shot
`modon embed --classify --apply`) or absent (importer heuristics).
Re-importing a folder refreshes heuristic values but never overwrites agent
values; an agent's tag list is kept verbatim, so removed tags stay removed.
Zero-shot values are kept until the importer has a value of its own.

### Refreshing heuristics: `modon reclassify`

When the import heuristics improve, `modon reclassify` re-runs them on
the indexed files without needing the original import directories (files
are re-parsed; stored metadata is used for unreadable ones). It is a dry
run that lists the category moves with counts and examples; `--apply`
writes them under the re-import rules, enforced in the SQL update itself:
agent-written fields are never touched (even if an agent edits the asset
during the run), zero-shot values give way only to a real heuristic
value, and descriptions and `needs_review` stay. Folder hints come from
the path below the pack folder, or the last three folders.

The heuristics read the file name first: two-word phrases ("xmas tree"
is a decoration, "space ship" a spacecraft), then nouns (left to right
across `_` segments, last word first inside a CamelCase segment, so
`WallLantern` is a lamp), then naming-convention codes (`Chr`, `Veh`,
`Wep`, `Bld`), then folders and mesh names. Pieces of characters (hair,
beards, attachments in a character context) are `character-part`, not
`character`. `vehicle` means a land vehicle; `aircraft`, `spacecraft`
and `watercraft` are separate and keep a `vehicle` tag. "Ship",
"shuttle", "station", "rocket" and "plane" are read in context: space
words make a spacecraft, a weapon code makes a rocket a weapon, and a
plane without an air context is a flat quad.

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
