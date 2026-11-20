package logging

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Redacted replaces a secret value in logs.
const Redacted = "[REDACTED]"

// IsSensitiveKey reports whether a field or JSON key name holds a secret: it
// mentions a password, secret, token, credential or authorization, or its last
// word is "key" (encryption_key, db-key, key). key_length and batch_key_count
// are not secrets and are kept.
func IsSensitiveKey(name string) bool {
	k := strings.ToLower(name)
	for _, s := range []string{"password", "passwd", "secret", "token", "credential", "authorization"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	words := strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == ' ' })
	return len(words) > 0 && (words[len(words)-1] == "key" || words[len(words)-1] == "apikey")
}

// redactingCore replaces the value of every sensitive field before it is
// encoded, whether attached with With or passed to a log call.
type redactingCore struct{ zapcore.Core }

func redactFields(fields []zapcore.Field) []zapcore.Field {
	var out []zapcore.Field
	for i, f := range fields {
		if !IsSensitiveKey(f.Key) {
			continue
		}
		if out == nil {
			out = append([]zapcore.Field(nil), fields...)
		}
		out[i] = zap.String(f.Key, Redacted)
	}
	if out == nil {
		return fields
	}
	return out
}

func (c redactingCore) With(fields []zapcore.Field) zapcore.Core {
	return redactingCore{c.Core.With(redactFields(fields))}
}

func (c redactingCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c redactingCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(e, redactFields(fields))
}

// secretFlags take a secret as their value. --password-file and --token-file
// take a path and are not listed.
var secretFlags = map[string]bool{"--password": true, "-p": true, "--token": true}

// RedactArgs returns a copy of a command line with secret flag values
// replaced, for logging the invocation.
func RedactArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		a := out[i]
		if name, _, ok := strings.Cut(a, "="); ok && secretFlags[name] {
			out[i] = name + "=" + Redacted
			continue
		}
		if secretFlags[a] && i+1 < len(out) {
			out[i+1] = Redacted
			i++
			continue
		}
		// kingpin also accepts a short flag's value attached: -psecret.
		if strings.HasPrefix(a, "-p") && !strings.HasPrefix(a, "--") && len(a) > 2 {
			out[i] = "-p" + Redacted
		}
	}
	return out
}

// RedactJSON returns a JSON document with the value of every sensitive key
// replaced, at any depth. A document that does not parse is replaced entirely,
// because it cannot be shown to be free of secrets.
func RedactJSON(doc []byte) []byte {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return []byte(`"` + Redacted + ` (unparseable JSON)"`)
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return []byte(`"` + Redacted + `"`)
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if IsSensitiveKey(k) {
				t[k] = Redacted
			} else {
				t[k] = redactValue(val)
			}
		}
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
	}
	return v
}
