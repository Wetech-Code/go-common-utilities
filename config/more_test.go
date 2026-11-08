package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/config"
)

const secret = "s3cr3t-value"

func mustNotLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks a value: %v", err)
	}
}

func TestNumberReaders(t *testing.T) {
	r := config.FromMap(map[string]string{"I64": "-9000000000", "U": "42", "F": "2.5"})
	if got := r.Int64("I64", 0); got != -9000000000 {
		t.Errorf("Int64 = %d", got)
	}
	if got := r.Uint("U", 0); got != 42 {
		t.Errorf("Uint = %d", got)
	}
	if got := r.Float64("F", 0); got != 2.5 {
		t.Errorf("Float64 = %v", got)
	}
	if r.Int64("NOPE", 7) != 7 || r.Uint("NOPE", 8) != 8 || r.Float64("NOPE", 1.5) != 1.5 || r.Err() != nil {
		t.Error("defaults not used")
	}

	bad := config.FromMap(map[string]string{"I64": secret, "U": "-1", "F1": "NaN", "F2": "inf", "F3": secret})
	bad.Int64("I64", 3)
	bad.Uint("U", 4)
	for _, k := range []string{"F1", "F2", "F3"} {
		if got := bad.Float64(k, 9); got != 9 {
			t.Errorf("%s: Float64 = %v, want the default", k, got)
		}
	}
	err := bad.Err()
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	for _, k := range []string{"I64", "U", "F1", "F2", "F3"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("no mention of %s: %v", k, err)
		}
	}
	mustNotLeak(t, err)
}

