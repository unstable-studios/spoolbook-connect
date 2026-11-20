package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prusa3d/prusa-connect/sdk/manifest"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
	"gopkg.in/yaml.v3"
)

// TestManifestParses reads module.yaml with the SDK's own parser, which is
// what `prusa-connect module add` holds a release to.
func TestManifestParses(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Parse("module.yaml", raw, yaml.Unmarshal)
	if err != nil {
		t.Fatal(err)
	}

	if m.Name != name || m.Version != version {
		t.Errorf("module.yaml is %s %s, main.go is %s %s", m.Name, m.Version, name, version)
	}

	if len(m.Egress) != 1 || m.Egress[0].Option != "spoolbook_url" {
		t.Errorf("egress = %+v", m.Egress)
	}

	if !slices.Contains(m.Calls, "com.github.aux.prusa3d.materials") || !slices.Contains(m.Permissions, "observe") {
		t.Errorf("calls = %v, permissions = %v", m.Calls, m.Permissions)
	}
}

// TestOptionsMatchSettings: plugin.configure decodes strictly, so every
// option has to be a Settings field and the other way round.
func TestOptionsMatchSettings(t *testing.T) {
	t.Parallel()

	raw, _ := os.ReadFile("module.yaml")
	m, err := manifest.Parse("module.yaml", raw, yaml.Unmarshal)
	if err != nil {
		t.Fatal(err)
	}

	sent := map[string]string{}
	for _, o := range m.Options {
		sent[o.Key] = "x"
	}

	body, _ := json.Marshal(wirerpc.Settings{Config: mustJSON(t, sent)})
	if _, err := wirerpc.ReadSettings[Settings](body); err != nil {
		t.Errorf("the options don't decode into Settings: %v", err)
	}

	fields := regexp.MustCompile(`json:"([a-z_]+)"`).FindAllStringSubmatch(settingsSource(t), -1)
	if len(fields) != len(sent) {
		t.Errorf("Settings has %d fields, module.yaml %d options", len(fields), len(sent))
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func settingsSource(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	start := strings.Index(string(src), "type Settings struct {")
	end := strings.Index(string(src)[start:], "}")

	return string(src)[start : start+end]
}

// TestOpenAPIIsWhatTheHostAccepts: a module whose fragment the host refuses
// doesn't start.
func TestOpenAPIIsWhatTheHostAccepts(t *testing.T) {
	t.Parallel()

	f, err := routesapi.ParseFragment(openAPI)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"/links", "/links/{materials_spool}", "/page", "/spools", "/status"}
	if got := f.Paths(); !slices.Equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}

	if _, err := routesapi.Describe(wirerpc.Info{Name: name, Version: version, Contract: wirerpc.Contract}, openAPI); err != nil {
		t.Error(err)
	}
}
