// Package config defines agent2api's configuration schema and assembles the
// effective config from three layers: built-in defaults, an optional config
// file, and CLI flags — later layers winning per key.
//
// The defaults are Go, not a file: see [Defaults]. They merge as a map rather
// than decoding onto the struct, because decoding replaces a whole
// map[string]Adapter entry, silently dropping the rest of that adapter.
//
// The schema is small on purpose: which backends run is discovered by probing,
// and which models they serve is the CLIs' business.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// Delim is the key separator used for flattened config paths and flag names.
const Delim = "."

// KnownAdapters are the adapter ids the schema accepts. An adapter listed here
// may still be unimplemented in this build; that is reported at startup rather
// than at config load.
var KnownAdapters = []string{"claude-code", "codex", "cursor", "qwen-code"}

// RegisterAdapterID makes an adapter id valid in a config file. Adapter
// packages that are not part of a normal build (the conformance-only mock
// backend) call this from their init, so linking one in is all it takes for its
// config block to be accepted.
func RegisterAdapterID(id string) {
	if !contains(KnownAdapters, id) {
		KnownAdapters = append(KnownAdapters, id)
	}
}

// SandboxModes are the values adapters.codex.sandbox accepts, in order of
// increasing reach. Anything else is a typo, and a typo here would quietly
// widen what the CLI can touch.
var SandboxModes = []string{"read-only", "workspace-write", "danger-full-access"}

// CursorModes are the values adapters.cursor.mode accepts. Both are read-only:
// "ask" answers the question, "plan" proposes a plan for it. The CLI's other
// modes let it edit files, which is not what a completions API is for, so they
// are not offered here.
var CursorModes = []string{"ask", "plan"}

// Seconds is a duration a config file writes as a plain number, with no unit:
// `request_timeout: 600` is ten minutes.
//
// It is its own type rather than a time.Duration because a duration is an int64
// count of nanoseconds, so a config file's `600` would decode as 600ns — a
// timeout that fires the instant it is armed. Naming the unit in the type keeps
// that out of the schema, and the compiler routes every use site through
// [Seconds.Duration].
type Seconds int

// Duration converts to a real duration, for the code that enforces the limit.
func (s Seconds) Duration() time.Duration { return time.Duration(s) * time.Second }

// String renders the value the way a config file spells it, and adds the
// duration it means once that stops being obvious: "30", but "600 (10m0s)".
func (s Seconds) String() string {
	if s < 60 {
		return strconv.Itoa(int(s))
	}
	return fmt.Sprintf("%d (%s)", int(s), s.Duration())
}

// secondsKeys are the config paths measured in [Seconds]. `config print` reads
// this to annotate them, since a bare number is otherwise ambiguous on the
// page. TestSecondsKeysCoverEverySecondsField keeps it honest.
var secondsKeys = map[string]bool{
	"server.request_timeout":    true,
	"server.idle_timeout":       true,
	"server.queue_timeout":      true,
	"server.heartbeat_interval": true,
	"server.shutdown_timeout":   true,
}

// Config is the effective configuration.
type Config struct {
	Server   Server             `koanf:"server"`
	Log      Log                `koanf:"log"`
	Adapters map[string]Adapter `koanf:"adapters"`
}

// Server holds HTTP listener and per-request timeout settings.
type Server struct {
	Host string `koanf:"host"`
	Port int    `koanf:"port"`
	// APIKey is the static bearer key every request must present. Empty means
	// no authentication, which is only allowed on a loopback bind.
	APIKey string `koanf:"api_key"`
	// The four deadlines and the keepalive cadence are all in seconds; see
	// [Seconds] for why they carry no unit.
	RequestTimeout Seconds `koanf:"request_timeout"`
	IdleTimeout    Seconds `koanf:"idle_timeout"`
	QueueTimeout   Seconds `koanf:"queue_timeout"`
	// HeartbeatInterval is how often a streaming response emits a keepalive
	// while the CLI is thinking. Zero disables keepalives.
	HeartbeatInterval Seconds `koanf:"heartbeat_interval"`
	// ShutdownTimeout is how long a shutdown waits for in-flight requests
	// before cutting them off. A streaming answer can be minutes long, so this
	// is not the same order of magnitude as an ordinary web server's.
	ShutdownTimeout Seconds `koanf:"shutdown_timeout"`
	// MaxConcurrency caps how many CLI processes one adapter may run at once.
	MaxConcurrency int `koanf:"max_concurrency"`
	// ScratchDir is the parent of the per-request workdirs; empty means the OS
	// temp directory.
	ScratchDir string `koanf:"scratch_dir"`
}

