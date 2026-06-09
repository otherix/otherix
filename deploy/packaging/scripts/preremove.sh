#!/bin/sh
# preremove: stop the service before files are removed. Only one of the
# two units is installed on a given host; stop whichever is enabled.
set -e

for unit in otherix-api otherix-agent; do
	if systemctl is-enabled "$unit.service" >/dev/null 2>&1; then
		systemctl disable --now "$unit.service" >/dev/null 2>&1 || true
	fi
done

exit 0
