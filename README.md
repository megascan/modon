# modon

A Go MCP server (plus CLI) that lets agents find, preview and download 3D mesh
assets from three sources, owned-first:

1. **Local index** — your own models, imported with `modon setup` / `import`
   into a SQLite FTS index with inferred categories, art-style descriptors,
   tags, animation flags and Blender/f3d thumbnails.
2. **[poly.pizza](https://poly.pizza)** — free low-poly models, via API key
   (`X-Auth-Token`, `api.poly.pizza/v1.1`).
3. **[Fab.com](https://www.fab.com)** — Epic's marketplace, via its internal
   web API. Catalog search works unauthenticated; owned-asset downloads need
   your `fab.com` session cookie.

Optional **semantic search** with [EmbeddingGemma 2](https://huggingface.co/google/embeddinggemma-2)
ranks owned assets by meaning *and by what their preview thumbnails look
like*, so `"mossy stone ruin"` finds `SM_Env_Ruin_03.fbx` even with no
description; it also powers query-by-image, "find similar / do I already own
this?" and zero-shot category/style suggestions. See
[Semantic search](#semantic-search-embeddinggemma-2).

Paid results are gated by a purchase policy (`ask` / `allow` / `deny`) plus an
optional max price, and flagged `purchase_required` so the agent **asks the
user before buying — modon never purchases on your behalf**. The same flow
covers animations and other asset types.

## Quick start

```bash
go build -o modon .
./modon setup --previews ~/Assets/Models
./modon search "low-poly wooden chair" --free
./modon serve   # MCP server over stdio (also the default with no args)
```

`setup` writes `~/.config/modon/config.json`, imports + classifies the
directory (recursive by default), and optionally renders thumbnails.
Re-run `modon import` later to pick up new files (agent-written metadata is
preserved across re-imports).

## MCP tools

| Tool | What it does |
|---|---|
| `search_assets` | Free-text + tags/category/style/type/animated/free/max-price/source/limit search, or `image` (query by picture), plus `style_reference` (image or asset id) to keep a consistent look. Owned (`local`) results come first, hybrid-ranked when semantic search is on (`score`, `matched_by`); paid hits carry `purchase_required: true`. |
| `find_similar` | Owned assets that look like / are described like `local:` (variants, duplicates) or a remote `polypizza:`/`fab:` result (*"you already own one like this"* before buying). |
| `suggest_metadata` | Zero-shot category + art-style guesses (with confidence margin) from a local asset's preview and text. |
| `embed_index` | Build/refresh vectors (incremental), optionally render missing previews and auto-fill empty categories. |
| `get_asset_details` | Full details for `local:<rowid>`, `polypizza:<id>` or `fab:<uid>`. |
| `get_preview` | Preview image inline (plus cached path). Local assets render on demand via Blender/f3d. |
| `download_asset` | `copy` (default) / `symlink` / `reference` for local; downloads remote free files. Paid/unclaimed Fab assets return `purchase_required` / `claim_required` with price + listing URL instead of files. |
| `import_assets` | The setup command as a tool: import + classify a directory, optional thumbnails. |
| `update_asset_metadata` | Enrich a local asset (description, category, style, type, tags). Clears `needs_review`. |
| `list_unclassified` | Local assets still needing rich descriptions, largest first. |
| `index_stats` | Index totals/breakdowns, semantic status (vectors, server) + which sources/credentials are configured. |

Typical agent loop: `search_assets` → `get_preview` → `find_similar` on
anything `purchase_required` (offer owned look-alikes) → ask user →
`download_asset` → `list_unclassified` + `suggest_metadata` →
`update_asset_metadata` for enrichment.

## Client config

Add to your MCP client (opencode `opencode.json`, Claude Code, etc.):

```json
{
  "mcp": {
    "modon": {
      "type": "local",
      "command": ["/path/to/modon", "serve"],
      "enabled": true,
      "environment": {
        "MODON_POLYPIZZA_API_KEY": "...",
        "MODON_FAB_COOKIE": "..."
      }
    }
  }
}
```

Anything stdio-based works: `command: ["modon"]` also serves (no args =
`serve`).

## Configuration

File: `~/.config/modon/config.json` (override with `MODON_CONFIG`).
`modon config` shows status (secrets masked); `modon config set <key> <value>`
changes it. Env vars always win for secrets:

| Key / env | Meaning |
|---|---|
| `polypizza_api_key` / `MODON_POLYPIZZA_API_KEY` (or `POLYPIZZA_AUTH_TOKEN`) | poly.pizza API key from poly.pizza settings → create app |
| `fab_cookie` / `MODON_FAB_COOKIE` | Raw `Cookie` header from a logged-in fab.com session (enables owned detection + downloads; auto-claim of free assets is opt-in via `fab_auto_claim_free`) |
| `purchase_policy` / `MODON_PURCHASE_POLICY` | `ask` (default, flag paid) · `allow` (include silently) · `deny` (exclude paid) |
| `max_price` / `MODON_MAX_PRICE` | Exclude results above this price (`0` = unlimited) |
| `currency` | Billing currency for Fab search (default `USD`) |
| `default_sources` | Search order, default `["local","polypizza","fab"]` (owned first) |
| `preview.renderer` | `auto` (default) · `blender` · `f3d` · `none` |
| `preview.size` | Thumbnail pixels (default `512`) |
| `blender_path` / `f3d_path` | Renderer binaries (default: look up in `PATH`) |
| `fab_auto_claim_free` | Claim free Fab listings into your library automatically (needs cookie) |
| `http_timeout_seconds` | Remote HTTP timeout (default `30`) |
| `preview.workers` | Parallel thumbnail renders (default `2`) |
| `embedding.endpoint` / `MODON_EMBED_ENDPOINT` | Embedding server URL, e.g. `http://127.0.0.1:8178`; empty = semantic search off |
| `embedding.autostart` | Launch the bundled sidecar (via `uv`) when nothing answers at the endpoint |
| `embedding.dims` | Matryoshka size `768` · `512` · `256` (default) · `128`; changing it re-embeds |
| `embedding.image_tokens` | Vision tokens per image, `70`–`1120` (model default `280`; lower = faster) |
| `embedding.device` | Sidecar device `cuda` · `cpu` · `mps` (default auto; CUDA OOM falls back to CPU) |
| `embedding.model` / `uv_path` / `timeout_seconds` | Model id recorded with vectors · `uv` binary · per-request timeout (default `300`) |

Paths: index DB `~/.local/share/modon/index.db` (`MODON_DB`), cache
`~/.cache/modon` (`MODON_CACHE`), downloads `~/.local/share/modon/downloads`
(`MODON_DOWNLOADS`).

## CLI reference

```
modon [serve]                  MCP server over stdio (default)
modon setup [flags] <dir>      write config, import + classify meshes
modon import [flags] <dir>     import a directory (recursive by default)
modon search [flags] <query>   search owned first, then remotes
modon details <id>             full details for an asset id
modon preview <id> [--out f]   fetch/render a preview image
modon download <id> [--dest d] [--mode copy|symlink|reference]
modon embed [flags]            build/refresh semantic vectors (incremental)
modon embed-server [flags]     run the EmbeddingGemma 2 sidecar (needs uv)
modon similar [flags] <id>     owned assets that look like <id>
modon suggest local:<id>       zero-shot category/style guesses
modon config [get|set|path]    inspect/change config (secrets masked)
modon stats                    index stats + config status
```

Search flags: `--category`, `--style`, `--type`, `--animated` / `--static`,
`--free`, `--max-price`, `--sources local,polypizza,fab`, `--limit`, `--json`,
`--image PATH|URL` (flags before the query). Embed flags: `--no-text`,
`--no-images`, `--render`, `--max-render N`, `--limit N`, `--classify
[--apply] [--margin F]`, `--eval`. Import flags: `--no-recursive`, `--previews`,
`--max-previews N`. Setup additionally takes `--polypizza-key`,
`--fab-cookie`, `--purchase-policy`, `--max-price`.

## Semantic search (EmbeddingGemma 2)

[EmbeddingGemma 2](https://huggingface.co/google/embeddinggemma-2) (Apache
2.0, 740M params) maps text and images into one vector space. modon embeds
two vectors per local asset — a text document and the **preview
thumbnail** — and ranks with reciprocal-rank fusion of keyword (FTS5),
text-vector and image-vector hits. The text document is deliberately lean:
the cleaned name (`SF_Veh_BatteringRam_01` → `vehicle Battering Ram`) plus
any agent-written description (folders only when the name says nothing).
On a 24.5k-asset library with synonym queries ("something to sit on",
"a boat pier") that scored precision@20 0.78 vs 0.70 for a document that
also listed category/style/tags/materials/folders — shared boilerplate
drowns out what the object is; style and pack stay reachable through
keyword search and facet filters. Everything is optional: with no
embedding server, search is the plain keyword search.

```bash
modon config set embedding.endpoint http://127.0.0.1:8178
modon config set embedding.autostart true   # or run `modon embed-server` yourself
modon embed --render                        # render missing previews, embed everything
modon search "mossy stone ruin"             # hybrid; results show score + matched_by
modon search --image ref.png                # query by picture
modon search --style-ref local:2373 table   # tables that look like owned asset 2373
modon search --style-ref ref.png --style-weight 0.7 barrel
modon similar fab:<uid>                     # owned look-alikes of a paid listing
modon suggest local:42                      # zero-shot category/style
modon embed --eval                          # zero-shot accuracy vs. importer categories
modon embed --classify [--apply]            # fill empty categories (dry run by default)
```

- **Server.** `modon embed-server` runs the bundled `embed_server.py` with
  `uv run`; uv installs torch + sentence-transformers on first launch and
  the model downloads from Hugging Face (~1.5 GB). It runs on CUDA in
  bfloat16 (never float16, which yields NaNs) and falls back to CPU, where it
  works but is slow for images (seconds each). Any OpenAI-compatible
  `/v1/embeddings` server (e.g. `llama-server --embeddings --pooling mean`
  with `unsloth/embeddinggemma-2-GGUF`) also works, text only.
- **Storage.** Vectors live in `asset_vectors` inside the index DB
  (`256` dims → 1 KB per vector), keyed by model + dims + a content hash, so
  `modon embed` only re-embeds what changed; `serve` refreshes changed assets
  in the background at startup and after `update_asset_metadata`. Search is
  brute force in memory (tens of thousands of assets in milliseconds).
- **Scoring.** A vector hit must stand out from the query's score
  distribution over the whole corpus: z ≥ √(2 ln n) − 0.5 (≈3 for 500
  assets, ≈4 for 25k), which leaves gibberish queries with 0–10 hits on a
  24.5k library with full thumbnail coverage, for text and preview signals
  alike (raw cosines separated better than neutral-query-corrected ones at
  that scale). A signal covering too few assets (e.g. 2 previews in
  a 24k library) is ignored. Facet filters (category/style/tags/animated/
  type) apply to vector hits too. Task prefixes follow the model card (`task: search result |
  query: …` / `title: … | text: …`; images unprefixed).
- **Style references.** `style_reference` / `--style-ref` takes an image
  or an asset id (`local:N` — e.g. something already in the scene — or a
  `fab:`/`polypizza:` id, whose preview is used). The query text picks a
  relevant candidate pool (3× the limit); those are re-ranked by preview
  similarity to the reference, blended with relevance by `style_weight`
  (default 0.5). Among candidates that are all "tables", similarity mostly
  reflects the look: a gothic stone chair pulls ornate turned-leg tables to
  the top, a thin metal office chair pulls minimalist metal-frame ones.
  Without query text it returns the closest-looking owned assets.
- **find_similar / image queries** fuse per-signal z-scores (visual and
  text weighted equally for find_similar) so an exact match is not
  flattened by rank fusion; `similarity` is the raw preview-vs-preview
  cosine. Workbench thumbnails share background/camera/lighting, so
  unrelated previews still sit around 0.8: read `similarity` as a
  tiebreaker, and ≳0.97 as "probably the same model".
- **Zero-shot labels are hints.** Image-only category guesses agreed with
  the importer's filename heuristic 47% top-1 / 68% top-3 on a stratified
  2,095-asset sample, and 40% / 57% over a full 24.5k Synty-heavy library
  (11.3k categorized assets, dominated by character pieces, modular walls
  and building parts where the heuristic is noisy too, e.g. police officers
  filed under *creature*). Agreement rises with the confidence margin: 75%
  at the `--classify` default (0.05, ~18% of assets). `--classify` only
  fills *empty* categories and is a dry run unless `--apply`; treat
  `suggest_metadata` as a hint for agents writing real descriptions.
- **Images need previews.** Run `modon embed --render` (Blender/f3d,
  `preview.workers` in parallel) or `import --previews`; assets without one
  are still found through their text vector. Blender renders in batches of
  25 per session (~0.2 s/asset instead of ~3.6 s with a Blender launch per
  file); a crash or 2-minute stall fails just that asset. Textures whose
  stored paths point at the artist's machine are relinked by name from the
  asset's folder and its parents (e.g. `Texture_01.psd` → shipped
  `Texture_01_A.png`); still-missing ones render in material colours
  instead of Blender's magenta.

## How it works

- `internal/store` — SQLite index (`modernc.org/sqlite`, no cgo) with FTS5
  (+ LIKE fallback), facet filters, preserved agent metadata.
- `internal/parse` — best-effort glTF/GLB, OBJ (+MTL textures), FBX binary +
  ASCII (+embedded thumbnails), STL, PLY, COLLADA; unknown types degrade to
  filename metadata.
- `internal/classify` — heuristic category / art-style / tag inference
  (path + mesh/material names + tri counts); agents refine via
  `update_asset_metadata`.
- `internal/importer` — directory walk, parse → classify → upsert, parallel
  Blender/f3d thumbnails.
- `internal/preview` — headless Workbench renders (`render_preview.py`) or
  f3d, auto-detected.
- `internal/source` — `local` / `polypizza` / `fab` backends + registry with
  owned-first ordering and purchase-policy gating. Fab's undocumented API is
  parsed defensively (search- vs detail-shape payloads, string-or-number
  prices).
- `internal/embed` — EmbeddingGemma 2 client (OpenAI `/v1/embeddings`
  plus image inputs), task prefixes, MRL truncation, and the bundled
  `embed_server.py` sidecar.
- `internal/semantic` — vector backfill, hybrid RRF search, query by image,
  find-similar, zero-shot classification.
- `internal/server` — MCP tools over the official
  `github.com/modelcontextprotocol/go-sdk`.

## Dev

```bash
go build ./... && go vet ./... && go test ./...
```

Fab search is live-network; the unit suite stays hermetic by scoping the
registry test to `local` (plus shape fixtures for Fab payloads).