// Log configures the structured logger.
type Log struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
	File   string `koanf:"file"`
}

// System prompt handling modes, used by adapters whose CLI ships a system
// prompt of its own.
const (
	// SystemPromptAppend adds the caller's system message to the CLI's.
	SystemPromptAppend = "append"
	// SystemPromptReplace sends only the caller's system message.
	SystemPromptReplace = "replace"
)

// Adapter configures one agent CLI backend.
//
// There is no model list: a request names "<adapter>" for the CLI's own default
// model, or "<adapter>:<model>" to pass a model through. The CLI validates the
// name, so agent2api never carries a stale copy of it.
type Adapter struct {
	Binary    string            `koanf:"binary"`
	ExtraArgs []string          `koanf:"extra_args"`
	Env       map[string]string `koanf:"env"`
	// EnvPassthrough names variables the CLI inherits from the gateway's own
	// environment, beyond the vendor prefixes each adapter already lets
	// through. Env sets a value in the config file; this names one that stays
	// out of it, which is what a credential wants.
	EnvPassthrough []string `koanf:"env_passthrough"`
	// Sandbox is codex-specific and ignored by other adapters.
	Sandbox string `koanf:"sandbox"`
	// Mode is cursor-specific and ignored by other adapters: it picks the
	// CLI's execution mode, one of [CursorModes].
	Mode string `koanf:"mode"`
	// SystemPromptMode is claude-code-specific: "replace" (the default) sends
	// only the caller's system message, so the backend behaves like a bare LLM;
	// "append" keeps the CLI's own agent system prompt and adds the caller's to
	// it. See SystemPrompt* above.
	SystemPromptMode string `koanf:"system_prompt_mode"`
}

// AdapterIDs returns the configured adapter ids in a stable order. Being listed
// is not the same as being served: an adapter joins the router only if probing
// finds its CLI usable on this machine.
func (c Config) AdapterIDs() []string {
	ids := make([]string, 0, len(c.Adapters))
	for id := range c.Adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Source records which layer supplied a key's effective value.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceFlag    Source = "flag"
)

// Provenance answers "where did this value come from?" for `config print`.
type Provenance struct {
	// File is the config file that was loaded, empty if none was found.
	File string
	// Of maps a flattened config path to the layer that set it.
	Of map[string]Source
	// Values holds the effective value of every flattened config path.
	Values map[string]any
}

func (p *Provenance) source(key string) Source {
	if s, ok := p.Of[key]; ok {
		return s
	}
	return SourceDefault
}

