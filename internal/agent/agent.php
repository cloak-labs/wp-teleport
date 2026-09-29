<?php
/**
 * wp-teleport agent. Streamed to WP-CLI with `wp eval-file - <action> <base64-json>`
 * so nothing has to be installed on the server. Output is one JSON document
 * between markers; anything else a plugin prints is ignored by the client.
 */

namespace WPTeleport\Agent;

const META = '_teleport_runs';
const LOCK_TTL = 3600;

function out($data)
{
    if ($data === []) {
        $data = new \stdClass();
    }
    echo "\n<<<TELEPORT\n" . json_encode($data, JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE) . "\nTELEPORT>>>\n";
}

function fail($message)
{
    out(['error' => $message]);
    exit(0);
}

function q($name)
{
    return '`' . str_replace('`', '``', $name) . '`';
}

function db()
{
    global $wpdb;
    return $wpdb;
}

function query($sql)
{
    $result = db()->query($sql);
    if ($result === false) {
        throw new \RuntimeException(db()->last_error . ' [' . substr($sql, 0, 300) . ']');
    }
    return $result;
}

function table_names()
{
    return db()->get_col('SHOW TABLES');
}

function ensure_meta()
{
    query('CREATE TABLE IF NOT EXISTS ' . META . ' (id VARCHAR(32) NOT NULL PRIMARY KEY, created INT UNSIGNED NOT NULL, data LONGTEXT NOT NULL) DEFAULT CHARSET=utf8mb4');
}

function meta_rows()
{
    if (!in_array(META, table_names(), true)) {
        return [];
    }
    $rows = [];
    foreach (db()->get_results('SELECT id, created, data FROM ' . META . ' ORDER BY created DESC', ARRAY_A) as $row) {
        if ($row['id'] === 'lock') {
            continue;
        }
        $data = json_decode($row['data'], true) ?: [];
        $data['id'] = $row['id'];
        $data['created'] = (int) $row['created'];
        $rows[] = $data;
    }
    // Several runs can start within the same second.
    usort($rows, fn ($a, $b) => ($b['created_us'] ?? $b['created'] * 1000000) <=> ($a['created_us'] ?? $a['created'] * 1000000));
    return $rows;
}

function save_run($run)
{
    ensure_meta();
    $data = $run;
    unset($data['id'], $data['created']);
    db()->replace(META, ['id' => $run['id'], 'created' => $run['created'], 'data' => wp_json_encode($data)]);
}

function home_dir()
{
    $home = getenv('HOME');
    if (!$home && function_exists('posix_getpwuid')) {
        $home = posix_getpwuid(posix_geteuid())['dir'] ?? '';
    }
    return $home ?: sys_get_temp_dir();
}

function cnf_path($run)
{
    return home_dir() . '/.teleport/' . preg_replace('/[^a-z0-9]/i', '', $run) . '.cnf';
}

/** Write a 0600 MySQL option file so credentials never leave the server. */
function write_cnf($run)
{
    $dir = home_dir() . '/.teleport';
    if (!is_dir($dir) && !mkdir($dir, 0700, true)) {
        throw new \RuntimeException("Cannot create $dir");
    }
    $parsed = db()->parse_db_host(DB_HOST);
    [$host, $port, $socket] = $parsed ?: [DB_HOST, null, null];
    $esc = function ($v) {
        return '"' . str_replace(['\\', '"'], ['\\\\', '\\"'], (string) $v) . '"';
    };
    $lines = ['[client]', 'user=' . $esc(DB_USER), 'password=' . $esc(DB_PASSWORD)];
    if ($host) {
        $lines[] = 'host=' . $esc($host);
    }
    if ($port) {
        $lines[] = 'port=' . (int) $port;
    }
    if ($socket) {
        $lines[] = 'socket=' . $esc($socket);
    }
    $path = cnf_path($run);
    $old = umask(0077);
    file_put_contents($path, implode("\n", $lines) . "\n");
    umask($old);
    chmod($path, 0600);
    return $path;
}

function checksums($tables)
{
    $existing = array_flip(table_names());
    $sums = [];
    foreach (array_chunk(array_values(array_filter($tables, function ($t) use ($existing) {
        return isset($existing[$t]);
    })), 50) as $chunk) {
        $rows = db()->get_results('CHECKSUM TABLE ' . implode(', ', array_map(__NAMESPACE__ . '\q', $chunk)), ARRAY_A);
        foreach ($rows as $row) {
            $name = $row['Table'];
            $dot = strpos($name, '.');
            if ($dot !== false) {
                $name = substr($name, $dot + 1);
            }
            $sums[$name] = (string) $row['Checksum'];
        }
    }
    return $sums;
}

