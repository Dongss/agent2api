package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// loadWith writes body as the config file and loads it.
func loadWith(t *testing.T, body string) (*Config, *Provenance, error) {
	t.Helper()
	isolate(t)
	path := filepath.Join(t.TempDir(), "agent2api.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(newFlags(t, "--config", path))
}

func TestSecondsAccepted(t *testing.T) {
	cases := []struct {
		name string
		body string
		get  func(Server) Seconds
		want Seconds
	}{
		{"plain number", "server:\n  idle_timeout: 90\n",
			func(s Server) Seconds { return s.IdleTimeout }, 90},
		{"quoted number", "server:\n  idle_timeout: \"90\"\n",
			func(s Server) Seconds { return s.IdleTimeout }, 90},
		{"zero disables", "server:\n  heartbeat_interval: 0\n",
			func(s Server) Seconds { return s.HeartbeatInterval }, 0},
		{"a day", "server:\n  request_timeout: 86400\n",
			func(s Server) Seconds { return s.RequestTimeout }, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := loadWith(t, tc.body)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.get(cfg.Server); got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

// The property the Seconds type exists for: a config file's bare number is
// seconds. Decoded straight into a time.Duration it would have been 90
// nanoseconds, a timeout that kills every CLI the instant it starts.
func TestBareNumberIsSeconds(t *testing.T) {
	cfg, _, err := loadWith(t, "server:\n  idle_timeout: 90\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Server.IdleTimeout.Duration(); got != 90*time.Second {
		t.Errorf("idle_timeout: 90 became %v, want 90s", got)
	}
}

// Anything that is not a number is refused. The message names the field, which
// is what a config typo needs.
func TestSecondsRejectsNonNumbers(t *testing.T) {
	for _, value := range []string{"600s", "10m", "banana", "[1, 2]"} {
		t.Run(value, func(t *testing.T) {
			_, _, err := loadWith(t, "server:\n  idle_timeout: "+value+"\n")
			if err == nil {
				t.Fatalf("%s was accepted; this field takes a number of seconds", value)
			}
			if !strings.Contains(err.Error(), "idle_timeout") {
				t.Errorf("message should name the field: %v", err)
			}
		})
	}
}

func TestSecondsRejectsNegative(t *testing.T) {
	_, _, err := loadWith(t, "server:\n  idle_timeout: -5\n")
	if err == nil {
		t.Fatal("a negative timeout must be refused")
	}
	if !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("message = %v", err)
	}
}

// `config print` renders a bare number, which is ambiguous on the page once it
// gets large, so it annotates what the number means.
func TestConfigPrintAnnotatesSeconds(t *testing.T) {
	_, prov, err := loadWith(t, "server:\n  request_timeout: 600\n  heartbeat_interval: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Collapse the column padding so the assertions read on one line.
	report := strings.Join(strings.Fields(prov.Report()), " ")
	if !strings.Contains(report, "server.request_timeout = 600 (10m0s)") {
		t.Errorf("a long duration should be annotated:\n%s", prov.Report())
	}
	// Under a minute the number speaks for itself, and "(30s)" is noise.
	if !strings.Contains(report, "server.queue_timeout = 30 (default)") {
		t.Errorf("a short duration should render bare:\n%s", prov.Report())
	}
	if !strings.Contains(report, "server.heartbeat_interval = 0 (file)") {
		t.Errorf("zero should render bare:\n%s", prov.Report())
	}
}

// secondsKeys drives the `config print` annotation, and a field missing from it
// would silently print unannotated. Derive the truth from the struct instead of
// trusting the list.
func TestSecondsKeysCoverEverySecondsField(t *testing.T) {
	want := map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("koanf")
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + Delim + tag
			}
			switch {
			case f.Type == reflect.TypeOf(Seconds(0)):
				want[path] = true
			case f.Type.Kind() == reflect.Struct:
				walk(f.Type, path)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")

	if len(want) == 0 {
		t.Fatal("found no Seconds fields; this test has stopped testing anything")
	}
	for path := range want {
		if !secondsKeys[path] {
			t.Errorf("%s is a Seconds field but is missing from secondsKeys", path)
		}
	}
	for path := range secondsKeys {
		if !want[path] {
			t.Errorf("secondsKeys lists %s, which is not a Seconds field", path)
		}
	}
}
