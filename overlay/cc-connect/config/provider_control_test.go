package config

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestProviderControlConfigRoundTripPreservesLegacyFields(t *testing.T) {
	input := `[[projects]]
name = "desktop"
[projects.provider_control]
config_file = "control-api.json"
app = "claude"
proxy_provider = "cc-switch"
[[projects]]
name = "coding"
[projects.provider_control]
db_path = "cc-switch.db"
app_type = "codex"
`
	var cfg Config
	if _, err := toml.Decode(input, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 2 || cfg.Projects[1].ProviderControl == nil {
		t.Fatal("integration configuration was silently dropped")
	}
	var out bytes.Buffer
	if err := toml.NewEncoder(&out).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	var reloaded Config
	if _, err := toml.Decode(out.String(), &reloaded); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Projects {
		if !reflect.DeepEqual(cfg.Projects[i].ProviderControl, reloaded.Projects[i].ProviderControl) {
			t.Fatal("configuration round trip lost provider control fields")
		}
	}
}