function site_info($blog_id)
{
    $switched = false;
    if (is_multisite() && get_current_blog_id() !== (int) $blog_id) {
        switch_to_blog($blog_id);
        $switched = true;
    }
    $uploads = wp_upload_dir(null, false);
    $info = [
        'id' => (int) $blog_id,
        'home' => untrailingslashit(get_option('home')),
        'siteurl' => untrailingslashit(get_option('siteurl')),
        'uploads_dir' => untrailingslashit($uploads['basedir']),
        'uploads_url' => untrailingslashit($uploads['baseurl']),
        'prefix' => db()->get_blog_prefix($blog_id),
        'title' => get_option('blogname'),
    ];
    if ($switched) {
        restore_current_blog();
    }
    return $info;
}

function manifest()
{
    global $wp_version;
    $wpdb = db();
    $version = $wpdb->get_var('SELECT VERSION()');
    $tables = [];
    foreach ($wpdb->get_results('SHOW TABLE STATUS', ARRAY_A) as $t) {
        $tables[] = [
            'name' => $t['Name'],
            'rows' => (int) $t['Rows'],
            'bytes' => (int) $t['Data_length'] + (int) $t['Index_length'],
            'engine' => $t['Engine'],
            'collation' => $t['Collation'],
            'view' => $t['Comment'] === 'VIEW',
        ];
    }
    $sites = [];
    $network = null;
    if (is_multisite()) {
        foreach (get_sites(['number' => 0, 'fields' => 'ids']) as $id) {
            $s = get_site($id);
            $info = site_info($id);
            $info['domain'] = $s->domain;
            $info['path'] = $s->path;
            $sites[] = $info;
        }
        $net = get_network();
        $network = [
            'domain' => $net->domain,
            'path' => $net->path,
            'subdomain_install' => is_subdomain_install(),
            'main_site' => (int) get_main_site_id(),
        ];
    } else {
        $info = site_info(1);
        $parts = wp_parse_url($info['home']);
        $info['domain'] = $parts['host'] ?? '';
        $info['path'] = trailingslashit($parts['path'] ?? '/');
        $sites[] = $info;
    }
    $lock = null;
    if (in_array(META, table_names(), true)) {
        $row = $wpdb->get_row('SELECT created, data FROM ' . META . " WHERE id = 'lock'", ARRAY_A);
        if ($row) {
            $lock = json_decode($row['data'], true) ?: [];
            $lock['created'] = (int) $row['created'];
        }
    }
    return [
        'wp_version' => $wp_version,
        'wp_cli_version' => defined('WP_CLI_VERSION') ? WP_CLI_VERSION : '',
        'php_version' => PHP_VERSION,
        'multisite' => is_multisite(),
        'network' => $network,
        'base_prefix' => $wpdb->base_prefix,
        'global_tables' => array_values($wpdb->tables('global', true)),
        'abspath' => untrailingslashit(ABSPATH),
        'content_dir' => untrailingslashit(WP_CONTENT_DIR),
        'plugin_dir' => untrailingslashit(WP_PLUGIN_DIR),
        'mu_plugin_dir' => untrailingslashit(WPMU_PLUGIN_DIR),
        'theme_root' => untrailingslashit(get_theme_root()),
        'sites' => $sites,
        'tables' => $tables,
        'db' => [
            'name' => DB_NAME,
            'host' => DB_HOST,
            'server' => (string) $wpdb->get_var('SELECT @@hostname'),
            'version' => $version,
            'engine' => stripos($version, 'mariadb') !== false ? 'mariadb' : 'mysql',
            'charset' => $wpdb->charset,
            'collation' => $wpdb->collate,
        ],
        'lock' => $lock,
        'runs' => array_slice(meta_rows(), 0, 20),
    ];
}

function acquire_lock($lock)
{
    ensure_meta();
    $row = db()->get_row('SELECT created, data FROM ' . META . " WHERE id = 'lock'", ARRAY_A);
    if ($row && (time() - (int) $row['created']) < LOCK_TTL && empty($lock['force'])) {
        $held = json_decode($row['data'], true) ?: [];
        throw new \RuntimeException(sprintf(
            'Another migration holds the lock (%s, started %ds ago). Wait, or re-run with --force-unlock.',
            $held['by'] ?? 'unknown',
            time() - (int) $row['created']
        ));
    }
    db()->replace(META, ['id' => 'lock', 'created' => time(), 'data' => wp_json_encode($lock)]);
}

