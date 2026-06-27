#!/bin/sh
# Otherix uninstaller. Removes every Otherix component and ALL its state on this
# host: the otherix-api / otherix-agent packages (apt purge), the otherix CLI,
# /var/lib/otherix, /etc/otherix, the CLI config (~/.otherix), the otherix system
# user, and any otherix-managed bridges / nft rules. Destructive and final.
#
# Non-interactive (e.g. piped):
#   curl -fsSL https://get.otherix.dev/uninstall.sh | sudo FORCE=true sh
# Interactive (prompts for confirmation):
#   curl -fsSL https://get.otherix.dev/uninstall.sh -o uninstall.sh && sudo sh uninstall.sh
#
# Env:
#   FORCE   1|true|yes  skip the confirmation prompt (required for piped use)
set -eu

step() { echo; echo "==> $1"; }
have() { command -v "$1" >/dev/null 2>&1; }

[ "$(id -u)" = "0" ] || { echo "uninstall: run as root (sudo)" >&2; exit 1; }

echo "This permanently removes Otherix and ALL its state on this host:"
echo "  - otherix-api / otherix-agent packages (apt purge) + the otherix CLI"
echo "  - /var/lib/otherix (etcd data, CA, certs, pools, VM state) and /etc/otherix"
echo "  - the CLI config (~/.otherix) and the 'otherix' system user"
echo "  - otherix-managed bridges and nft rules"

# Confirm: an explicit FORCE, or an interactive 'yes'. A piped run with no FORCE
# refuses, so a blind 'curl | sudo sh' never wipes a host without consent.
confirm() {
	case "${FORCE:-}" in
		1 | true | yes | y | Y) return 0 ;;
	esac
	if [ -t 0 ]; then
		printf '\nType "yes" to proceed: '
		read -r _ans
		[ "$_ans" = "yes" ] && return 0
		echo "aborted"
		exit 0
	fi
	echo >&2
	echo "uninstall: refusing without confirmation. Re-run interactively, or pass FORCE=true:" >&2
	echo "  curl -fsSL https://get.otherix.dev/uninstall.sh | sudo FORCE=true sh" >&2
	exit 1
}
confirm

# ---- stop + disable services -------------------------------------------
step "Stopping services"
systemctl disable --now otherix-api.service otherix-agent.service >/dev/null 2>&1 || true
# Kill any stray manual / dev-stack processes that systemd does not manage.
pkill -f 'otherix-api|otherix-agent' 2>/dev/null || true

# ---- purge packages -----------------------------------------------------
step "Purging packages"
if have apt-get; then
	apt-get purge -y otherix-api otherix-agent >/dev/null 2>&1 || true
	apt-get autoremove -y >/dev/null 2>&1 || true
elif have dpkg; then
	dpkg --purge otherix-api otherix-agent >/dev/null 2>&1 || true
fi

# ---- remove the CLI (installed outside dpkg) ---------------------------
step "Removing the CLI"
rm -f /usr/local/bin/otherix

# ---- tear down otherix-managed network devices (best-effort) -----------
step "Removing managed network devices"
if have ip; then
	for _dev in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | sed 's/@.*//'); do
		case "$_dev" in
			otb[0-9]* | otvb[0-9]* | otvx[0-9]* | otwg[0-9]* | oxbr[0-9]*) ip link del "$_dev" 2>/dev/null || true ;;
		esac
	done
fi
if have nft; then
	# Delete every nft table whose name contains "otherix" (otherix-nat, ...).
	nft list tables 2>/dev/null | awk '/otherix/ {print $2, $3}' | while read -r _fam _name; do
		[ -n "$_name" ] && nft delete table "$_fam" "$_name" 2>/dev/null || true
	done
fi

# ---- wipe state + config ------------------------------------------------
step "Wiping state and config"
rm -rf /var/lib/otherix /etc/otherix
rm -rf /root/.otherix
if [ -n "${SUDO_USER:-}" ]; then
	_home="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
	[ -n "$_home" ] && rm -rf "$_home/.otherix"
fi

# ---- remove the system user / group ------------------------------------
step "Removing the otherix system user"
if getent passwd otherix >/dev/null 2>&1; then userdel otherix 2>/dev/null || true; fi
if getent group otherix >/dev/null 2>&1; then groupdel otherix 2>/dev/null || true; fi

systemctl daemon-reload >/dev/null 2>&1 || true

# ---- report -------------------------------------------------------------
echo
echo "Otherix removed. The ports it uses should now be free:"
if have ss && ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE '[:.](8080|8443|9443|2379|2380)$'; then
	echo "  WARNING: something is still listening:"
	ss -ltn 2>/dev/null | awk '{print $4}' | grep -E '[:.](8080|8443|9443|2379|2380)$' | sed 's/^/    /'
else
	echo "  8080 / 8443 / 9443 / 2379 / 2380 all clear."
fi