// Report renders the effective configuration one key per line, annotated with
// the layer each value came from.
//
// The output is meant for bug reports, so anything that can hold a credential —
// the bearer key, adapter env values, extra_args values — is shown as set
// rather than shown. Names stay: whether a key is set, and from which layer, is
// what the report is read for.
func (p *Provenance) Report() string {
	keys := make([]string, 0, len(p.Values))
	width := 0
	for key := range p.Values {
		keys = append(keys, key)
		if len(key) > width {
			width = len(key)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	if p.File != "" {
		fmt.Fprintf(&b, "# config file: %s\n", p.File)
	} else {
		b.WriteString("# config file: none (built-in defaults and flags only)\n")
	}
	for _, key := range keys {
		fmt.Fprintf(&b, "%-*s = %-24s (%s)\n", width, key, formatValue(key, p.Values[key]), p.source(key))
	}
	return b.String()
}

func formatValue(key string, v any) string {
	if secondsKeys[key] {
		// A bare number is ambiguous to read, so show what it means.
		if n, ok := asSeconds(v); ok {
			return n.String()
		}
	}
	if isSecretKey(key) {
		return redactedValue(v)
	}
	if isExtraArgsKey(key) {
		return formatExtraArgs(v)
	}
	return formatAny(v)
}

// formatAny renders one value for the report. It goes through reflection rather
// than a list of concrete types because the values arrive typed from the
// defaults struct (map[string]string, []string, *bool) but untyped from a YAML
// file (map[string]any, []any) — the same key can be either, depending on which
// layer won, and both have to read the same on the page.
func formatAny(v any) string {
	if v == nil {
		return `""`
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return `""`
		}
		return formatAny(rv.Elem().Interface())

	case reflect.String:
		if rv.Len() == 0 {
			return `""`
		}
		return rv.String()

	case reflect.Slice, reflect.Array:
		parts := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			parts = append(parts, fmt.Sprint(rv.Index(i).Interface()))
		}
		return "[" + strings.Join(parts, ", ") + "]"

	case reflect.Map:
		if rv.Len() == 0 {
			return "{}"
		}
		entries := make([]string, 0, rv.Len())
		for _, key := range rv.MapKeys() {
			entries = append(entries, fmt.Sprint(key.Interface())+"="+fmt.Sprint(rv.MapIndex(key).Interface()))
		}
		sort.Strings(entries)
		return "{" + strings.Join(entries, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// isSecretKey reports whether a config path's value can carry a credential.
//
// Adapter environment variables are one flattened key each
// (adapters.<id>.env.<NAME>), so the variable name is already on the page; only
// what it is set to has to be withheld.
func isSecretKey(key string) bool {
	if key == "server.api_key" {
		return true
	}
	return strings.HasPrefix(key, "adapters.") && strings.Contains(key, ".env.")
}

// redactedValue shows that a value is set without showing it. An unset one is
// rendered plainly: "not configured" is not a secret, and hiding it would make
// the report useless for the question it is usually read to answer.
func redactedValue(v any) string {
	switch val := v.(type) {
	case nil:
		return `""`
	case string:
		if val == "" {
			return `""`
		}
	}
	return "[redacted]"
}

// isExtraArgsKey matches adapters.<id>.extra_args.
func isExtraArgsKey(key string) bool {
	return strings.HasPrefix(key, "adapters.") && strings.HasSuffix(key, ".extra_args")
}

// formatExtraArgs shows which flags an adapter was given while withholding what
// they were set to. A token that starts with a dash is a flag name, which is not
// a secret; anything else is a value, which can be — as can the right-hand side
// of a --flag=value token.
func formatExtraArgs(v any) string {
	rv := reflect.ValueOf(v)
	if v == nil || rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return redactedValue(v)
	}
	if rv.Len() == 0 {
		return "[]"
	}
	args := make([]string, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		args = append(args, fmt.Sprint(rv.Index(i).Interface()))
	}

	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case !strings.HasPrefix(arg, "-"):
			out = append(out, "[redacted]")
		case strings.Contains(arg, "="):
			name, _, _ := strings.Cut(arg, "=")
			out = append(out, name+"=[redacted]")
		default:
			out = append(out, arg)
		}
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// asSeconds interprets a raw merged value as a count of seconds, for display.
// It reports false for anything unparseable, which Validate will have rejected
// already — `config print` still has to render something.
func asSeconds(v any) (Seconds, bool) {
	switch n := v.(type) {
	case Seconds:
		return n, true
	case int:
		return Seconds(n), true
	case int64:
		return Seconds(n), true
	case float64:
		return Seconds(int64(n)), true
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return Seconds(parsed), true
		}
	}
	return 0, false
}

// Load assembles the effective configuration: built-in defaults, then the file
// named by -c/--config if there is one, then any flag the user actually set.
//
// The file is decoded strictly: an unknown key is a startup error, so typos
// fail loudly instead of being silently ignored.
func Load(flags *pflag.FlagSet) (*Config, *Provenance, error) {
	path, err := resolveConfigPath(flags)
	if err != nil {
		return nil, nil, err
	}

	var fileMap map[string]any
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read config file %s: %w", path, err)
		}
		fileMap, err = parseYAML(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot parse config file %s: %w", path, err)
		}
		fileMap = stripNil(fileMap)
	}

	flagMap, err := flagOverrides(flags)
	if err != nil {
		return nil, nil, err
	}

	k := koanf.New(Delim)
	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return nil, nil, fmt.Errorf("built-in defaults are invalid: %w", err)
	}
	if fileMap != nil {
		if err := k.Load(confmap.Provider(fileMap, Delim), nil); err != nil {
			return nil, nil, fmt.Errorf("cannot load config file %s: %w", path, err)
		}
	}
	if err := k.Load(confmap.Provider(flagMap, Delim), nil); err != nil {
		return nil, nil, err
	}

	var cfg Config
	if err := unmarshalStrict(k, &cfg); err != nil {
		return nil, nil, fmt.Errorf("invalid configuration: %w", err)
	}

	prov := &Provenance{File: path, Of: map[string]Source{}, Values: k.All()}
	for key := range flatten(fileMap) {
		prov.Of[key] = SourceFile
	}
	for key := range flagMap {
		prov.Of[key] = SourceFlag
	}

	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return &cfg, prov, nil
}

