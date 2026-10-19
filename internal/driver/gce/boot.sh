#!/bin/bash
# Runs as root on every boot of a Computer VM (Container-Optimized OS; cloud-init runs runcmd on every boot there).
# It mounts the home disk, puts the in-VM firewall in place, then opens the engine to the Controller over mutual TLS.
# The engine listens only after the firewall is in place, so an engine that answers is one behind it. The Controller
# does everything after that; the VM never starts the agent by itself.
set -euo pipefail

MD=http://169.254.169.254/computeMetadata/v1
md() { curl -fsS -H 'Metadata-Flavor: Google' "$MD/$1"; }
guest() { curl -fsS -X PUT -H 'Metadata-Flavor: Google' --data-binary "$2" "$MD/instance/guest-attributes/a12s/$1" > /dev/null; }
fail() { guest boot-error "line $1: $BASH_COMMAND" || true; }
trap 'fail $LINENO' ERR

guest boot-error none || true
CONTROLLER=$(md instance/attributes/a12s-controller)
IDENTITY=$(md instance/attributes/a12s-identity)
SELF=$(md instance/network-interfaces/0/ip)

# The home drive: formatted on first use, grown to the disk's size on every boot (online, so a resize needs no step).
HOME_DEV=/dev/disk/by-id/google-home
HOME_DIR=/mnt/disks/a12s-home
if ! blkid "$HOME_DEV" > /dev/null 2>&1; then
  mkfs.ext4 -q -m 0 -E lazy_itable_init=0,lazy_journal_init=0 "$HOME_DEV"
fi
mkdir -p "$HOME_DIR"
mountpoint -q "$HOME_DIR" || mount -o nosuid,nodev "$HOME_DEV" "$HOME_DIR"
resize2fs "$HOME_DEV" > /dev/null 2>&1 || true

# The firewall the VPC can't express: the metadata server (and its DNS) is always reachable from inside a VM, and a
# container can reach its own host. The container may reach only the Controller's proxy and mTLS ports, plus the
# metadata server's HTTP port when the Computer has its own identity. Nothing else, DNS included, gets out.
iptables -N DOCKER-USER 2> /dev/null || true
iptables -F DOCKER-USER
iptables -A DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A DOCKER-USER -i docker0 -d "$CONTROLLER" -p tcp -m multiport --dports 3128,7443 -j RETURN
if [ "$IDENTITY" = per-computer ]; then
  iptables -A DOCKER-USER -i docker0 -d 169.254.169.254 -p tcp --dport 80 -j RETURN
fi
iptables -A DOCKER-USER -i docker0 -j DROP
iptables -A DOCKER-USER -o docker0 -s "$CONTROLLER" -p tcp --dport 7443 -j RETURN
iptables -A DOCKER-USER -o docker0 -j DROP
iptables -C INPUT -i docker0 -j DROP 2> /dev/null || iptables -I INPUT 1 -i docker0 -j DROP
iptables -C INPUT -s "$CONTROLLER" -p tcp --dport 2376 -j ACCEPT 2> /dev/null ||
  iptables -A INPUT -s "$CONTROLLER" -p tcp --dport 2376 -j ACCEPT

# The engine's TLS: the server key is made here and never leaves the boot disk; its certificate is published as a
# guest attribute, which only this VM can write and the Controller pins. Clients must hold a certificate from the
# Fleet's CA with client-auth usage, which only the Controller's has.
TLS=/var/lib/a12s/tls
mkdir -p "$TLS"
chmod 0700 "$TLS"
md instance/attributes/a12s-ca > "$TLS/ca.pem"
if [ ! -s "$TLS/key.pem" ] || [ ! -s "$TLS/cert.pem" ]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
    -subj /CN=a12s-engine -keyout "$TLS/key.pem.new" -out "$TLS/cert.pem.new" 2> /dev/null
  chmod 0600 "$TLS/key.pem.new"
  mv "$TLS/key.pem.new" "$TLS/key.pem"
  mv "$TLS/cert.pem.new" "$TLS/cert.pem"
fi
guest engine-cert "$(cat "$TLS/cert.pem")"

# /etc is stateless on Container-Optimized OS, so this is written fresh on every boot.
touch /etc/default/docker
if ! grep -q -- "--tlsverify" /etc/default/docker; then
  OPTS="-H unix:///var/run/docker.sock -H tcp://$SELF:2376 --tlsverify --tlscacert=$TLS/ca.pem --tlscert=$TLS/cert.pem --tlskey=$TLS/key.pem"
  if grep -q '^DOCKER_OPTS=' /etc/default/docker; then
    sed -i "s|^DOCKER_OPTS=\"\(.*\)\"$|DOCKER_OPTS=\"\1 $OPTS\"|" /etc/default/docker
  else
    echo "DOCKER_OPTS=\"$OPTS\"" >> /etc/default/docker
  fi
  systemctl daemon-reload
  systemctl restart docker
fi
guest ready "$SELF"
