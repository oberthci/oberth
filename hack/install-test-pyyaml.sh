#!/bin/sh
# Public test dependency only; never used by credentialed release consumers.
set -eu
root=${OBERTH_TOOLS_DIR:?OBERTH_TOOLS_DIR is required}
mkdir -p "$root/bin" "$root/python"
archive=$root/pyyaml-6.0.3.tar.gz
curl --fail --silent --show-error --proto =https --tlsv1.3 \
  --connect-timeout 10 --max-time 90 --retry 2 \
  -o "$archive" \
  https://files.pythonhosted.org/packages/05/8e/961c0007c59b8dd7729d542c61a4d537767a59645b82a0b521206e1e25c2/pyyaml-6.0.3.tar.gz
printf '%s  %s\n' d76623373421df22fb4cf8817020cbb7ef15c725b9d5e45f17e189bfc384190f "$archive" | sha256sum -c -
tar -xzf "$archive" -C "$root/python" --strip-components=2 pyyaml-6.0.3/lib/yaml
rm -f "$archive"
