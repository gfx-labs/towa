package config

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/goccy/go-yaml"
)

// EnvFiles names the environment variable that lists extra config files,
// separated like PATH.
const EnvFiles = "TOWA_CONFIG"

// Flags are the flags that set the configuration. A command line embeds one
// beside its commands, and Parse resolves it.
type Flags struct {
	// Files are read in order after the files of Sources. Each must exist.
	Files []string `name:"config" sep:"none" placeholder:"FILE" help:"Read this config file after the system, user and TOWA_CONFIG files (repeatable)."`
	// The prefix starts every environment variable that sets a key, and
	// marks a flag as a key. kong does not read the environment itself: the
	// environment is Sources.Environ, which a test supplies.
	// TOWA_TELEMETRY__LOGS__STDERR__LEVEL sets
	// telemetry.logs.stderr.level: the rest of the name is lowercased and
	// "__" separates the levels.
	Config Config `embed:"" envprefix:"TOWA_"`
}

// Sources says where each layer comes from. A later layer overrides an
// earlier one key by key: the defaults, the system file, the user file,
// Files, the files of --config, the environment, then the flags. A table is
// one key, so the last layer that sets it gives all of it.
type Sources struct {
	// SystemDir and UserDir may each hold one config.yaml, config.yml or
	// config.json. An empty name or a directory without a config file
	// contributes nothing.
	SystemDir string
	UserDir   string
	// Files are read in order. Each must exist.
	Files []string
	// Environ is the environment, as os.Environ returns it.
	Environ []string
	// StoredAccounts returns the accounts of the auth store: the provider of
	// each, by id. They are not a layer. Each joins Config.Accounts as an
	// oauth account, unless a layer declares an account of that id. So does
	// an account that a model chain names on a built-in provider. It is
	// called once the layers are resolved, and nil means there is no store.
	StoredAccounts func() (map[string]string, error)
}

// Loaded is a resolved configuration and where its values came from.
type Loaded struct {
	Config Config
	// Values holds display-only JSON with credentials redacted. A duration
	// is text such as "30m0s". Config keeps the original runtime values.
	Values map[string]string
	// Origin names the layer that supplied each key's value: "default", a
	// file, "environment" or "flag".
	Origin map[string]string
}

// Parse parses args with grammar, a struct that embeds *flags beside its
// commands, and resolves the configuration from src and the flags. A key
// that Config does not have is an error, whichever layer it came from.
// options are kong's own, such as the program's name.
func Parse(src Sources, grammar any, flags *Flags, args []string, options ...kong.Option) (*kong.Context, *Loaded, error) {
	l := &layers{src: src, listed: &flags.Files, env: map[string]variable{}, origin: map[string]string{}}
	parser, err := kong.New(grammar, slices.Concat(options, []kong.Option{
		kong.NamedMapper("table", kong.MapperFunc(decodeTable)),
		kong.WithBeforeResolve(l.load),
	})...)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	ctx, err := parser.Parse(args)
	if err != nil {
		return nil, nil, err
	}

	loaded := &Loaded{Config: flags.Config, Values: map[string]string{}, Origin: l.origin}
	for _, element := range ctx.Path {
		if element.Flag != nil && !element.Resolved && l.keys[element.Flag.Name] == element.Flag {
			loaded.Origin[element.Flag.Name] = "flag"
		}
	}
	for key := range l.keys {
		if _, set := loaded.Origin[key]; !set {
			loaded.Origin[key] = "default"
		}
	}
	var stored map[string]string
	if src.StoredAccounts != nil {
		if stored, err = src.StoredAccounts(); err != nil {
			return nil, nil, err
		}
	}
	loaded.Config.addStoredAccounts(stored)
	if err := loaded.Config.validate(); err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	// After validation, which refuses a number JSON cannot write.
	for key, flag := range l.keys {
		text, err := displayValue(key, flag.Target.Interface())
		if err != nil {
			return nil, nil, fmt.Errorf("config: %s: %w", key, err)
		}
		loaded.Values[key] = string(text)
	}
	return ctx, loaded, nil
}

// valueOptions write a value for Loaded.Values: the members of a table in
// name order, and a duration as the text a file gives it.
var valueOptions = json.JoinOptions(
	json.Deterministic(true),
	json.WithMarshalers(json.MarshalFunc(func(d time.Duration) ([]byte, error) {
		return json.Marshal(d.String())
	})),
)

// layers holds the files and the environment of one parse. As a
// kong.Resolver it gives kong the value they set for a key no flag set.
type layers struct {
	src Sources
	// listed is the target of the flag that names more files.
	listed *[]string
	// keys are the configuration's flags by name. sections are the names
	// above them: telemetry and telemetry.logs for telemetry.logs.otel.level.
	keys     map[string]*kong.Flag
	sections map[string]bool
	// files are in the order they were read.
	files  []file
	env    map[string]variable
	origin map[string]string
}

type file struct {
	path   string
	values map[string]any
}

