# A WordPress "server" reachable over SSH, with only what teleport expects on a
# typical host: WP-CLI, database clients, rsync and zstd.
FROM php:8.3-cli-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server mariadb-client rsync zstd less ca-certificates curl \
 && docker-php-ext-install mysqli \
 && echo 'memory_limit=512M' > /usr/local/etc/php/conf.d/memory.ini \
 && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL -o /usr/local/bin/wp https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar \
 && chmod +x /usr/local/bin/wp

RUN useradd -m -s /bin/bash wp \
 && mkdir -p /srv/wp /run/sshd /home/wp/.ssh \
 && chown -R wp:wp /srv/wp /home/wp/.ssh \
 && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config \
 && echo 'AllowAgentForwarding yes' >> /etc/ssh/sshd_config

USER wp
RUN wp core download --path=/srv/wp --version=6.8
USER root

COPY setup.sh /usr/local/bin/tp-setup
RUN chmod +x /usr/local/bin/tp-setup

CMD ["/bin/sh", "-c", "install -o wp -g wp -m 600 /keys/authorized_keys /home/wp/.ssh/authorized_keys && ssh-keygen -A && exec /usr/sbin/sshd -D -e"]
