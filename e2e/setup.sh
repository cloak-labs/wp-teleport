#!/bin/sh
# tp-setup <single|multisite> <src|dst> <db-host>
# Installs WordPress in /srv/wp and seeds content that exercises the tricky
# parts of a migration: serialized and JSON-escaped URLs, and blog IDs that
# differ between the two sides.
set -eu
mode=$1 side=$2 dbhost=$3
url="http://$side.test"
cd /srv/wp
wp() { command wp --path=/srv/wp "$@"; }

wp config create --dbname=wp --dbuser=wp --dbpass=wp --dbhost="$dbhost" --skip-check --force
if [ "$mode" = multisite ]; then
	wp core multisite-install --url="$url" --title="$side" --admin_user=admin --admin_email=a@example.com --admin_password=x --skip-email
	# Different creation order so "shop" gets a different blog ID on each side.
	if [ "$side" = src ]; then
		wp site create --slug=junk --quiet
		wp site create --slug=shop --title="Shop" --quiet
		wp site create --slug=news --title="News" --quiet
	else
		wp site create --slug=shop --title="Old shop" --quiet
	fi
	seed_url="$url/shop"
else
	wp core install --url="$url" --title="$side" --admin_user=admin --admin_email=a@example.com --admin_password=x --skip-email
	seed_url="$url"
fi

seed() {
	target=$1
	if [ "$side" = src ]; then
		for i in 1 2 3 4 5; do
			id=$(wp --url="$target" post create --post_title="Post $i" --post_status=publish --post_content="<a href=\"$target/p$i\">x</a> <!-- wp:image {\"url\":\"${target}/img$i.jpg\"} -->" --porcelain)
			wp --url="$target" post meta update "$id" tp_ser "{\"u\":\"$target/a\",\"n\":[1,2]}" --format=json
		done
		wp --url="$target" post create --post_type=revision --post_title="rev" --post_status=inherit --quiet
		wp --url="$target" option update tp_json "{\"url\":\"$(printf '%s' "$target/x" | sed 's#/#\\/#g')\"}"
		wp --url="$target" transient set tp_transient 1
		up=$(wp --url="$target" eval 'echo wp_upload_dir()["basedir"];')
		mkdir -p "$up/2024/01" "$up/2025/06"
		head -c 200000 /dev/urandom >"$up/2024/01/a.bin"
		echo hello >"$up/2025/06/b.txt"
	else
		wp --url="$target" post create --post_title="Destination only" --post_status=publish --quiet
		wp --url="$target" option update blog_public 0
	fi
}
seed "$seed_url"
