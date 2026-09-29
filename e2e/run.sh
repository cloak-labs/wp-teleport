#!/usr/bin/env bash
# End-to-end test: two WordPress servers over SSH, real migrations between them.
#
#   MODE=single|multisite SRC_DB_IMAGE=mysql:8.0 DST_DB_IMAGE=mariadb:10.11 e2e/run.sh
#
# Needs docker, ssh, rsync >= 3.2.3, zstd and go.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
work="$here/.work"
MODE=${MODE:-single}
export SRC_DB_IMAGE=${SRC_DB_IMAGE:-mysql:8.0} DST_DB_IMAGE=${DST_DB_IMAGE:-mariadb:10.11}
compose=(docker compose -f "$here/compose.yml")

pass=0
ok() { pass=$((pass + 1)); printf '  \033[32mok\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }
eq() { [ "$2" = "$3" ] && ok "$1" || fail "$1: expected '$3', got '$2'"; }
has() { case "$2" in *"$3"*) ok "$1" ;; *) fail "$1: '$3' not in: $2" ;; esac; }
lacks() { case "$2" in *"$3"*) fail "$1: unexpected '$3' in: $2" ;; *) ok "$1" ;; esac; }
on() { local side=$1; shift; "${compose[@]}" exec -T -u wp "$side" wp --path=/srv/wp "$@"; }

cleanup() {
	[ -n "${SSH_AGENT_PID:-}" ] && kill "$SSH_AGENT_PID" 2>/dev/null || true
	[ -n "${KEEP:-}" ] || "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== e2e: $MODE, $SRC_DB_IMAGE -> $DST_DB_IMAGE"
rm -rf "$work" && mkdir -p "$work/keys" "$work/home"
ssh-keygen -q -t ed25519 -N '' -f "$work/id_ed25519"
cp "$work/id_ed25519.pub" "$work/keys/authorized_keys"
cat >"$work/ssh_config" <<EOF
Host tp-src tp-dst
  HostName 127.0.0.1
  User wp
  IdentityFile $work/id_ed25519
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
Host tp-src
  Port 2201
Host tp-dst
  Port 2202
EOF
export TELEPORT_SSH_CONFIG="$work/ssh_config"
eval "$(ssh-agent -s)" >/dev/null
ssh-add -q "$work/id_ed25519"

(cd "$root" && go build -o "$work/teleport" ./cmd/teleport)
"${compose[@]}" up -d --build --wait --quiet-pull >/dev/null
"${compose[@]}" exec -T -u wp src tp-setup "$MODE" src src-db >/dev/null
"${compose[@]}" exec -T -u wp dst tp-setup "$MODE" dst dst-db >/dev/null

cat >"$work/teleport.yml" <<EOF
environments:
  src: { ssh: tp-src, path: /srv/wp }
  dst: { ssh: tp-dst, path: /srv/wp, protected: true }
hooks:
  after: [wp cache flush]
EOF
export TELEPORT_CONFIG="$work/teleport.yml"
tp() { (cd "$work/home" && "$work/teleport" "$@"); }

tp doctor >/dev/null && ok "doctor"

if [ "$MODE" = single ]; then
	sel=--db dsturl=http://dst.test srcsite=--url=src.test dstsite=--url=dst.test
else
	sel=--sites=shop dsturl=http://dst.test/shop srcsite=--url=src.test/shop dstsite=--url=dst.test/shop
fi
src_on() { on src "$srcsite" "$@"; }
dst_on() { on dst "$dstsite" "$@"; }

if tp sync src dst $sel --db </dev/null 2>/dev/null; then fail "protected env must require confirmation"; fi
ok "protected destination refuses without --confirm"

out=$(tp sync src dst $sel --db --media --exclude-post-types=revision --exclude-transients --confirm=dst --json)
first_run=$(echo "$out" | sed -n 's/^  "run": "\(.*\)",$/\1/p')
echo "$out" | grep -Eq '"changed_values": [1-9]' && ok "values rewritten in the stream" || fail "no values rewritten: $out"
eq "home rewritten" "$(dst_on option get home)" "$dsturl"
eq "blog_public preserved" "$(dst_on option get blog_public)" "0"
eq "posts copied" "$(dst_on post list --post_type=post --format=count)" "6"
eq "revisions filtered" "$(dst_on post list --post_type=revision --post_status=any --format=count)" "0"
prefix=$(dst_on db prefix)
eq "transients filtered" "$(dst_on db query "SELECT COUNT(*) FROM ${prefix}options WHERE option_name LIKE '%tp_transient%'" --skip-column-names)" "0"
eq "no source URLs left" "$(dst_on db query "SELECT COUNT(*) FROM ${prefix}posts WHERE post_content LIKE '%src.test%'" --skip-column-names)" "0"
pid=$(dst_on post list --post_type=post --field=ID --posts_per_page=1)
has "serialized meta rewritten" "$(dst_on post meta get "$pid" tp_ser --format=json)" "dst.test"
has "JSON-escaped URL rewritten" "$(dst_on option get tp_json)" 'dst.test\/'
has "block attribute URL rewritten" "$(dst_on post get "$pid" --field=post_content)" "\"url\":\"$dsturl/img"
if [ "$MODE" = multisite ]; then
	eq "user_roles remapped" "$(dst_on eval 'echo count(get_option($GLOBALS["wpdb"]->prefix."user_roles") ?: []) > 0 ? "yes" : "no";')" "yes"
fi
srcup=$(src_on eval 'echo wp_upload_dir()["basedir"];')
dstup=$(dst_on eval 'echo wp_upload_dir()["basedir"];')
eq "media copied" "$("${compose[@]}" exec -T dst md5sum "$dstup/2024/01/a.bin" | cut -d' ' -f1)" "$("${compose[@]}" exec -T src md5sum "$srcup/2024/01/a.bin" | cut -d' ' -f1)"

out=$(tp sync src dst $sel --db --exclude-post-types=revision --exclude-transients --confirm=dst --json)
lacks "repeat run skips unchanged tables" "$out" '"unchanged_tables": 0'

tp rollback dst "$first_run" --confirm=dst >/dev/null 2>&1
has "rollback of an older run (and everything after it)" "$(dst_on post list --post_type=post --field=post_title)" "Destination only"

src_on post create --post_title="After rollback" --post_status=publish --quiet
tp sync src dst $sel --tables=posts,postmeta --confirm=dst >/dev/null 2>&1
has "--tables copies a subset" "$(dst_on post list --post_type=post --field=post_title)" "After rollback"

if [ "$MODE" = multisite ]; then
	tp sync src dst --sites=news --create-site --db --confirm=dst >/dev/null 2>&1
	eq "--create-site creates and fills the site" "$(on dst --url=dst.test/news option get blogname)" "News"

	tp sync src dst --network --db --confirm=dst >/dev/null 2>&1
	urls=$(on dst site list --field=url | sort | tr '\n' ' ')
	eq "--network copies every site with remapped domains" "$urls" "http://dst.test/ http://dst.test/junk/ http://dst.test/news/ http://dst.test/shop/ "
fi

echo "== $pass checks passed ($MODE, $SRC_DB_IMAGE -> $DST_DB_IMAGE)"
