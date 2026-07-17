package chlogcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `change_logs:
  default: CHANGELOG.md
default_change_logs:
  - default
components:
  - fabrik
  - csrf
`

// writeChlog lays out a .chloggen dir with the config, a TEMPLATE.yaml that must be
// ignored, and the given fragments, returning the config path.
func writeChlog(t *testing.T, fragments map[string]string) string {
	t.Helper()
	chlog := filepath.Join(t.TempDir(), ".chloggen")
	if err := os.MkdirAll(chlog, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(chlog, "config.yaml")
	if err := os.WriteFile(cfg, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	// A TEMPLATE.yaml with empty fields must not trip validation.
	tmpl := "change_type: enhancement\ncomponent: fabrik\nnote: \"\"\n"
	if err := os.WriteFile(filepath.Join(chlog, "TEMPLATE.yaml"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range fragments {
		if err := os.WriteFile(filepath.Join(chlog, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		fragment string
		wantErr  string // substring to expect; "" means the fragment must pass
	}{
		{
			// The whole point: a well-formed fragment with no issues passes.
			// chloggen's own validate rejects this ("specify one or more issues").
			name:     "no issues passes",
			fragment: "change_type: enhancement\ncomponent: csrf\nnote: \"Add the csrf library.\"\n",
			wantErr:  "",
		},
		{
			name:     "issues present is accepted too",
			fragment: "change_type: enhancement\ncomponent: csrf\nnote: \"Add the csrf library.\"\nissues: [42]\n",
			wantErr:  "",
		},
		{
			name:     "invalid change_type fails",
			fragment: "change_type: nope\ncomponent: csrf\nnote: \"x\"\n",
			wantErr:  "change_type",
		},
		{
			name:     "empty note fails",
			fragment: "change_type: enhancement\ncomponent: csrf\nnote: \"\"\n",
			wantErr:  "note",
		},
		{
			name:     "unknown component fails",
			fragment: "change_type: enhancement\ncomponent: bogus\nnote: \"x\"\n",
			wantErr:  "component",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := writeChlog(t, map[string]string{"frag.yaml": tt.fragment})
			err := Validate(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected pass, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}
