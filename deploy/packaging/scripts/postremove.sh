#!/bin/sh
# postremove. On purge, remove operator config but KEEP /var/lib/otherix
# (etcd data, VM state) - data is never destroyed by package removal.
set -e

systemctl daemon-reload >/dev/null 2>&1 || true

if [ "$1" = "purge" ]; then
	rm -f /etc/otherix/api.env
	rm -rf /etc/otherix
	echo "otherix: /var/lib/otherix retained (data). Remove it manually if intended." >&2
fi

exit 0