function release_lock()
{
    if (in_array(META, table_names(), true)) {
        query('DELETE FROM ' . META . " WHERE id = 'lock'");
    }
}

function short_name($prefix, $table)
{
    $name = $prefix . $table;
    return strlen($name) <= 64 ? $name : $prefix . substr(md5($table), 0, 20);
}

/** Copy preserved option rows from the live options tables into the imported ones. */
function preserve($pairs, $names)
{
    if (!$pairs || !$names) {
        return;
    }
    $in = implode(',', array_map(function ($n) {
        return "'" . esc_sql($n) . "'";
    }, $names));
    $existing = array_flip(table_names());
    foreach ($pairs as $p) {
        if (!isset($existing[$p['tmp']], $existing[$p['live']])) {
            continue;
        }
        $tmp = q($p['tmp']);
        $live = q($p['live']);
        query("DELETE t FROM $tmp t WHERE t.option_name IN ($in)");
        query("INSERT INTO $tmp (option_name, option_value, autoload) SELECT option_name, option_value, autoload FROM $live WHERE option_name IN ($in)");
    }
}

/** Atomically swap imported tables in, keeping the previous ones for rollback. */
function swap($run, $pairs, $keep_backup, $description)
{
    $existing = array_flip(table_names());
    $renames = [];
    $record = [
        'id' => $run,
        'created' => time(),
        'created_us' => (int) (microtime(true) * 1000000),
        'description' => $description,
        'swapped' => [],
        'created_tables' => [],
        'backup' => (bool) $keep_backup,
        'rolled_back' => false,
    ];
    foreach ($pairs as $p) {
        if (!isset($existing[$p['tmp']])) {
            throw new \RuntimeException("Imported table {$p['tmp']} is missing; nothing was swapped.");
        }
        if (isset($existing[$p['live']])) {
            $bak = short_name('_b' . $run . '_', $p['live']);
            $renames[] = q($p['live']) . ' TO ' . q($bak);
            $record['swapped'][] = ['live' => $p['live'], 'backup' => $bak];
        } else {
            $record['created_tables'][] = $p['live'];
        }
        $renames[] = q($p['tmp']) . ' TO ' . q($p['live']);
    }
    if ($renames) {
        query('RENAME TABLE ' . implode(', ', $renames));
    }
    if (!$keep_backup) {
        foreach ($record['swapped'] as &$s) {
            query('DROP TABLE IF EXISTS ' . q($s['backup']));
            $s['backup'] = null;
        }
        unset($s);
    }
    save_run($record);
    return $record;
}

/**
 * Roll back one migration. Newer migrations that are still applied are rolled
 * back first, newest to oldest, so the result is the state before $run_id.
 */
function rollback($run_id)
{
    $chain = [];
    $found = false;
    foreach (meta_rows() as $r) {
        if (!empty($r['rolled_back'])) {
            if ($run_id && $r['id'] === $run_id) {
                throw new \RuntimeException("Migration {$r['id']} was already rolled back.");
            }
            continue;
        }
        $chain[] = $r;
        if (!$run_id || $r['id'] === $run_id) {
            $found = true;
            break;
        }
    }
    if (!$found) {
        throw new \RuntimeException($run_id ? "No migration $run_id on this environment." : 'No migration to roll back.');
    }
    $existing = array_flip(table_names());
    foreach ($chain as $r) {
        if (empty($r['backup'])) {
            throw new \RuntimeException("Migration {$r['id']} ran with backups disabled; it cannot be rolled back.");
        }
        foreach ($r['swapped'] as $s) {
            if (!isset($existing[$s['backup']])) {
                throw new \RuntimeException("Backup table {$s['backup']} is gone (pruned?); cannot roll back {$r['id']}.");
            }
        }
    }
    $run = null;
    foreach ($chain as $r) {
        $run = rollback_one($r);
    }
    $run['also_rolled_back'] = array_map(fn ($r) => $r['id'], array_slice($chain, 0, -1));
    return $run;
}

