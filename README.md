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
| `search_assets` | Free-text + tags/category/style/type/animated/free/max-price/source/limit search. Owned (`local`) results come first; paid hits carry `purchase_required: true`. |
| `get_asset_details` | Full details for `local:<rowid>`, `polypizza:<id>` or `fab:<uid>`. |
| `get_preview` | Preview image inline (plus cached path). Local assets render on demand via Blender/f3d. |
| `download_asset` | `copy` (default) / `symlink` / `reference` for local; downloads remote free files. Paid/unclaimed Fab assets return `purchase_required` / `claim_required` with price + listing URL instead of files. |
| `import_assets` | The setup command as a tool: import + classify a directory, optional thumbnails. |
| `update_asset_metadata` | Enrich a local asset (description, category, style, type, tags). Clears `needs_review`. |
| `list_unclassified` | Local assets still needing rich descriptions, largest first. |
| `index_stats` | Index totals/breakdowns + which sources/credentials are configured. |

Typical agent loop: `search_assets` → `get_preview` → ask user if
`purchase_required` → `download_asset` → `update_asset_metadata` /
`list_unclassified` for enrichment.

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
modon config [get|set|path]    inspect/change config (secrets masked)
modon stats                    index stats + config status
```

Search flags: `--category`, `--style`, `--type`, `--animated` / `--static`,
`--free`, `--max-price`, `--sources local,polypizza,fab`, `--limit`, `--json`
(flags before the query). Import flags: `--no-recursive`, `--previews`,
`--max-previews N`. Setup additionally takes `--polypizza-key`,
`--fab-cookie`, `--purchase-policy`, `--max-price`.

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
- `internal/server` — MCP tools over the official
  `github.com/modelcontextprotocol/go-sdk`.

## Dev

```bash
go build ./... && go vet ./... && go test ./...
```

Fab search is live-network; the unit suite stays hermetic by scoping the
registry test to `local` (plus shape fixtures for Fab payloads).
