#!/usr/bin/env bash
# PhoneBorg installer for Debian and Ubuntu (docs/INSTALL.md#installer).
#
#   deploy/install.sh host     systemd services (a PC, a NUC or a VM with USB passthrough)
#   deploy/install.sh docker   the same cluster in Docker containers
#
# Without a checkout:
#   curl -fsSL https://raw.githubusercontent.com/dzaczek/phoneborg/main/deploy/install.sh | bash -s -- host
#
# Run it as the normal user whose adb key the phones will accept, not as
# root; it asks for sudo where needed. Running it again is safe and is how
# you update: it keeps what exists (admin token, models, service files,
# Prometheus and Grafana config, compose.env), pulls the code if the
# checkout is clean, rebuilds and restarts.
#
# Settings (environment variables):
#   LAN=192.168.1.0/24   networks that may use the endpoint without an API key, comma separated
#   HOST_IP=<ip>         the address you open the panel at (default: the host's first address)
#   GRAFANA=0            skip Prometheus and Grafana
#   LLAMA_FROM=<dir>     llama.cpp for the phones built elsewhere (a bin/llama directory, local
#                        or user@host:path for rsync); skips the Docker build
set -euo pipefail

MODE=${1:-}
PB=/srv/phoneborg
APP=$PB/app
REPO=${REPO:-https://github.com/dzaczek/phoneborg}
GO_VERSION=${GO_VERSION:-1.27.1}
MODEL=qwen2.5-0.5b-instruct-q4_k_m.gguf
MODEL_URL=https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/$MODEL
LLAMA_VARIANTS="armv8.2-a+dotprod+fp16 armv8.2-a+fp16 armv8-a"
GRAFANA=${GRAFANA:-1}
TRUSTED=127.0.0.0/8,::1/128${LAN:+,$LAN}
HOST_IP=${HOST_IP:-$(hostname -I | awk '{print $1}')}
USER=${USER:-$(id -un)}
COMPOSE=(docker compose --env-file "$PB/compose.env" -f "$APP/deploy/docker-compose.linux.yml")
RELOGIN=

step() { printf '\n==> %s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
apt_get() { sudo env DEBIAN_FRONTEND=noninteractive apt-get -q "$@"; }

case $MODE in
host | docker) ;;
*) die "usage: install.sh host|docker (see the top of this file for settings)" ;;
esac
[ "$(uname -s)" = Linux ] || die "this installer is for Linux; on macOS follow docs/QUICKSTART.md"
[ "$(id -u)" -ne 0 ] || die "run it as the normal user that owns the phones' adb key, not as root"
command -v apt-get >/dev/null || die "needs Debian or Ubuntu (apt-get)"
sudo -v

# The two modes share ports and the phones' adb server: only one may run.
if [ "$MODE" = docker ]; then
	for s in phoneborg-controller phoneborg-pcprov prometheus grafana-server; do
		! systemctl is-active -q "$s" ||
			die "$s runs on this host; stop it first: sudo systemctl disable --now $s"
	done
elif command -v docker >/dev/null &&
	[ -n "$(sudo docker ps -q --filter label=com.docker.compose.project=phoneborg-linux)" ]; then
	die "the Docker install runs; stop it first: sudo ${COMPOSE[*]} --profile monitoring down"
fi

step "System packages"
apt_get update
apt_get install -y --no-upgrade git make curl ca-certificates openssl adb rsync file
if ! id -nG "$USER" | grep -qw plugdev; then
	sudo usermod -aG plugdev "$USER"
	RELOGIN=1
fi

step "Folders in $PB"
sudo mkdir -p "$PB/state/models"
sudo chown "$USER": "$PB" "$PB/state" "$PB/state/models"
mkdir -p "$HOME/.android"

step "Code in $APP"
if [ -d "$APP/.git" ]; then
	if [ -z "$(git -C "$APP" status --porcelain --untracked-files=no)" ]; then
		git -C "$APP" pull --ff-only
	else
		echo "local changes in $APP: building them as they are, not pulling"
	fi
elif [ -e "$APP" ]; then
	die "$APP exists but is not a git checkout"
else
	git clone "$REPO" "$APP"
fi

install_docker() {
	if command -v docker >/dev/null && sudo docker buildx version >/dev/null 2>&1; then
		return
	fi
	step "Docker"
	# shellcheck source=/dev/null
	. /etc/os-release
	sudo install -m 0755 -d /etc/apt/keyrings
	sudo curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$ID $VERSION_CODENAME stable" |
		sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
	apt_get update
	apt_get install -y --no-upgrade docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
	sudo usermod -aG docker "$USER"
	RELOGIN=1
}

