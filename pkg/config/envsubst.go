package config

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// ${VAR} substitution in config values.
//
//   ${VAR}            value of VAR
//   ${VAR:-default}   value of VAR, or default when VAR is unset or empty
//   $${               a literal "${"
//
// A reference to an unset variable without a default is left as written and
// reported, so existing values that happen to contain shell-style ${...}
// (hook commands, MCP args) keep working.
//
// Plain string settings are substituted by substituteEnvRefs at load time.
// Secrets (SecureString) substitute themselves when decoded and keep the
// reference as their persisted value, so .security.yml keeps "${VAR}".

var envRefPattern = regexp.MustCompile(`\$\$\{|\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// hasEnvRef reports whether s may contain a ${...} reference or a $${ escape.
func hasEnvRef(s string) bool {
	return strings.Contains(s, "${")
}

// expandEnvRefs substitutes ${VAR} references in s. It returns the expanded
// string and the names of referenced variables that were unset with no default.
func expandEnvRefs(s string) (string, []string) {
	if !hasEnvRef(s) {
		return s, nil
	}
	var missing []string
	out := envRefPattern.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$${" {
			return "${"
		}
		sub := envRefPattern.FindStringSubmatch(m)
		name, hasDefault := sub[1], strings.Contains(m, ":-")
		if v, ok := os.LookupEnv(name); ok && (v != "" || !hasDefault) {
			return v
		}
		if hasDefault {
			return sub[2]
		}
		missing = append(missing, name)
		return m
	})
	return out, missing
}

var channelType = reflect.TypeOf(Channel{})

// substituteEnvRefs expands ${VAR} references in every exported string
// setting of cfg, including decoded channel settings, and returns the sorted,
// de-duplicated names of unset variables that had no default.
func substituteEnvRefs(cfg *Config) []string {
	missing := map[string]struct{}{}
	substituteValue(reflect.ValueOf(cfg).Elem(), missing)
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func substituteValue(v reflect.Value, missing map[string]struct{}) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() && hasEnvRef(v.String()) {
			expanded, names := expandEnvRefs(v.String())
			for _, n := range names {
				missing[n] = struct{}{}
			}
			v.SetString(expanded)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			substituteValue(v.Elem(), missing)
		}
	case reflect.Interface:
		if v.IsNil() || !v.CanSet() {
			return
		}
		elem := v.Elem()
		if elem.Kind() == reflect.Pointer {
			substituteValue(elem, missing)
			return
		}
		cp := reflect.New(elem.Type()).Elem()
		cp.Set(elem)
		substituteValue(cp, missing)
		v.Set(cp)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				substituteValue(v.Field(i), missing)
			}
		}
		// Channel settings live behind an unexported, decoded pointer.
		if t == channelType && v.CanAddr() {
			if decoded, err := v.Addr().Interface().(*Channel).GetDecoded(); err == nil && decoded != nil {
				substituteValue(reflect.ValueOf(decoded), missing)
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // raw bytes (e.g. undecoded JSON), not settings
		}
		for i := 0; i < v.Len(); i++ {
			substituteValue(v.Index(i), missing)
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		iter := v.MapRange()
		for iter.Next() {
			val := iter.Value()
			cp := reflect.New(val.Type()).Elem()
			cp.Set(val)
			substituteValue(cp, missing)
			v.SetMapIndex(iter.Key(), cp)
		}
	}
}
