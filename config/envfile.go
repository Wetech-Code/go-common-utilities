package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// FromFile returns a Reader for the process environment with the settings of a
// .env file beneath it: a variable that is set in the real environment always wins
// over the file, so a deployment can override anything. It is meant for local
// development; nothing reads a file unless you call it.
//
// File format: one KEY=VALUE per line; blank lines and lines starting with # are
// ignored; an optional leading "export "; a value may be wrapped in single quotes
// (kept literally) or double quotes (with \n, \t, \\ and \" escapes); an unquoted
// value ends at a space followed by # (an inline comment). There is no variable
// expansion. A key must be letters, digits and underscores and not start with a
// digit. Files larger than 1 MiB are refused.
//
// The error names the line number of a malformed line but never quotes the line,
// because it may contain a secret.
func FromFile(path string) (*Reader, error) {
	vars, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}
	return &Reader{lookup: func(k string) (string, bool) {
		if v, ok := os.LookupEnv(k); ok {
			return v, true
		}
		v, ok := vars[k]
		return v, ok
	}}, nil
}

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open env file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("config: read env file: %w", err)
	}
	if info.Size() > maxSecretFile {
		return nil, fmt.Errorf("%w: env file is larger than 1 MiB", ErrInvalid)
	}

	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf") // byte order mark
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !validKey(key) {
			return nil, fmt.Errorf("%w: env file line %d is not KEY=VALUE", ErrInvalid, n)
		}
		val, ok := parseEnvValue(strings.TrimSpace(raw))
		if !ok {
			return nil, fmt.Errorf("%w: env file line %d has an unterminated quote", ErrInvalid, n)
		}
		vars[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: read env file: %w", err)
	}
	return vars, nil
}

func validKey(k string) bool {
	if k == "" || k[0] >= '0' && k[0] <= '9' {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func parseEnvValue(v string) (string, bool) {
	if v == "" {
		return "", true
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", false
		}
		return v[1 : 1+end], true
	case '"':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			switch c := v[i]; c {
			case '"':
				return b.String(), true
			case '\\':
				if i+1 >= len(v) {
					return "", false
				}
				i++
				switch v[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case '\\', '"':
					b.WriteByte(v[i])
				default:
					b.WriteByte('\\')
					b.WriteByte(v[i])
				}
			default:
				b.WriteByte(c)
			}
		}
		return "", false
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), true
}