func TestRanges(t *testing.T) {
	r := config.FromMap(map[string]string{"P": "8080", "LOW": "0", "HIGH": "70000", "TXT": secret, "D": "30s", "DLOW": "1ms", "DHIGH": "2h", "DTXT": secret})
	if got := r.IntRange("P", 1, 1, 65535); got != 8080 {
		t.Errorf("IntRange = %d", got)
	}
	if r.IntRange("LOW", 5, 1, 65535) != 5 || r.IntRange("HIGH", 6, 1, 65535) != 6 || r.IntRange("TXT", 7, 1, 65535) != 7 {
		t.Error("an invalid value must give the default")
	}
	if got := r.IntRange("NOPE", 9, 1, 10); got != 9 {
		t.Errorf("default = %d", got)
	}
	if got := r.DurationRange("D", time.Second, time.Second, time.Minute); got != 30*time.Second {
		t.Errorf("DurationRange = %v", got)
	}
	if r.DurationRange("DLOW", time.Second, time.Second, time.Minute) != time.Second ||
		r.DurationRange("DHIGH", time.Second, time.Second, time.Minute) != time.Second ||
		r.DurationRange("DTXT", time.Second, time.Second, time.Minute) != time.Second ||
		r.DurationRange("NOPE", 3*time.Second, time.Second, time.Minute) != 3*time.Second {
		t.Error("DurationRange defaults")
	}
	err := r.Err()
	for _, want := range []string{"LOW must be an integer from 1 to 65535", "HIGH must be", "TXT must be an integer", "DLOW must be a duration from 1s to 1m0s", "DHIGH", "DTXT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	mustNotLeak(t, err)
}

func TestBytes(t *testing.T) {
	good := map[string]int64{
		"0": 0, "1048576": 1048576, "5B": 5, "10MB": 10_000_000, "10 mb": 10_000_000, "1.5GB": 1_500_000_000,
		"512KiB": 512 * 1024, "2gib": 2 << 30, "1TiB": 1 << 40, "3KB": 3000, "0.5MiB": 512 * 1024,
	}
	for in, want := range good {
		r := config.FromMap(map[string]string{"K": in})
		if got := r.Bytes("K", -1); got != want || r.Err() != nil {
			t.Errorf("Bytes(%q) = %d, err %v; want %d", in, got, r.Err(), want)
		}
	}
	for _, in := range []string{"-1", "abc", "MB", "1.5", "1.5B", "10XB", "1e3", "9223372036854775808", "10000000TB", ".", "1..2MB", secret} {
		r := config.FromMap(map[string]string{"K": in})
		if got := r.Bytes("K", 123); got != 123 || !errors.Is(r.Err(), config.ErrInvalid) {
			t.Errorf("Bytes(%q) = %d, err %v; want the default and ErrInvalid", in, got, r.Err())
		}
		mustNotLeak(t, r.Err())
	}
	if got := config.FromMap(nil).Bytes("K", 77); got != 77 {
		t.Errorf("default = %d", got)
	}
	// float64 cannot hold MaxInt64 exactly: it rounds up to 2^63 and is refused, the safe outcome.
	if got := config.FromMap(map[string]string{"K": "9223372036854775807"}).Bytes("K", 5); got != 5 {
		t.Errorf("MaxInt64 string gave %d, want the default", got)
	}
	if got := config.FromMap(map[string]string{"K": "9200000000000000000"}).Bytes("K", 5); got != 9_200_000_000_000_000_000 {
		t.Errorf("a large valid size gave %d", got)
	}
}

func TestURL(t *testing.T) {
	r := config.FromMap(map[string]string{
		"A": "https://example.com/x?y=1", "B": "postgres://user:" + secret + "@db:5432/app",
		"C": "ftp://example.com", "D": "/relative", "E": "https://", "F": "not a url " + secret, "G": "HTTP://Example.com",
	})
	if got := r.URL("A", ""); got != "https://example.com/x?y=1" {
		t.Errorf("URL = %q", got)
	}
	if got := r.URL("B", "", "postgres", "postgresql"); !strings.HasPrefix(got, "postgres://") {
		t.Errorf("postgres URL = %q", got)
	}
	if got := r.URL("G", ""); got != "HTTP://Example.com" {
		t.Errorf("scheme matching should ignore case, got %q", got)
	}
	for _, k := range []string{"C", "D", "E", "F"} {
		if got := r.URL(k, "dflt"); got != "dflt" {
			t.Errorf("%s: URL = %q, want the default", k, got)
		}
	}
	if r.URL("B", "") != "" || r.URL("B", "dflt") != "dflt" { // B is not http(s)
		t.Error("scheme list must be enforced")
	}
	if got := r.URL("NOPE", "d"); got != "d" {
		t.Errorf("default = %q", got)
	}
	err := r.Err()
	if !strings.Contains(err.Error(), "C must be an absolute URL with scheme http or https") {
		t.Errorf("err = %v", err)
	}
	mustNotLeak(t, err)
}

func TestMapAndEnum(t *testing.T) {
	r := config.FromMap(map[string]string{"M": " env = prod ,team=ops,,env=staging", "BAD1": "a=1,novalue", "BAD2": "=x", "E": "json", "E2": secret})
	want := map[string]string{"env": "staging", "team": "ops"}
	if got := r.Map("M", nil); !reflect.DeepEqual(got, want) {
		t.Errorf("Map = %v", got)
	}
	if r.Map("BAD1", map[string]string{"d": "1"})["d"] != "1" || r.Map("BAD2", nil) != nil {
		t.Error("an invalid map must give the default")
	}
	if got := r.Map("NOPE", map[string]string{"d": "1"}); got["d"] != "1" {
		t.Errorf("default = %v", got)
	}
	if got := r.Enum("E", "text", "json", "text"); got != "json" {
		t.Errorf("Enum = %q", got)
	}
	if got := r.Enum("E2", "text", "json", "text"); got != "text" {
		t.Errorf("Enum invalid = %q", got)
	}
	if got := r.Enum("NOPE", "text", "json", "text"); got != "text" {
		t.Errorf("Enum default = %q", got)
	}
	err := r.Err()
	if !strings.Contains(err.Error(), "E2 must be one of json, text") || !strings.Contains(err.Error(), "BAD1 must be comma-separated key=value pairs") {
		t.Errorf("err = %v", err)
	}
	mustNotLeak(t, err)
}

func TestPrefixHasAndInvalid(t *testing.T) {
	r := config.FromMap(map[string]string{"APP_PORT": "9000", "APP_BAD": secret, "PORT": "1"}).WithPrefix("APP_")
	if got := r.Int("PORT", 1); got != 9000 {
		t.Errorf("prefixed Int = %d", got)
	}
	if !r.Has("PORT") || r.Has("NOPE") || r.Has("BAD") == false {
		t.Error("Has")
	}
	r.Require("DATABASE_URL")
	r.Int("BAD", 0)
	r.Invalid("TLS_CERT", "set when TLS is enabled")
	err := r.Err()
	for _, want := range []string{"missing required setting: APP_DATABASE_URL", "APP_BAD must be an integer", "APP_TLS_CERT must be set when TLS is enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if !errors.Is(err, config.ErrMissing) || !errors.Is(err, config.ErrInvalid) {
		t.Errorf("err kinds: %v", err)
	}
	mustNotLeak(t, err)
}

func TestFileSecrets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pw := write("pw", "  hunter2\n")
	empty := write("empty", " \n")
	big := write("big", strings.Repeat("x", 1<<20+1))

	r := config.FromMap(map[string]string{
		"DB_PASSWORD_FILE": pw, "BOTH": "from-env", "BOTH_FILE": pw, "EMPTY_FILE": empty,
		"BIG_FILE": big, "GONE_FILE": filepath.Join(dir, "does-not-exist-"+secret), "NUM_FILE": write("num", "42\n"),
	}).WithFileSecrets()
	if got := r.Require("DB_PASSWORD"); got != "hunter2" {
		t.Errorf("file secret = %q", got)
	}
	if got := r.String("BOTH", ""); got != "from-env" {
		t.Errorf("the variable must win over its _FILE: %q", got)
	}
	if got := r.Int("NUM", 0); got != 42 {
		t.Errorf("typed read from a file = %d", got)
	}
	if got := r.String("EMPTY", "d"); got != "d" {
		t.Errorf("an empty file counts as unset: %q", got)
	}
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	r.String("BIG", "")
	r.String("GONE", "")
	err := r.Err()
	if !strings.Contains(err.Error(), "BIG_FILE could not be read") || !strings.Contains(err.Error(), "GONE_FILE could not be read") {
		t.Errorf("err = %v", err)
	}
	mustNotLeak(t, err)
	if strings.Contains(err.Error(), dir) {
		t.Errorf("error leaks the file path: %v", err)
	}

	// Without WithFileSecrets the _FILE variable is just another variable.
	off := config.FromMap(map[string]string{"DB_PASSWORD_FILE": pw})
	if off.String("DB_PASSWORD", "none") != "none" {
		t.Error("files must not be read unless WithFileSecrets is called")
	}
	// Prefix and files work together.
	both := config.FromMap(map[string]string{"APP_TOKEN_FILE": pw}).WithPrefix("APP_").WithFileSecrets()
	if both.String("TOKEN", "") != "hunter2" {
		t.Error("prefix + file secret")
	}
}

func envFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromFile(t *testing.T) {
	t.Setenv("GCU_ENVFILE_FROM_ENV", "real-environment")
	t.Setenv("GCU_ENVFILE_EMPTY_IN_ENV", "")
	p := envFile(t, "\xef\xbb\xbf# a comment\n"+
		"\n"+
		"GCU_ENVFILE_PLAIN=hello world\n"+
		"export GCU_ENVFILE_EXPORTED = spaced \n"+
		"GCU_ENVFILE_SINGLE='keep $this # literally'\n"+
		`GCU_ENVFILE_DOUBLE="line1\nline2\t\"q\" \\ \x"`+"\n"+
		"GCU_ENVFILE_INLINE=value # a comment\n"+
		"GCU_ENVFILE_HASH=a#b\n"+
		"GCU_ENVFILE_EMPTY=\n"+
		"GCU_ENVFILE_FROM_ENV=from-file\n"+
		"GCU_ENVFILE_EMPTY_IN_ENV=from-file\n"+
		"GCU_ENVFILE_URL=postgres://u:p@h/db?x=1\n")
	r, err := config.FromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"GCU_ENVFILE_PLAIN": "hello world", "GCU_ENVFILE_EXPORTED": "spaced", "GCU_ENVFILE_SINGLE": "keep $this # literally",
		"GCU_ENVFILE_DOUBLE": "line1\nline2\t\"q\" \\ \\x", "GCU_ENVFILE_INLINE": "value", "GCU_ENVFILE_HASH": "a#b",
		"GCU_ENVFILE_FROM_ENV": "real-environment", "GCU_ENVFILE_URL": "postgres://u:p@h/db?x=1",
	} {
		if got := r.String(key, "UNSET"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if r.String("GCU_ENVFILE_EMPTY", "default") != "default" {
		t.Error("an empty value counts as unset")
	}
	// A variable that is set but empty in the real environment still counts as set
	// there, so the file does not override it (and the value is then empty = unset).
	if got := r.String("GCU_ENVFILE_EMPTY_IN_ENV", "default"); got != "default" {
		t.Errorf("empty real variable: %q", got)
	}
	if r.Err() != nil {
		t.Fatal(r.Err())
	}
}

func TestFromFileErrors(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"no equals":     {"GOOD=1\n" + secret + "\n", "line 2 is not KEY=VALUE"},
		"bad key":       {"1BAD=x\n", "line 1 is not KEY=VALUE"},
		"key with dash": {"MY-KEY=" + secret + "\n", "line 1 is not KEY=VALUE"},
		"empty key":     {"=" + secret + "\n", "line 1 is not KEY=VALUE"},
		"open double":   {"A=\"" + secret + "\n", "line 1 has an unterminated quote"},
		"open single":   {"\n\nA='" + secret + "\n", "line 3 has an unterminated quote"},
		"dangling esc":  {"A=\"" + secret + "\\", "line 1 has an unterminated quote"},
	} {
		_, err := config.FromFile(envFile(t, tc.content))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, config.ErrInvalid) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
			continue
		}
		mustNotLeak(t, err)
	}
	if _, err := config.FromFile(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Error("a missing file must be an error, not silently ignored")
	}
	if _, err := config.FromFile(envFile(t, "A="+strings.Repeat("x", 1<<20+10))); !errors.Is(err, config.ErrInvalid) {
		t.Errorf("an oversized env file must be refused: %v", err)
	}
	if _, err := config.FromFile(t.TempDir()); err == nil {
		t.Error("a directory is not an env file")
	}
}