have_llama() {
	for v in $LLAMA_VARIANTS; do
		[ -x "$APP/bin/llama/$v/llama-server" ] || return 1
	done
}

step "llama.cpp for the phones"
if have_llama; then
	echo "already built in $APP/bin/llama"
elif [ -n "${LLAMA_FROM:-}" ]; then
	mkdir -p "$APP/bin/llama"
	rsync -a "${LLAMA_FROM%/}/" "$APP/bin/llama/"
	have_llama || die "LLAMA_FROM=$LLAMA_FROM lacks one of: $LLAMA_VARIANTS"
else
	install_docker
	# The phones are arm64: on an x86-64 host the build runs under QEMU.
	if [ "$(uname -m)" = x86_64 ]; then
		sudo docker run --privileged --rm tonistiigi/binfmt --install arm64
		echo "building under QEMU: about an hour"
	fi
	sudo make -C "$APP" llama-all
	sudo chown -R "$USER": "$APP/bin/llama"
fi

step "Bootstrap model and admin token"
if [ ! -s "$PB/state/models/$MODEL" ]; then
	curl -fL --retry 3 -o "$PB/state/models/$MODEL.part" "$MODEL_URL"
	mv "$PB/state/models/$MODEL.part" "$PB/state/models/$MODEL"
fi
if [ ! -s "$PB/admin-token" ]; then
	(umask 077 && openssl rand -hex 32 >"$PB/admin-token")
fi

# put_file DST: installs stdin as DST unless DST exists (your edits are kept).
put_file() {
	if sudo test -e "$1"; then
		echo "kept $1"
		cat >/dev/null
	else
		sudo tee "$1" >/dev/null
		echo "wrote $1"
	fi
}

grafana_url=http://127.0.0.1:3000
[ "$GRAFANA" = 1 ] || grafana_url=

if [ "$MODE" = host ]; then
	step "Go"
	export PATH=$PATH:/usr/local/go/bin
	minor=$( (go env GOVERSION 2>/dev/null || true) | sed -nE 's/^go1\.([0-9]+).*/\1/p')
	if [ "${minor:-0}" -lt 25 ]; then
		sudo rm -rf /usr/local/go
		curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-$(dpkg --print-architecture).tar.gz" |
			sudo tar -C /usr/local -xz
		# shellcheck disable=SC2016 # $PATH expands at login
		grep -qs /usr/local/go/bin "$HOME/.profile" ||
			echo 'export PATH=$PATH:/usr/local/go/bin' >>"$HOME/.profile"
	fi
	go version

	step "Build"
	make -C "$APP" agent pcprov controller pbctl

	step "Services"
	for s in phoneborg-controller phoneborg-pcprov; do
		sed -e "s/__USER__/$USER/" \
			-e "s|-trusted-cidrs 127.0.0.0/8,::1/128|-trusted-cidrs $TRUSTED|" \
			-e "s|-grafana-url http://127.0.0.1:3000|${grafana_url:+-grafana-url $grafana_url}|" \
			"$APP/deploy/host/$s.service" | put_file "/etc/systemd/system/$s.service"
	done
	sudo systemctl daemon-reload
	sudo systemctl enable -q phoneborg-controller phoneborg-pcprov
	sudo systemctl restart phoneborg-controller phoneborg-pcprov
	# A plain file, never through an old link to bin/pbctl.
	sudo rm -f /usr/local/bin/pbctl
	printf '#!/bin/sh\nexec %s/bin/pbctl -token-file %s/admin-token "$@"\n' "$APP" "$PB" |
		sudo tee /usr/local/bin/pbctl >/dev/null
	sudo chmod 755 /usr/local/bin/pbctl

	if [ "$GRAFANA" = 1 ]; then
		step "Prometheus and Grafana"
		apt_get install -y --no-upgrade prometheus gpg
		if [ ! -e /etc/apt/sources.list.d/grafana.list ]; then
			sudo mkdir -p /etc/apt/keyrings
			curl -fsSL https://apt.grafana.com/gpg.key | gpg --dearmor | sudo tee /etc/apt/keyrings/grafana.gpg >/dev/null
			echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg] https://apt.grafana.com stable main" |
				sudo tee /etc/apt/sources.list.d/grafana.list >/dev/null
			apt_get update
		fi
		apt_get install -y --no-upgrade grafana
		# The package's own prometheus.yml does not scrape PhoneBorg: replace it once.
		if ! grep -qs phoneborg-controller /etc/prometheus/prometheus.yml; then
			sudo cp -n /etc/prometheus/prometheus.yml /etc/prometheus/prometheus.yml.orig 2>/dev/null || true
			sudo cp "$APP/deploy/host/prometheus.yml" /etc/prometheus/prometheus.yml
		fi
		grep -qs created-timestamp-zero-ingestion /etc/default/prometheus ||
			sudo sed -i 's|^ARGS="|ARGS="--enable-feature=created-timestamp-zero-ingestion |' /etc/default/prometheus
		put_file /etc/grafana/provisioning/datasources/phoneborg.yml <"$APP/deploy/host/grafana-datasource.yml"
		put_file /etc/grafana/provisioning/dashboards/phoneborg.yml <"$APP/deploy/host/grafana-dashboards.yml"
		sudo mkdir -p /etc/systemd/system/grafana-server.service.d
		sed "s/__HOST__/$HOST_IP/" "$APP/deploy/host/grafana-phoneborg.conf" |
			put_file /etc/systemd/system/grafana-server.service.d/phoneborg.conf
		sudo systemctl daemon-reload
		sudo systemctl enable -q prometheus grafana-server
		sudo systemctl restart prometheus grafana-server
	fi
