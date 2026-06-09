#!/bin/sh
# postinstall for otherix-api. Idempotent: safe on install and upgrade.
set -e

# System user owning /var/lib/otherix and running the daemon.
if ! getent passwd otherix >/dev/null 2>&1; then
	useradd --system --home-dir /var/lib/otherix --shell /usr/sbin/nologin otherix
fi

mkdir -p /var/lib/otherix /etc/otherix
chown -R otherix:otherix /var/lib/otherix
chmod 0750 /var/lib/otherix

# Generate a jwt secret once, on first install, so a bare `apt install`
# boots a working single node. Never regenerate (would invalidate live
# tokens). Admin credentials are NOT generated here - the install script
# adds them, or the operator sets OTHERIX_BOOTSTRAP_ADMIN_* manually.
if [ ! -f /etc/otherix/api.env ]; then
	secret="$(head -c 48 /dev/urandom | base64 | tr -d '\n')"
	umask 077
	printf 'OTHERIX_AUTH__JWT_SECRET=%s\n' "$secret" > /etc/otherix/api.env
	chown otherix:otherix /etc/otherix/api.env
	chmod 0600 /etc/otherix/api.env
fi

systemctl daemon-reload >/dev/null 2>&1 || true
systemctl enable otherix-api.service >/dev/null 2>&1 || true
systemctl restart otherix-api.service >/dev/null 2>&1 || true

exit 0