type variable struct{ name, value string }

// load reads every file and the environment, and reports a name in them
// that is not a key. It is kong's BeforeResolve hook: the command line is
// parsed by then, so the files of --config are known, and no key has a
// value yet. kong calls the hook once for each element of the command line,
// and the first call does the work.
func (l *layers) load(ctx *kong.Context) error {
	if l.keys != nil {
		return nil
	}
	l.keys, l.sections = map[string]*kong.Flag{}, map[string]bool{}
	var listed []string
	prefix := ""
	for _, flag := range ctx.Model.Flags {
		if flag.Target.CanAddr() && flag.Target.Addr().Interface() == any(l.listed) {
			listed, _ = ctx.FlagValue(flag).([]string)
		}
		if flag.Tag.EnvPrefix == "" {
			continue
		}
		prefix = flag.Tag.EnvPrefix
		// A field without a name tag gets a dashed flag name from kong,
		// which is not the key a file writes.
		if strings.Contains(flag.Name, "-") || flag.Help == "" || flag.Hidden {
			return fmt.Errorf("config: key %s needs a snake_case name tag and help text that is shown", flag.Name)
		}
		l.keys[flag.Name] = flag
		for i, r := range flag.Name {
			if r == '.' {
				l.sections[flag.Name[:i]] = true
			}
		}
	}

	var paths []string
	for _, dir := range []string{l.src.SystemDir, l.src.UserDir} {
		if dir == "" {
			continue
		}
		path, err := findIn(dir)
		if err != nil {
			return err
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	for _, path := range slices.Concat(paths, l.src.Files, listed) {
		values, err := readFile(path)
		if err != nil {
			return err
		}
		if err := l.check(path, "", values); err != nil {
			return err
		}
		l.files = append(l.files, file{path, values})
	}
	// The last assignment of a name wins, as it does for a process.
	for _, kv := range l.src.Environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == EnvFiles || !strings.HasPrefix(name, prefix) {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, prefix), "__", "."))
		if _, known := l.keys[key]; !known {
			return fmt.Errorf("config: environment variable %s sets %s, which is not a config key", name, key)
		}
		l.env[key] = variable{name, value}
	}
	ctx.AddResolver(l)
	return nil
}

// check reports a name under prefix in a file's values that is not a key or
// a section.
func (l *layers) check(path, prefix string, values map[string]any) error {
	// In name order, so that the same file always reports the same error.
	for _, name := range slices.Sorted(maps.Keys(values)) {
		key := prefix + name
		// A file nests its sections. A name with a dot in it would match a
		// key here and then never be looked up.
		if strings.Contains(name, ".") {
			return fmt.Errorf("config: %s: %s is written with a dot: nest each name under the one before it", path, key)
		}
		if _, known := l.keys[key]; known {
			continue
		}
		if !l.sections[key] {
			return fmt.Errorf("config: %s: %s is not a config key", path, key)
		}
		switch under := values[name].(type) {
		case nil:
		case map[string]any:
			if err := l.check(path, key+".", under); err != nil {
				return err
			}
		default:
			return fmt.Errorf("config: %s: %s is a section, and the file gives it one value", path, key)
		}
	}
	return nil
}

// Validate is part of kong.Resolver. load has checked every name.
func (l *layers) Validate(*kong.Application) error { return nil }

// Resolve returns what the environment sets for the flag's key, or else
// what the last file that names the key sets. kong asks only for a key that
// no flag set. A null in a file sets nothing.
func (l *layers) Resolve(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
	if l.keys[flag.Name] != flag {
		return nil, nil
	}
	var value any
	origin, in := "", ""
	if v, set := l.env[flag.Name]; set {
		value, origin, in = v.value, "environment", v.name
	}
	for i := 0; i < len(l.files) && in == ""; i++ {
		raw := lookup(l.files[i].values, flag.Name)
		if raw == nil {
			continue
		}
		over, err := fileValue(flag, raw)
		if err != nil {
			return nil, fmt.Errorf("in %s: %w", l.files[i].path, err)
		}
		value, origin = over, l.files[i].path
	}
	if origin == "" {
		return nil, nil
	}
	if in == "" {
		in = origin
	}
	// kong decodes the value after this returns, and its error would not
	// say where the value was read. Decoding it here first does.
	scratch := reflect.New(flag.Target.Type()).Elem()
	scan := kong.Scan().PushTyped(value, kong.FlagValueToken)
	if err := flag.Mapper.Decode(&kong.DecodeContext{Value: flag.Value, Scan: scan}, scratch); err != nil {
		return nil, fmt.Errorf("in %s: %w", in, err)
	}
	l.origin[flag.Name] = origin
	return value, nil
}

// lookup returns the value at a dotted key in a file's values, or nil.
func lookup(values map[string]any, key string) any {
	var value any = values
	for name := range strings.SplitSeq(key, ".") {
		section, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = section[name]
	}
	return value
}

var durationType = reflect.TypeFor[time.Duration]()

