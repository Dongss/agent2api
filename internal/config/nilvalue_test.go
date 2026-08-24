package config

import (
	"strings"
	"testing"
)

// A key written without a value is not the same as a key set to the zero value.
// YAML parses `host:` as nil, and merged over the defaults nil would replace
// them — blanking the listen address, removing the request cap, or zeroing a
// whole section. It has to read as "not written".
func TestValuelessKeysKeepTheirDefaults(t *testing.T) {
	cases := []struct{ name, body string }{
		{"leaf", "server:\n  host:\n"},
		{"unvalidated leaf", "server:\n  request_timeout:\n"},
		{"whole section", "server:\n"},
		{"log section", "log:\n"},
		{"adapters section", "adapters:\n"},
		{"one adapter", "adapters:\n  claude-code:\n"},
		{"several at once", "server:\n  host:\n  port:\nlog:\n  level:\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := loadWith(t, tc.body)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			def := Defaults()
			if cfg.Server.Host != def.Server.Host {
				t.Errorf("host = %q, want the default %q", cfg.Server.Host, def.Server.Host)
			}
			if cfg.Server.Port != def.Server.Port {
				t.Errorf("port = %d, want the default %d", cfg.Server.Port, def.Server.Port)
			}
			if cfg.Server.RequestTimeout != def.Server.RequestTimeout {
				t.Errorf("request_timeout = %v, want the default %v",
					cfg.Server.RequestTimeout, def.Server.RequestTimeout)
			}
			if cfg.Log.Level != def.Log.Level {
				t.Errorf("log.level = %q, want the default %q", cfg.Log.Level, def.Log.Level)
			}
			if got := cfg.Adapters["claude-code"].Binary; got != def.Adapters["claude-code"].Binary {
				t.Errorf("claude-code.binary = %q, want the default", got)
			}
		})
	}
}

// A stripped key set nothing, so the report must not claim the file did.
func TestValuelessKeysAreNotAttributedToTheFile(t *testing.T) {
	_, prov, err := loadWith(t, "server:\n  host:\n  port: 9000\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := prov.source("server.host"); got != SourceDefault {
		t.Errorf("server.host source = %s, want default", got)
	}
	if got := prov.source("server.port"); got != SourceFile {
		t.Errorf("server.port source = %s, want file", got)
	}
}

// A file that writes an empty map means it; only a map left empty by stripping
// is dropped with its children.
func TestStripNilKeepsAnIntentionallyEmptyMap(t *testing.T) {
	in := map[string]any{
		"kept":    map[string]any{},                       // written as {}
		"dropped": map[string]any{"child": nil},           // empty only after stripping
		"deep":    map[string]any{"a": nil, "b": "value"}, // partially stripped
		"nil":     nil,
		"scalar":  "x",
	}
	out := stripNil(in)

	if _, ok := out["kept"]; !ok {
		t.Error(`an explicitly empty map must survive`)
	}
	if _, ok := out["dropped"]; ok {
		t.Error("a map emptied by stripping must go with its children")
	}
	if _, ok := out["nil"]; ok {
		t.Error("a nil value must be dropped")
	}
	if out["scalar"] != "x" {
		t.Errorf("scalar = %v, want it untouched", out["scalar"])
	}
	deep, _ := out["deep"].(map[string]any)
	if _, ok := deep["a"]; ok {
		t.Error("the nil child should be gone")
	}
	if deep["b"] != "value" {
		t.Errorf("deep.b = %v, want it kept", deep["b"])
	}
}

// An empty host is the widest bind net.Listen offers, not the narrowest, so it
// needs a bearer key exactly like 0.0.0.0 does. It used to pass as loopback,
// which meant a blank host quietly served the whole network unauthenticated.
func TestEmptyHostIsNotLoopback(t *testing.T) {
	if isLoopback("") {
		t.Fatal(`isLoopback("") must be false: net.Listen binds every interface for it`)
	}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		if !isLoopback(host) {
			t.Errorf("isLoopback(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "0.0.0.0", "192.168.1.10", "::"} {
		if isLoopback(host) {
			t.Errorf("isLoopback(%q) = true, want false", host)
		}
	}
}

func TestEmptyHostRequiresAKey(t *testing.T) {
	_, _, err := Load(newFlags(t, "--host", ""))
	if err == nil {
		t.Fatal("an empty host without a bearer key must be refused")
	}
	if !strings.Contains(err.Error(), "every interface") {
		t.Errorf("the message should say what an empty host means: %v", err)
	}
	if _, _, err := Load(newFlags(t, "--host", "", "--api-key", "sk-x")); err != nil {
		t.Errorf("an empty host with a bearer key is the user's call: %v", err)
	}
}
