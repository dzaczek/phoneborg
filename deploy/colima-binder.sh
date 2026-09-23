#!/bin/sh
# Loads the Android binder driver into the colima VM so redroid can boot.
# Also registers the module to load at VM boot, before dockerd starts the
# phone containers (otherwise Android hangs early in boot without binder).
set -eu
colima ssh -- sh -c '
  set -e
  if ! lsmod | grep -q binder_linux; then
    if ! modinfo binder_linux >/dev/null 2>&1; then
      sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
      sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "linux-modules-extra-$(uname -r)"
    fi
    sudo modprobe binder_linux devices=binder,hwbinder,vndbinder
  fi
  echo binder_linux | sudo tee /etc/modules-load.d/binder.conf >/dev/null
  echo "options binder_linux devices=binder,hwbinder,vndbinder" | sudo tee /etc/modprobe.d/binder.conf >/dev/null
  lsmod | grep binder_linux
'