function rollback_one($run)
{
    $existing = array_flip(table_names());
    $renames = [];
    $trash = [];
    $x = '_x' . substr(md5(uniqid('', true)), 0, 6) . '_';
    foreach ($run['swapped'] as $s) {
        if (!isset($existing[$s['backup']])) {
            throw new \RuntimeException("Backup table {$s['backup']} is gone (pruned?); cannot roll back {$run['id']}.");
        }
        if (isset($existing[$s['live']])) {
            $t = short_name($x, $s['live']);
            $renames[] = q($s['live']) . ' TO ' . q($t);
            $trash[] = $t;
        }
        $renames[] = q($s['backup']) . ' TO ' . q($s['live']);
    }
    foreach ($run['created_tables'] as $live) {
        if (isset($existing[$live])) {
            $t = short_name($x, $live);
            $renames[] = q($live) . ' TO ' . q($t);
            $trash[] = $t;
        }
    }
    if ($renames) {
        query('RENAME TABLE ' . implode(', ', $renames));
    }
    foreach ($trash as $t) {
        query('DROP TABLE IF EXISTS ' . q($t));
    }
    $run['rolled_back'] = true;
    $run['rolled_back_at'] = time();
    save_run($run);
    return $run;
}

function prune($keep)
{
    $dropped = 0;
    $existing = array_flip(table_names());
    foreach (array_slice(meta_rows(), max(0, (int) $keep)) as $run) {
        foreach ($run['swapped'] ?? [] as $s) {
            if ($s['backup'] && isset($existing[$s['backup']])) {
                query('DROP TABLE IF EXISTS ' . q($s['backup']));
                $dropped++;
            }
        }
        query('DELETE FROM ' . META . " WHERE id = '" . esc_sql($run['id']) . "'");
    }
    return $dropped;
}

/** Remove option files older than the lock TTL (left by killed runs). */
function clean_cnf()
{
    $removed = 0;
    foreach (glob(home_dir() . '/.teleport/*.cnf') ?: [] as $f) {
        if (time() - (int) @filemtime($f) > LOCK_TTL && @unlink($f)) {
            $removed++;
        }
    }
    return $removed;
}

/**
 * Prune backups beyond $keep and, when no migration is running, drop temporary
 * and trash tables that interrupted runs left behind.
 */
function clean($keep, $dry)
{
    $row = in_array(META, table_names(), true)
        ? db()->get_row('SELECT created FROM ' . META . " WHERE id = 'lock'", ARRAY_A)
        : null;
    $locked = $row && (time() - (int) $row['created']) < LOCK_TTL;
    $result = ['backups' => [], 'orphans' => [], 'cnf' => 0, 'locked' => $locked];
    $existing = array_flip(table_names());
    foreach (array_slice(meta_rows(), max(0, (int) $keep)) as $run) {
        foreach ($run['swapped'] ?? [] as $s) {
            if ($s['backup'] && isset($existing[$s['backup']])) {
                $result['backups'][] = $s['backup'];
            }
        }
    }
    if (!$locked) {
        foreach (array_keys($existing) as $t) {
            if (preg_match('/^_[tx][a-z0-9]{6}_/', $t)) {
                $result['orphans'][] = $t;
            }
        }
    }
    if ($dry) {
        return $result;
    }
    prune($keep);
    foreach ($result['orphans'] as $t) {
        query('DROP TABLE IF EXISTS ' . q($t));
    }
    $result['cnf'] = clean_cnf();
    return $result;
}

function drop_temp($names)
{
    $dropped = [];
    foreach ($names as $n) {
        if (!preg_match('/^_t[a-z0-9]{6}_/', $n)) {
            throw new \RuntimeException("Refusing to drop non-temporary table $n");
        }
        query('DROP TABLE IF EXISTS ' . q($n));
        $dropped[] = $n;
    }
    return $dropped;
}

function create_site($slug, $title)
{
    if (!is_multisite()) {
        throw new \RuntimeException('Cannot create a site: destination is not a multisite network.');
    }
    $net = get_network();
    if (is_subdomain_install()) {
        $domain = $slug . '.' . preg_replace('/^www\./', '', $net->domain);
        $path = $net->path;
    } else {
        $domain = $net->domain;
        $path = $net->path . $slug . '/';
    }
    $admins = get_super_admins();
    $user = $admins ? get_user_by('login', $admins[0]) : null;
    $id = wpmu_create_blog($domain, $path, $title ?: $slug, $user ? $user->ID : 1, [], $net->id);
    if (is_wp_error($id)) {
        throw new \RuntimeException($id->get_error_message());
    }
    return (int) $id;
}

