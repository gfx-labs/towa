package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvFiles names the environment variable that lists extra config files,
// separated like PATH.
const EnvFiles = "ACPHARNESS_CONFIG"

// envPrefix starts every environment variable that sets a config key.
// ACPHARNESS_TELEMETRY__LOGS__STDERR__LEVEL sets telemetry.logs.stderr.level:
// the rest of the name is lowercased and "__" separates the levels.
const envPrefix = "ACPHARNESS_"

// delim separates the levels of a config key.
const delim = "."

// Sources says where each layer comes from. Layers are applied in the order
// of the fields, and a later layer overrides an earlier one key by key.
type Sources struct {
	// SystemDir and UserDir may each hold one config.toml, config.yaml,
	// config.yml or config.json. An empty name or a directory without a
	// config file contributes nothing.
	SystemDir string
	UserDir   string
	// Files are read in order. Each must exist.
	Files []string
	// Environ is the environment, as os.Environ returns it.
	Environ []string
	// Flags holds the config keys set by command-line flags that were given.
	Flags map[string]any
	// Set holds "key=value" assignments. The value is text, converted to the
	// key's type, so a key that holds a list or a table cannot be set this way.
	Set []string
}

// Loaded is a resolved configuration and where its values came from.
type Loaded struct {
	Config Config
	// Values holds every key with its merged value, before decoding.
	Values map[string]any
	// Origin names the layer that supplied each key's value.
	Origin map[string]string
}

// Load merges the layers and decodes the result. A key that Config does not
// have is an error, whichever layer it came from.
func Load(src Sources) (*Loaded, error) {
	l := &loader{k: koanf.New(delim), origin: map[string]string{}}
	if err := l.add("default", confmap.Provider(defaults(), delim), nil); err != nil {
		return nil, err
	}
	for _, dir := range []string{src.SystemDir, src.UserDir} {
		if dir == "" {
			continue
		}
		path, err := findIn(dir)
		if err != nil {
			return nil, err
		}
		if path == "" {
			continue
		}
		if err := l.addFile(path); err != nil {
			return nil, err
		}
	}
	for _, path := range src.Files {
		if err := l.addFile(path); err != nil {
			return nil, err
		}
	}
	environ := env.Provider(delim, env.Opt{
		Prefix:        envPrefix,
		TransformFunc: envKey,
		EnvironFunc:   func() []string { return src.Environ },
	})
	if err := l.add("environment", environ, nil); err != nil {
		return nil, err
	}
	if err := l.add("flag", confmap.Provider(src.Flags, delim), nil); err != nil {
		return nil, err
	}
	set := map[string]any{}
	for _, kv := range src.Set {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("config: --set %q is not key=value", kv)
		}
		set[key] = value
	}
	if err := l.add("--set", confmap.Provider(set, delim), nil); err != nil {
		return nil, err
	}

	out := &Loaded{Values: l.k.All(), Origin: l.origin}
	decoder := &mapstructure.DecoderConfig{
		// Strings from the environment and --set become the field's type.
		// There is no weak typing beyond that: a number is not a list.
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.TextUnmarshallerHookFunc(),
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToBasicTypeHookFunc(),
		),
		ErrorUnused: true,
	}
	if err := l.k.UnmarshalWithConf("", &out.Config, koanf.UnmarshalConf{Tag: "koanf", DecoderConfig: decoder}); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := out.Config.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return out, nil
}

type loader struct {
	k      *koanf.Koanf
	origin map[string]string
}

// add merges one layer and records it as the origin of the keys it sets.
func (l *loader) add(name string, p koanf.Provider, pa koanf.Parser) error {
	layer := koanf.New(delim)
	if err := layer.Load(p, pa); err != nil {
		return fmt.Errorf("config: %s: %w", name, err)
	}
	for _, key := range layer.Keys() {
		l.origin[key] = name
	}
	if err := l.k.Merge(layer); err != nil {
		return fmt.Errorf("config: %s: %w", name, err)
	}
	return nil
}

func (l *loader) addFile(path string) error {
	var pa koanf.Parser
	switch ext := filepath.Ext(path); ext {
	case ".toml":
		pa = toml.Parser()
	case ".yaml", ".yml":
		pa = yaml.Parser()
	case ".json":
		pa = json.Parser()
	default:
		return fmt.Errorf("config: %s: extension %q is not .toml, .yaml, .yml or .json", path, ext)
	}
	return l.add(path, file.Provider(path), pa)
}

// findIn returns the one config file in dir, or "" when there is none.
func findIn(dir string) (string, error) {
	var found []string
	for _, name := range []string{"config.toml", "config.yaml", "config.yml", "config.json"} {
		path := filepath.Join(dir, name)
		switch _, err := os.Stat(path); {
		case err == nil:
			found = append(found, path)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return "", fmt.Errorf("config: %w", err)
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("config: %s holds more than one config file: %s", dir, strings.Join(found, ", "))
	}
}

// envKey turns an environment variable name into a config key. EnvFiles
// shares the prefix and is not a key.
func envKey(name, value string) (string, any) {
	if name == EnvFiles {
		return "", nil
	}
	key := strings.ToLower(strings.TrimPrefix(name, envPrefix))
	return strings.ReplaceAll(key, "__", delim), value
}