else
	install_docker
	step "Containers"
	put_file "$PB/compose.env" <<EOF
# Settings for deploy/docker-compose.linux.yml, written by deploy/install.sh.
PB_UID=$(id -u)
PB_GID=$(id -g)
PB_PLUGDEV_GID=$(getent group plugdev | cut -d: -f3)
PB_ADB_HOME=$HOME/.android
PB_HOST=$HOST_IP
PB_TRUSTED=$TRUSTED
PB_GRAFANA_URL=$grafana_url
COMPOSE_PROFILES=${grafana_url:+monitoring}
EOF
	sudo chown "$USER": "$PB/compose.env"
	# A host adb server would hold the phones' USB interfaces.
	adb kill-server 2>/dev/null || true
	sudo env PB_VERSION="$(date +%Y%m%d%H%M)" "${COMPOSE[@]}" up -d --build
	sudo rm -f /usr/local/bin/pbctl
	# shellcheck disable=SC2016 # expands in the wrapper
	printf '#!/bin/sh\n# pbctl inside the controller container (deploy/install.sh docker).\nD=docker; docker info >/dev/null 2>&1 || D="sudo docker"\nexec $D compose --env-file %s/compose.env -f %s/deploy/docker-compose.linux.yml exec -T controller pbctl -token-file %s/admin-token "$@"\n' "$PB" "$APP" "$PB" |
		sudo tee /usr/local/bin/pbctl >/dev/null
	sudo chmod 755 /usr/local/bin/pbctl
fi

step "Check"
for _ in $(seq 30); do
	curl -fsS -o /dev/null http://127.0.0.1:18080/healthz && break
	sleep 2
done
curl -fsS -o /dev/null http://127.0.0.1:18080/healthz || die "the controller does not answer on :18080; see the logs (docs/INSTALL.md#if-something-is-off)"
echo "controller: ok"

if [ "$MODE" = host ]; then
	logs="journalctl -u phoneborg-pcprov -f"
	devices="adb devices -l"
else
	logs="sudo ${COMPOSE[*]} logs -f pcprov"
	devices="sudo ${COMPOSE[*]} exec pcprov adb devices -l"
fi
cat <<EOF

PhoneBorg is installed ($MODE).

  Web panel     http://$HOST_IP:18080/ui/
  Admin token   cat $PB/admin-token   (sign in to the panel with it)
  API           http://$HOST_IP:18080/v1  (OpenAI-compatible, model "auto")

Next: plug in a phone with USB debugging on, unlock it and tap Allow
(tick "Always allow from this computer"). Within two minutes:

  $devices     # the phone shows "device"
  pbctl nodes                          # BENCHMARKING, then ACTIVE
  $logs   # provisioning as it happens

Then give the phones real models: docs/INSTALL.md#11-models-pools-and-the-router
EOF
if [ -n "$RELOGIN" ]; then
	echo
	echo "Log out and in once: you were added to the plugdev or docker group."
fi