function find_site($slug)
{
    if (!is_multisite()) {
        throw new \RuntimeException('Not a multisite network.');
    }
    foreach (get_sites(['number' => 0]) as $site) {
        $path = trim($site->path, '/');
        $key = $path === '' ? 'main' : basename($path);
        if ($key === $slug || (string) $site->blog_id === (string) $slug || trim($site->path, '/') === $slug) {
            return $site;
        }
    }
    throw new \RuntimeException("Site $slug not found.");
}

function delete_site($slug)
{
    $site = find_site($slug);
    $id = (int) $site->blog_id;
    $main = (int) get_network()->site_id;
    if ($id === $main || $id === 1) {
        throw new \RuntimeException("Refusing to delete the main site (#$id).");
    }
    $prefix = db()->get_blog_prefix($id);
    switch_to_blog($id);
    $uploads = wp_get_upload_dir()['basedir'] ?? '';
    restore_current_blog();
    $dropped = [];
    foreach (table_names() as $t) {
        if (preg_match('/^_[btx][a-z0-9]{6}_' . preg_quote($prefix, '/') . '/', $t)) {
            query('DROP TABLE IF EXISTS ' . q($t));
            $dropped[] = $t;
        }
    }
    $result = wp_delete_site($id);
    if (is_wp_error($result)) {
        throw new \RuntimeException($result->get_error_message());
    }
    return ['id' => $id, 'slug' => $slug, 'prefix' => $prefix, 'uploads_dir' => $uploads, 'dropped_backups' => $dropped];
}

function verify($input)
{
    $needles = $input['needles'] ?? [];
    $sites = $input['sites'] ?? [];
    $out = ['ok' => true, 'sites' => []];
    foreach ($sites as $s) {
        $row = verify_site($s, $needles);
        if (empty($row['ok'])) {
            $out['ok'] = false;
        }
        $out['sites'][] = $row;
    }
    return $out;
}

function verify_site($s, $needles)
{
    $prefix = $s['prefix'];
    $home = $s['home'] ?? '';
    $siteurl = $s['siteurl'] ?? '';
    $row = [
        'slug' => $s['slug'] ?? '',
        'prefix' => $prefix,
        'ok' => true,
        'home' => '',
        'siteurl' => '',
        'leftover' => 0,
        'broken_serialized' => 0,
        'serialized' => 0,
        'missing_authors' => 0,
        'problems' => [],
    ];
    $opt = $prefix . 'options';
    $posts = $prefix . 'posts';
    if (!in_array($opt, table_names(), true)) {
        $row['ok'] = false;
        $row['problems'][] = "table $opt is missing";
        return $row;
    }
    $row['home'] = (string) db()->get_var("SELECT option_value FROM " . q($opt) . " WHERE option_name = 'home' LIMIT 1");
    $row['siteurl'] = (string) db()->get_var("SELECT option_value FROM " . q($opt) . " WHERE option_name = 'siteurl' LIMIT 1");
    if ($home !== '' && rtrim($row['home'], '/') !== rtrim($home, '/')) {
        $row['ok'] = false;
        $row['problems'][] = "home is {$row['home']}, expected $home";
    }
    if ($siteurl !== '' && rtrim($row['siteurl'], '/') !== rtrim($siteurl, '/')) {
        $row['ok'] = false;
        $row['problems'][] = "siteurl is {$row['siteurl']}, expected $siteurl";
    }
    foreach ($needles as $n) {
        if ($n === '' || strlen($n) < 8) {
            continue;
        }
        $like = '%' . db()->esc_like($n) . '%';
        foreach (verify_scan_tables($prefix) as $spec) {
            if (!in_array($spec[0], table_names(), true)) {
                continue;
            }
            $count = (int) db()->get_var(db()->prepare('SELECT COUNT(*) FROM ' . q($spec[0]) . ' WHERE ' . $spec[1] . ' LIKE %s', $like));
            $row['leftover'] += $count;
        }
    }
    if ($row['leftover'] > 0) {
        $row['ok'] = false;
        $row['problems'][] = $row['leftover'] . ' values still contain a source URL or path';
    }
    foreach (verify_scan_tables($prefix) as $spec) {
        if (!in_array($spec[0], table_names(), true)) {
            continue;
        }
        $offset = 0;
        while ($offset < 20000) {
            $values = db()->get_col('SELECT ' . $spec[1] . ' FROM ' . q($spec[0]) . ' WHERE ' . $spec[1] . " LIKE 'a:%' OR " . $spec[1] . " LIKE 'O:%' OR " . $spec[1] . " LIKE 's:%' LIMIT 500 OFFSET $offset");
            if (!$values) {
                break;
            }
            foreach ($values as $v) {
                if (!is_serialized($v)) {
                    continue;
                }
                $row['serialized']++;
                if (@unserialize($v) === false && $v !== 'b:0;') {
                    $row['broken_serialized']++;
                }
            }
            $offset += 500;
            if (count($values) < 500) {
                break;
            }
        }
    }
    if ($row['broken_serialized'] > 0) {
        $row['ok'] = false;
        $row['problems'][] = $row['broken_serialized'] . ' serialized values do not unserialize';
    }
    $users = db()->base_prefix . 'users';
    if (in_array($posts, table_names(), true) && in_array($users, table_names(), true)) {
        $row['missing_authors'] = (int) db()->get_var('SELECT COUNT(*) FROM ' . q($posts) . ' p LEFT JOIN ' . q($users) . ' u ON u.ID = p.post_author WHERE p.post_status = \'publish\' AND u.ID IS NULL');
        if ($row['missing_authors'] > 0) {
            $row['ok'] = false;
            $row['problems'][] = $row['missing_authors'] . ' published posts point at users that do not exist (pass --users, or create the authors first)';
        }
    }
    return $row;
}