// unmarshalStrict decodes the merged map into cfg, rejecting keys the schema
// does not define.
func unmarshalStrict(k *koanf.Koanf, cfg *Config) error {
	return k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           cfg,
			TagName:          "koanf",
			ErrorUnused:      true,
			WeaklyTypedInput: true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToSliceHookFunc(","),
			),
		},
	})
}

// Validate checks the invariants the rest of the process relies on.
func (c *Config) Validate() error {
	var errs []error

	for id := range c.Adapters {
		if !known(id) {
			errs = append(errs, fmt.Errorf("adapters.%s: unknown adapter; valid ids are %s",
				id, strings.Join(KnownAdapters, ", ")))
		}
	}
	for _, id := range c.AdapterIDs() {
		a := c.Adapters[id]
		if a.Binary == "" {
			errs = append(errs, fmt.Errorf("adapters.%s.binary: must not be empty", id))
		}
		if a.Sandbox != "" && !contains(SandboxModes, a.Sandbox) {
			errs = append(errs, fmt.Errorf("adapters.%s.sandbox: must be one of %s, got %q",
				id, strings.Join(SandboxModes, ", "), a.Sandbox))
		}
		if a.Mode != "" && !contains(CursorModes, a.Mode) {
			errs = append(errs, fmt.Errorf("adapters.%s.mode: must be one of %s, got %q",
				id, strings.Join(CursorModes, ", "), a.Mode))
		}
		for _, name := range a.EnvPassthrough {
			if !validEnvName(name) {
				errs = append(errs, fmt.Errorf("adapters.%s.env_passthrough: %q is not an environment variable name", id, name))
				continue
			}
			if _, set := a.Env[name]; set {
				errs = append(errs, fmt.Errorf("adapters.%s: %s is in both env and env_passthrough; keep it in one, so which value the CLI gets is not a question",
					id, name))
			}
		}
		switch a.SystemPromptMode {
		case "", SystemPromptAppend, SystemPromptReplace:
		default:
			errs = append(errs, fmt.Errorf("adapters.%s.system_prompt_mode: must be %q or %q, got %q",
				id, SystemPromptAppend, SystemPromptReplace, a.SystemPromptMode))
		}
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: must be debug, info, warn or error, got %q", c.Log.Level))
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format: must be text or json, got %q", c.Log.Format))
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port: must be between 1 and 65535, got %d", c.Server.Port))
	}
	if !isLoopback(c.Server.Host) && c.Server.APIKey == "" {
		errs = append(errs, fmt.Errorf(
			"server.host: binding to %s exposes the gateway beyond this machine, so a bearer key is required; set --api-key or bind to 127.0.0.1",
			hostForMessage(c.Server.Host)))
	}
	if c.Server.MaxConcurrency < 1 {
		errs = append(errs, fmt.Errorf("server.max_concurrency: must be at least 1, got %d", c.Server.MaxConcurrency))
	}
	for _, d := range []struct {
		key string
		val Seconds
	}{
		{"server.request_timeout", c.Server.RequestTimeout},
		{"server.idle_timeout", c.Server.IdleTimeout},
		{"server.queue_timeout", c.Server.QueueTimeout},
		{"server.heartbeat_interval", c.Server.HeartbeatInterval},
		{"server.shutdown_timeout", c.Server.ShutdownTimeout},
	} {
		if d.val < 0 {
			errs = append(errs, fmt.Errorf("%s: must not be negative", d.key))
		}
	}
	return errors.Join(errs...)
}

