# wp-teleport

Fast, safe WordPress database and file migrations between local, staging and production. `teleport` is a single binary that runs on your machine. On servers it only needs SSH and WP-CLI, so there is no plugin to install, license to activate or port to open.

```sh
teleport pull production --sites=shop              # defaults to --db --media
teleport push staging --sites=shop --db --themes=my-theme
teleport sync staging production --sites=shop      # server to server
teleport rollback production                       # undo the last swap
teleport wp production plugin list --url=example.com/shop
```

Agents and scripts: pass `--json` (progress stays on stderr), `--confirm=<env>` or `TELEPORT_CONFIRM=<env>` for protected destinations, and `--verify` to fail the run if leftover source URLs or broken serialized data remain.

## Why

WP Migrate DB Pro sends data through PHP over HTTP in small chunks, and a migration writes over live tables as it goes. teleport takes a different approach:

- **Streams, not chunks.** `mysqldump` on the source, compressed with zstd over one multiplexed SSH connection, and piped into `mysql` on the destination. Tables are split across parallel streams, balanced by size.
- **Atomic.** Tables are imported under temporary names and swapped in with a single `RENAME TABLE`. Visitors never see a half-imported site, and an interrupted run leaves the live site untouched.
- **Rollback.** The replaced tables are kept (the last 3 runs by default), so `teleport rollback <env>` gets you back in seconds.
- **Serialization-safe find and replace in the stream.** URLs, paths, table prefixes and multisite IDs are rewritten while the dump flows through. PHP serialized strings get their lengths fixed, and JSON-escaped URLs (for example in block attributes) are handled too.
- **Deltas.** Tables whose checksums have not changed on either side since the last run are skipped. Files move with rsync (parallel shards; tar+zstd for the first copy into an empty folder). Between two servers, files go directly from server to server instead of through your laptop.
- **Multisite that makes sense.** Subsites are matched by path, not by ID, because IDs almost never agree across environments. Copy one site into another (`--as`), create it on the way (`--create-site`), or move the whole network (`--network`).
- **Guardrails.** Protected environments require you to type their name (or pass `--confirm` / `TELEPORT_CONFIRM`). Each environment has a lock so two migrations can't collide. teleport refuses to run when two environments point at the same database, and `blog_public` (search engine visibility) is never overwritten.

Measured on a real multisite (production to local over the internet, one 26-table subsite with 315 MB of media): database in 0.5 s, media in 26 s, 37 s end to end. A repeat run with nothing changed finishes in 2.5 s.

## Install

```sh
brew install cloak-labs/tap/wp-teleport     # installs `teleport` and the short alias `wpt`
# or
go install github.com/cloak-labs/wp-teleport/cmd/teleport@latest
```