function verify_scan_tables($prefix)
{
    return [
        [$prefix . 'posts', 'post_content'],
        [$prefix . 'posts', 'post_excerpt'],
        [$prefix . 'postmeta', 'meta_value'],
        [$prefix . 'options', 'option_value'],
        [$prefix . 'comments', 'comment_content'],
    ];
}

$action = $args[0] ?? 'manifest';
$input = isset($args[1]) ? json_decode(base64_decode($args[1]), true) : [];
if (!is_array($input)) {
    $input = [];
}

try {
    switch ($action) {
        case 'manifest':
            out(manifest());
            break;
        case 'begin':
            $result = [];
            clean_cnf();
            if (!empty($input['lock'])) {
                acquire_lock($input['lock']);
            }
            if (!empty($input['cnf'])) {
                $result['cnf'] = write_cnf($input['cnf']);
            }
            if (!empty($input['checksum'])) {
                $result['checksums'] = checksums($input['checksum']);
            }
            out($result);
            break;
        case 'checksum':
            out(['checksums' => checksums($input['tables'] ?? [])]);
            break;
        case 'finalize':
            $result = [];
            preserve($input['preserve_pairs'] ?? [], $input['preserve'] ?? []);
            if (!empty($input['swap'])) {
                $result['run'] = swap($input['run'], $input['swap'], !empty($input['keep_backup']), $input['description'] ?? '');
            }
            if (isset($input['prune'])) {
                $result['pruned'] = prune($input['prune']);
            }
            if (!empty($input['checksum'])) {
                $result['checksums'] = checksums($input['checksum']);
            }
            if (!empty($input['cleanup'])) {
                @unlink(cnf_path($input['cleanup']));
            }
            if (!empty($input['unlock'])) {
                release_lock();
            }
            out($result);
            break;
        case 'abort':
            $result = ['dropped' => drop_temp($input['drop'] ?? [])];
            if (!empty($input['cleanup'])) {
                @unlink(cnf_path($input['cleanup']));
            }
            if (!empty($input['unlock'])) {
                release_lock();
            }
            out($result);
            break;
        case 'unlock':
            release_lock();
            out(['ok' => true]);
            break;
        case 'rollback':
            out(['run' => rollback($input['run'] ?? '')]);
            break;
        case 'runs':
            out(['runs' => meta_rows()]);
            break;
        case 'clean':
            out(clean($input['keep'] ?? 0, !empty($input['dry'])));
            break;
        case 'create-site':
            out(['id' => create_site($input['slug'], $input['title'] ?? '')]);
            break;
        case 'delete-site':
            out(delete_site($input['slug'] ?? ''));
            break;
        case 'verify':
            out(verify($input));
            break;
        default:
            fail("Unknown agent action $action");
    }
} catch (\Throwable $e) {
    fail($e->getMessage());
}
