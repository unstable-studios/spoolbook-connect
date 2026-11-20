#!/bin/sh
# End to end against a real Prusa Connect demo: installs Materials (without
# its browser half) and this module, points the module at a stand-in for
# spoolbook on 127.0.0.1:8787, adds a spool on a printer's tool, links it, and
# watches what goes out.
#
# Run it in a Go container from the repository root:
#
#   git clone --depth 1 --branch v0.1.0-beta.1 https://github.com/prusa3d/prusa-connect.git .core
#   podman run --rm -v "$PWD":/mod:Z -v "$PWD/.core":/core:Z -v gomod:/go/pkg/mod \
#     docker.io/library/golang:1.27 sh /mod/e2e/run.sh
#
# The demo's printers report no filament use, so every print goes with
# grams 0. What it shows is the wiring: the events, the Materials routes, the
# record store, the settings and the requests spoolbook gets.
set -eu
W=${WORK:-/tmp/e2e}
CORE=${CORE:-/core}
MOD=${MOD:-/mod}
rm -rf "$W" && mkdir -p "$W/rel-sb" "$W/rel-mat" "$W/demo"

(cd "$CORE" && go build -o "$W/prusa-connect" ./cmd/prusa-connect && go build -o "$W/demo-server" ./cmd/demo)
(cd "$MOD" && CGO_ENABLED=0 go build -o "$W/rel-sb/spoolbook_linux_amd64" . && cp module.yaml openapi.json "$W/rel-sb/")
(cd "$CORE/extensions/materials" && CGO_ENABLED=0 go build -o "$W/rel-mat/materials_linux_amd64" . &&
	sed '/^ui:/,$d' module.yaml > "$W/rel-mat/module.yaml" && cp openapi.json "$W/rel-mat/")
(cd "$MOD/e2e/fake" && go build -o "$W/fake" .)

cat > "$W/config.yaml" <<YAML
core:
  overlay: $W/demo/resources.json
  modules: $W/demo/modules
  authz:
    database: $W/demo/config.d/authz.db
resources:
  db:
    main:
      type: sqlite
      config:
        path: $W/state.db
YAML
printf '{"spoolbook_url":"http://127.0.0.1:8787","token":"sbk_0123456789abcdef_abcdefghijABCDEFGHIJ0123456789_-abcdefghijk","library":"lib_0123456789abcdef"}' > "$W/settings.json"

cd "$W"
MAT_VERSION=$(sed -n 's/^version:[[:space:]]*//p' rel-mat/module.yaml)
./prusa-connect module add "github.com/prusa3d/materials@$MAT_VERSION" -config config.yaml -from rel-mat -approve -database sqlite
./prusa-connect module add github.com/unstable-studios/spoolbook-connect@'*' -config config.yaml -from rel-sb -approve -settings settings.json
./fake spoolbook.log &
./demo-server -config config.yaml -dir demo -addr 127.0.0.1:18080 -printers 1 -flap 0 -cameras=false \
	-print-gap 20s -password demodemo1 -log-level info > demo.log 2>&1 &
sleep 8

(cd "$MOD/e2e/drive" && go run .)

echo "=== the module's log"
grep "resource=spoolbook" demo.log | grep module_stderr || true
echo "=== what spoolbook got"
cat spoolbook.log