Prebuilt binaries for macOS and Linux (amd64/arm64) are attached to each [release](https://github.com/cloak-labs/wp-teleport/releases).

**Your machine** needs `ssh`, `zstd` and rsync 3.2.3 or newer. macOS ships Apple's `openrsync`, so run `brew install rsync zstd`; teleport will use Homebrew's copy.

**Each server** needs WP-CLI, the `mysqldump`/`mysql` (or `mariadb-dump`/`mariadb`) clients, and ideally `zstd` and `rsync`. Run `teleport doctor` to check.

## Configure

Run `teleport init`, or write a `teleport.yml` next to your WordPress install (teleport searches upward from the current directory, or reads `--config` / `$TELEPORT_CONFIG`):

```yaml
environments:
  local:
    docker: wordpress          # run commands in this container; omit for a WordPress on this machine
    path: /var/www/html        # WordPress root as that environment sees it
  staging:
    ssh: staging               # a Host from ~/.ssh/config, or user@host
    path: ~/public_html
  production:
    ssh: production
    path: ~/public_html
    protected: true            # requires typing "production" to write to it

hooks:
  after:
    - wp cache flush           # runs on the destination; "wp " expands to that env's WP-CLI
    # - wp cache flush --url={url}   # once per migrated site

preserve_options: [blog_public]   # destination options that are never overwritten
replace: ["old-cdn.example.com=>cdn.example.com"]
files:
  exclude: [cache/]
backups:
  keep: 3
parallel: 4

profiles:
  refresh-local:
    from: production
    to: local
    sites: [shop]
    db: true
    media: true
    exclude_revisions: true
```

teleport reads database credentials from each WordPress install on the server, and they never leave it. The config holds no secrets and is safe to commit. Environments can also set `wp` (a custom WP-CLI command), `wp_flags`, `host_path` (where a Docker install's files live on your machine, if not auto-detected from its mounts) and their own `hooks`.

SSH connections use your normal `~/.ssh/config`. Set `TELEPORT_SSH_CONFIG=/path/to/ssh_config` to use a different file, which is handy in CI (see `e2e/run.sh`). Protected writes also honour `TELEPORT_CONFIRM=<env>`.

## Commands

| Command | What it does |
| --- | --- |
| `pull <env>` / `push <env>` / `sync <from> <to>` | Copy. With no `--db`/`--media`/… flags, copies database and uploads |
| `diff <from> <to>` | Dry run: which tables changed, row counts, and the bytes rsync would move |
| `run <profile>` | Run a saved profile; any flag overrides the profile |
| `verify <from> <to>` | Check leftover source URLs, serialized PHP, home/siteurl, missing authors |
| `replace <env>` | Serialization-safe find/replace in place (`--replace='old=>new'`) |
| `export <env>` / `import <env> <file>` | Snapshot to `.sql.zst` and restore into any environment |
| `rollback <env> [run]` | Swap the previous tables back in |
| `backups list \| restore \| prune` | Inspect, restore, or drop kept backups |
| `sites <env>` | List a multisite network's sites and their slugs |
| `delete-site <env> <slug>` | Delete a subsite (never the main site) and `uploads/sites/<id>` |
| `wp <env> -- <args>` | WP-CLI in that environment (exit status passed through) |
| `shell <env> [command]` | Login shell, or one command, in the WordPress root |
| `doctor [env...]` | Check connectivity, tools, versions and database access |
| `envs`, `profiles`, `init`, `unlock <env>` | Housekeeping |

Choose what to move:

| Flag | |
| --- | --- |
| `--db` | Database tables of the selected sites |
| `--tables=posts,postmeta,gf_*` / `--exclude-tables=...` | Only, or all but, these tables (globs ok) |
| `--exclude-post-types=revision`, `--exclude-revisions`, `--exclude-spam`, `--exclude-transients` | Filter rows |
| `--users` | Also copy the network-wide users tables (overwrites destination users) |
| `--media`, `--media-since=2025-06` | Uploads, optionally only recent ones |
| `--themes[=a,b]`, `--plugins[=a,b]`, `--mu-plugins`, `--files=languages` | Code and other `wp-content` paths |
| `--delete` / `--exclude='*.mp4'` | Remove extra destination files, or skip patterns |
| `--sites=a,b`, `--as=b`, `--create-site`, `--network` | Multisite selection (`main` is the root site) |
| `--replace='a=>b'`, `--regex='p=>r'`, `--skip-guids` | Rewriting |
| `--preserve-options=x`, `--preserve-active-plugins` | Keep destination options / active plugins and theme |
| `-n/--dry-run`, `--full`, `--no-backup`, `--keep=N`, `--maintenance`, `--verify`, `-j/--parallel` | Run behaviour |

Add `--json` to any command for machine-readable output on stdout (`ok`, plus the result or `error`); progress goes to stderr.

## How it works

1. teleport opens one multiplexed SSH connection per environment (`ControlMaster`) and runs a small embedded PHP script through `wp eval-file -`. The script reports the environment's sites, tables, sizes, URLs, paths and database engine as JSON, and writes a mode-0600 MySQL option file under `~/.teleport/` for the dump and import to use. Nothing else is written outside the database.
2. It builds a plan. Sites are mapped by path; table names, blog IDs, URLs, upload paths and prefixed options such as `wp_2_user_roles` are remapped. Unchanged tables are skipped using `CHECKSUM TABLE`.
3. Each stream runs `mysqldump | zstd` → teleport's rewriter → `zstd -d | mysql`. The rewriter also maps collations between MySQL and MariaDB (for example `utf8mb4_0900_ai_ci` on MariaDB before 11.4).
4. Imports land in `_t<run>_*` tables. After every stream has finished and been checked for truncation, preserved options are copied over and the tables are swapped in one `RENAME TABLE`. The old tables become `_b<run>_*`, and older backups are pruned.
5. Files are copied, then the `after` hooks run.

## Limitations

- Needs shell access. Hosts without SSH or WP-CLI are not supported.
- Rollback covers the database only. Files are never deleted unless you pass `--delete`.
- Replacing a table briefly needs twice its disk space.
- `--users` overwrites the whole network's users table, not one site's authors.

## License

MIT