// fileValue returns a file's value for a key as kong decodes it: text, as a
// flag's value is, a list of text, or a table as it was parsed. A value
// that is not text must have the key's own type. YAML reads 0600 as the
// number 384, so a number is never taken for a key that holds text, and a
// duration is never taken from a number of nanoseconds.
func fileValue(flag *kong.Flag, value any) (any, error) {
	if flag.Tag.Type != "" {
		return value, nil
	}
	kind := flag.Target.Kind()
	float := kind == reflect.Float32 || kind == reflect.Float64
	number := float || (kind >= reflect.Int && kind <= reflect.Uint64 && flag.Target.Type() != durationType)
	wrong := errors.New("the value does not have the key's type")
	if kind == reflect.String || flag.Target.Type() == durationType {
		wrong = errors.New("the key holds text, and the value is not text: put it in quotes")
	}
	switch v := value.(type) {
	case string:
		// A flag splits its text at commas. A file writes a list.
		if kind == reflect.Slice {
			return nil, errors.New("the key holds a list, and the value is one text")
		}
		return v, nil
	case bool:
		if kind == reflect.Bool {
			return strconv.FormatBool(v), nil
		}
	case int64:
		if number {
			return strconv.FormatInt(v, 10), nil
		}
	case uint64:
		if number {
			return strconv.FormatUint(v, 10), nil
		}
	case float64:
		if float {
			return strconv.FormatFloat(v, 'g', -1, 64), nil
		}
	case []any:
		if kind != reflect.Slice {
			break
		}
		for i, item := range v {
			if _, text := item.(string); !text {
				return nil, fmt.Errorf("item %d of the list is not text: put it in quotes", i)
			}
		}
		return v, nil
	}
	return nil, wrong
}

var errDuration = errors.New("is not a duration such as 30s")

// tableOptions decode a table strictly: a member the struct does not have is
// an error, and so is a number where text belongs. A duration is text, as
// it is for a flag.
var tableOptions = json.JoinOptions(
	json.RejectUnknownMembers(true),
	json.WithUnmarshalers(json.UnmarshalFunc(func(data []byte, d *time.Duration) error {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return errDuration
		}
		*d = parsed
		return nil
	})),
)

// decodeTable is kong's mapper for a key of type "table". A file gives the
// table as it was parsed. A flag or an environment variable gives it as
// YAML text. The errors hold no value: a table can hold a credential.
func decodeTable(ctx *kong.DecodeContext, target reflect.Value) error {
	token, err := ctx.Scan.PopValue("YAML")
	if err != nil {
		return err
	}
	value := token.Value
	if text, ok := value.(string); ok {
		value = nil
		if err := yaml.Unmarshal([]byte(text), &value); err != nil {
			return errors.New(yaml.FormatError(err, false, false))
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	table := reflect.New(target.Type())
	err = json.Unmarshal(data, table.Interface(), tableOptions)
	var semantic *json.SemanticError
	if errors.As(err, &semantic) {
		key := ctx.Value.Name
		for name := range semantic.JSONPointer.Tokens() {
			if _, err := strconv.ParseUint(name, 10, 64); err == nil {
				key += "[" + name + "]"
			} else {
				key += "." + name
			}
		}
		switch {
		case errors.Is(err, json.ErrUnknownName):
			return fmt.Errorf("%s is not a config key", key)
		case errors.Is(err, errDuration):
			return fmt.Errorf("%s %w", key, errDuration)
		}
		return fmt.Errorf("%s does not have the key's type", key)
	}
	if err != nil {
		return err
	}
	target.Set(table.Elem())
	return nil
}

// readFile parses a YAML file. JSON is YAML, so a .json file reads the same.
func readFile(path string) (map[string]any, error) {
	switch ext := filepath.Ext(path); ext {
	case ".yaml", ".yml", ".json":
	default:
		return nil, fmt.Errorf("config: %s: extension %q is not .yaml, .yml or .json", path, ext)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// The source is left out of the error: it can hold a credential.
	var values map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&values); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %s: %s", path, yaml.FormatError(err, false, false))
	}
	var second any
	switch err := decoder.Decode(&second); {
	case err == nil:
		return nil, fmt.Errorf("config: %s: holds more than one YAML document", path)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("config: %s: %s", path, yaml.FormatError(err, false, false))
	}
	return values, nil
}

// findIn returns the one config file in dir, or "" when there is none.
func findIn(dir string) (string, error) {
	var found []string
	for _, name := range []string{"config.yaml", "config.yml", "config.json"} {
		path := filepath.Join(dir, name)
		switch _, err := os.Stat(path); {
		case err == nil:
			found = append(found, path)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return "", fmt.Errorf("config: %w", err)
		}
	}
	switch {
	case len(found) == 0:
		return "", nil
	case len(found) > 1:
		return "", fmt.Errorf("config: %s holds more than one config file: %s", dir, strings.Join(found, ", "))
	}
	return found[0], nil
}
