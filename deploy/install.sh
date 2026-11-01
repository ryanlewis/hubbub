#!/bin/sh
# Install or upgrade hubbub on a systemd host. Run as root on the target, from
# a directory holding the linux binary (./hubbub) and this repo's deploy/ files.
#
#   sh install.sh            # install or upgrade the binary and unit
#
# Idempotent. It never writes config: /etc/hubbub/*.toml are the operator's,
# and the first start fails loudly until they exist. On upgrade the running
# binary is kept as hubbub.prev and restored if the new one doesn't answer
# /health within 10s.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
bin=/opt/hubbub/bin
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ -x "$here/hubbub" ] || { echo "no ./hubbub binary next to install.sh" >&2; exit 1; }

id hubbub >/dev/null 2>&1 || useradd --system --home-dir /var/lib/hubbub --no-create-home --shell /usr/sbin/nologin hubbub
install -d -m 0755 -o root -g root "$bin"
install -d -m 0700 -o hubbub -g hubbub /etc/hubbub

# Rename, not copy, onto the live path: Linux refuses to write over a running
# executable (ETXTBSY).
if [ -e "$bin/hubbub" ]; then
	if cmp -s "$here/hubbub" "$bin/hubbub"; then
		echo "binary unchanged"
	else
		cp -p "$bin/hubbub" "$bin/hubbub.prev"
		install -m 0755 "$here/hubbub" "$bin/hubbub.new"
		mv "$bin/hubbub.new" "$bin/hubbub"
	fi
else
	install -m 0755 "$here/hubbub" "$bin/hubbub"
fi

if ! cmp -s "$here/hubbub.service" /etc/systemd/system/hubbub.service; then
	install -m 0644 "$here/hubbub.service" /etc/systemd/system/hubbub.service
	systemctl daemon-reload
fi
systemctl enable hubbub >/dev/null 2>&1

for f in hubbub.toml keys.toml channels.toml; do
	[ -e "/etc/hubbub/$f" ] || { echo "binary and unit installed; write /etc/hubbub/$f, then: systemctl restart hubbub"; exit 0; }
done

systemctl restart hubbub
port=$(sed -n 's/^public_port *= *\([0-9]*\).*/\1/p' /etc/hubbub/hubbub.toml)
port=${port:-8080}
i=0
until curl -fsS -o /dev/null "http://127.0.0.1:$port/health"; do
	i=$((i + 1))
	if [ "$i" -ge 10 ]; then
		echo "hubbub did not answer /health on :$port" >&2
		if [ -e "$bin/hubbub.prev" ]; then
			mv "$bin/hubbub.prev" "$bin/hubbub"
			systemctl restart hubbub
			echo "rolled back to the previous binary" >&2
		fi
		exit 1
	fi
	sleep 1
done
curl -fsS "http://127.0.0.1:$port/health"; echo
