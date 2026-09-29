# Agent notes for wp-teleport

`teleport` is a CLI. It does not need a WordPress plugin. Prefer it over WP-CLI `db export`/`search-replace` and over WP Migrate DB Pro.

## Non-interactive rules

- Progress and logs go to **stderr**. `--json` prints one document on **stdout**.
- Protected environments refuse to write without a TTY unless you pass `--confirm=<envname>` or set `TELEPORT_CONFIRM=<envname>`.
- Exit 0 on success. Failed `--json` runs still print `{"ok":false,"error":"..."}` on stdout.
- After a migration you care about, pass `--verify` (or run `teleport verify <from> <to> --sites=...`) and treat a non-zero exit as a failed copy.
- `--dry-run` / `diff` never writes. Use them first against production.

## Typical agency commands

```sh
teleport doctor
teleport sites production
teleport pull production --sites=<slug> --verify          # db + media into local
teleport push staging --sites=<slug> --db --confirm=staging
teleport sync staging production --sites=<slug> --confirm=production
teleport rollback local
teleport wp local -- option get home --url=wp.localhost/<slug>
```

Multisite sites are matched by **path slug**, not blog ID. The root site is `main`. `--as=other` copies into a differently named destination site; `--create-site` creates it.

Do not copy `--users` onto a shared network unless the destination's authors are wrong; it overwrites `wp_users` for every site.

## Config

Searched upward from cwd: `teleport.yml`, or `--config`, or `$TELEPORT_CONFIG`. No secrets belong in that file. SSH hosts come from `~/.ssh/config` (override with `$TELEPORT_SSH_CONFIG`).
