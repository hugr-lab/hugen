package config

import (
	"fmt"
	"os"
	"reflect"

	"github.com/go-viper/mapstructure/v2"
)

// LoadStaticInput parses a config map (typically the agent.Config
// blob from pkg/identity) into a StaticInput ready to hand to
// NewStaticService. Caller passes localDBEnabled separately
// because it is bootstrap-mode driven, not YAML-driven.
//
// Every string field of the parsed input is then run through
// os.ExpandEnv, so `${VAR}` placeholders anywhere in the config
// (model paths, auth tokens, MCP endpoints, headers, env values,
// etc.) resolve uniformly. Bootstrap has already promoted .env
// keys into os.Environ.
//
// We use mapstructure directly (instead of viper) because viper
// lowercases every key during MergeConfigMap, which silently
// breaks case-sensitive maps such as `env:` (sub-process
// environment variables) and `headers:` (HTTP request headers).
func LoadStaticInput(raw map[string]any, localDBEnabled bool) (StaticInput, error) {
	in := StaticInput{LocalDBEnabled: localDBEnabled}

	if err := decodeKey(raw, "models", &in.Models); err != nil {
		return in, err
	}
	if err := decodeKey(raw, "embedding", &in.Embedding); err != nil {
		return in, err
	}
	if err := decodeKey(raw, "local_db", &in.LocalDB); err != nil {
		return in, err
	}
	if err := decodeKey(raw, "auth", &in.Auth); err != nil {
		return in, err
	}
	if err := decodeKey(raw, "permissions", &in.Permissions); err != nil {
		return in, err
	}
	if err := decodeKey(raw, "permission_settings", &in.PermSettings); err != nil {
		return in, err
	}
	if err := decodeKey(raw, "tool_providers", &in.ToolProviders); err != nil {
		return in, err
	}

	expandEnvInPlace(&in)
	return in, nil
}

// decodeKey extracts raw[key] (when present) and decodes it into
// dest with mapstructure. ErrorUnused/ErrorUnset stay off so an
// unknown YAML key doesn't fail the boot — the rest of the config
// keeps working.
func decodeKey(raw map[string]any, key string, dest any) error {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil
	}
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           dest,
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
		),
	})
	if err != nil {
		return fmt.Errorf("config: decoder for %q: %w", key, err)
	}
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("config: decode %q: %w", key, err)
	}
	return nil
}

// expandEnvInPlace recursively walks every string field of v (a
// pointer to a struct / slice / map) and rewrites it through
// os.ExpandEnv. Numeric, json.RawMessage, and time.Duration fields
// are left alone — only strings carry `${VAR}` placeholders.
//
// Driven by reflect so the loader stays correct as new config
// fields land: any string introduced anywhere in StaticInput is
// expanded automatically.
func expandEnvInPlace(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	walkEnv(rv.Elem())
}

func walkEnv(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if !v.CanSet() {
			return
		}
		s := v.String()
		if expanded := os.ExpandEnv(s); expanded != s {
			v.SetString(expanded)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			walkEnv(v.Field(i))
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkEnv(v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkEnv(v.Index(i))
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			val := iter.Value()
			switch val.Kind() {
			case reflect.String:
				expanded := os.ExpandEnv(val.String())
				if expanded != val.String() {
					v.SetMapIndex(iter.Key(), reflect.ValueOf(expanded))
				}
			default:
				if val.CanInterface() {
					tmp := reflect.New(val.Type()).Elem()
					tmp.Set(val)
					walkEnv(tmp)
					v.SetMapIndex(iter.Key(), tmp)
				}
			}
		}
	}
}
