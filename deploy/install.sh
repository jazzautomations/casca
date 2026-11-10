#!/bin/sh
# Instala/atualiza o casca num host. Uso (da máquina de build):
#   ./deploy/install.sh root@HOST USUARIO
# Precisa de root no destino só pra criar o serviço runit.
set -eu
DEST=${1:?uso: install.sh root@host usuario}
RUNAS=${2:?uso: install.sh root@host usuario}
HERE=$(cd "$(dirname "$0")/.." && pwd)
cd "$HERE"
CGO_ENABLED=0 GOOS=linux GOARCH=386 go build -trimpath -ldflags "-s -w" -o casca .
scp -q casca deploy/run deploy/log-run gui/casca-gui deploy/casca.desktop "$DEST:/tmp/"
ssh "$DEST" RUNAS="$RUNAS" sh -s <<'REMOTE'
set -eu
H=$(getent passwd "$RUNAS" | cut -d: -f6)
install -d -o "$RUNAS" -g "$RUNAS" "$H/.local/bin"
[ -f "$H/.local/bin/casca" ] && cp -p "$H/.local/bin/casca" "$H/.local/bin/casca.prev"
install -o "$RUNAS" -g "$RUNAS" -m 755 /tmp/casca "$H/.local/bin/casca.new"
mv "$H/.local/bin/casca.new" "$H/.local/bin/casca"
# janela nativa (GTK3 + VTE): precisa só do widget de terminal
python3 -c 'import gi; gi.require_version("Vte","2.91")' 2>/dev/null || \
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends gir1.2-vte-2.91 python3-gi gir1.2-gtk-3.0 >/dev/null
install -o "$RUNAS" -g "$RUNAS" -m 755 /tmp/casca-gui "$H/.local/bin/casca-gui"
install -d -o "$RUNAS" -g "$RUNAS" "$H/.local/share/applications"
sed "s#^Exec=.*#Exec=$H/.local/bin/casca-gui#" /tmp/casca.desktop > "$H/.local/share/applications/casca.desktop"
chown "$RUNAS:$RUNAS" "$H/.local/share/applications/casca.desktop"
[ -d "$H/Desktop" ] && install -o "$RUNAS" -g "$RUNAS" -m 755 "$H/.local/share/applications/casca.desktop" "$H/Desktop/casca.desktop"
mkdir -p /etc/sv/casca/log
sed "s#@HOME@#$H#g; s#@USER@#$RUNAS#g" /tmp/run > /etc/sv/casca/run
cp /tmp/log-run /etc/sv/casca/log/run
chmod 755 /etc/sv/casca/run /etc/sv/casca/log/run
rm -f /tmp/casca /tmp/run /tmp/log-run /tmp/casca-gui /tmp/casca.desktop
SVDIR=/etc/service; [ -d "$SVDIR" ] || SVDIR=/etc/runit/runsvdir/default
[ -e "$SVDIR/casca" ] || ln -s /etc/sv/casca "$SVDIR/casca"
sleep 6
sv restart casca >/dev/null 2>&1 || true
sleep 2
sv status casca
su - "$RUNAS" -c "$H/.local/bin/casca url"
REMOTE
