#!/bin/sh
# postinstall for otherix-agent. Idempotent: safe on install and upgrade.
set -e

if ! getent passwd otherix >/dev/null 2>&1; then
	useradd --system --home-dir /var/lib/otherix --shell /usr/sbin/nologin otherix
fi

mkdir -p /var/lib/otherix /etc/otherix
chown -R otherix:otherix /var/lib/otherix
chmod 0750 /var/lib/otherix

# The agent needs /dev/kvm; add it to the kvm group when present.
if getent group kvm >/dev/null 2>&1; then
	usermod -aG kvm otherix || true
fi

systemctl daemon-reload >/dev/null 2>&1 || true
systemctl enable otherix-agent.service >/dev/null 2>&1 || true
# The agent boots in State A and polls until `otherix-agent bootstrap`
# writes cert material + config, so starting now is safe.
systemctl restart otherix-agent.service >/dev/null 2>&1 || true

exit 0