// isLoopback reports whether host keeps the listener on this machine.
//
// An empty host does not. net.Listen reads "" as every interface, so it is the
// widest bind there is, not the narrowest — and the one most likely to be an
// accident, since a key left blank looks like a key left unset.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostForMessage names a bind address in an error. An empty one is spelled out
// rather than printed, since printing nothing would hide what went wrong.
func hostForMessage(host string) string {
	if host == "" {
		return "an empty host (every interface)"
	}
	return host
}

// resolveConfigPath returns the file named by -c/--config, or "" when the flag
// was not given. There are no default locations: configuration comes from the
// built-in defaults, the flags, and a file the user points at explicitly.
// A path that does not exist is an error: the user named a file, so silently
// running on defaults instead would be the wrong kind of helpful.
func resolveConfigPath(flags *pflag.FlagSet) (string, error) {
	if flags == nil {
		return "", nil
	}
	f := flags.Lookup(flagConfig)
	if f == nil || !f.Changed {
		return "", nil
	}
	path := f.Value.String()
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("config file %s: %w", path, err)
	}
	return path, nil
}

// stripNil drops the keys a config file wrote without a value.
//
// A key with nothing after the colon parses as nil, and nil is a value: merged
// over the defaults it replaces them, so `host:` would blank the listen address
// and a bare `server:` would zero the section. Unwritten is what it means.
//
// A map the file wrote as empty is kept, since `env: {}` says something; one
// left empty by stripping goes with its children.
func stripNil(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for key, val := range m {
		switch v := val.(type) {
		case nil:
			continue
		case map[string]any:
			sub := stripNil(v)
			if len(v) > 0 && len(sub) == 0 {
				continue
			}
			out[key] = sub
		default:
			out[key] = val
		}
	}
	return out
}

// parseYAML decodes a config document into koanf's nested map form.
func parseYAML(raw []byte) (map[string]any, error) {
	return yaml.Parser().Unmarshal(raw)
}

// flatten turns a nested config map into dotted paths, matching koanf's own key
// space, so provenance can be recorded per leaf.
func flatten(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, v map[string]any)
	walk = func(prefix string, v map[string]any) {
		for key, val := range v {
			path := key
			if prefix != "" {
				path = prefix + Delim + key
			}
			if sub, ok := val.(map[string]any); ok && len(sub) > 0 {
				walk(path, sub)
				continue
			}
			out[path] = val
		}
	}
	walk("", m)
	return out
}

func known(id string) bool { return contains(KnownAdapters, id) }

// validEnvName accepts what a shell can export: a letter or underscore, then
// letters, digits and underscores. Anything else could not have been set in
// the gateway's environment to begin with, so it is a typo worth naming.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
