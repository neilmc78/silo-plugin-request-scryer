package main

import (
	"testing"

	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

// The embedded manifest must load with the SDK, including the server form.
func TestManifestLoadsWithServerForm(t *testing.T) {
	m, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range m.GetCapabilities()[0].GetConfigSchema() {
		for _, f := range s.GetAdminForm().GetFields() {
			keys = append(keys, f.GetKey())
		}
	}
	if len(keys) != 1 || keys[0] != "anime_movies_library" {
		t.Fatalf("form fields = %v", keys)
	}
}
