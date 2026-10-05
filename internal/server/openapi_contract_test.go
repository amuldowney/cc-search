package server

import (
	"encoding/json"
	"testing"
)

func TestOpenAPIRefreshAndSearchDefaults(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Ref string `json:"$ref"`
			}
		}
		Components struct {
			Parameters map[string]struct{ Schema struct{ Default any } }
			Schemas    map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	if document.Components.Parameters["Limit"].Schema.Default != float64(20) {
		t.Fatal("documented search default must be20")
	}
	if _, ok := document.Paths["/v1/refresh"]["post"]; !ok {
		t.Fatal("missing synchronous refresh endpoint")
	}
	if _, ok := document.Components.Schemas["Freshness"]; !ok {
		t.Fatal("missing freshness schema")
	}
	for _, path := range []string{"/v1/search", "/v1/last", "/v1/read", "/v1/context", "/v1/commands", "/v1/sessions", "/v1/info"} {
		found := false
		for _, parameter := range document.Paths[path]["get"].Parameters {
			found = found || parameter.Ref == "#/components/parameters/Refresh"
		}
		if !found {
			t.Errorf("%s does not document refresh=true", path)
		}
	}
}

func TestOpenAPIRetrievalControls(t *testing.T) {
	var d struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Ref  string `json:"$ref"`
				Name string `json:"name"`
			}
		}
	}
	if err := json.Unmarshal(openAPIDocument, &d); err != nil {
		t.Fatal(err)
	}
	for path, names := range map[string][]string{
		"/v1/search":   {"CWD", "Hours", "PerSession", "ReduceNoise"},
		"/v1/context":  {"CWD", "Hours", "WindowHours", "WindowMessages", "PerSession", "ReduceNoise"},
		"/v1/commands": {"CWD", "Hours", "IncludeCurrent", "CommandMatch"},
		"/v1/last":     {"CWD", "WindowHours", "limit"},
	} {
		for _, name := range names {
			found := false
			for _, p := range d.Paths[path]["get"].Parameters {
				found = found || p.Ref == "#/components/parameters/"+name || p.Name == name
			}
			if !found {
				t.Errorf("%s missing %s", path, name)
			}
		}
	}
}
