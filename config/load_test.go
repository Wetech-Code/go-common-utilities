package config_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/config"
)

type tags []string
type labels map[string]string

type appConfig struct {
	Port     int               `env:"PORT" default:"8080" min:"1" max:"65535"`
	DSN      string            `env:"DATABASE_URL" required:"true"`
	LogLevel string            `env:"LOG_LEVEL" default:"info" enum:"debug,info,warn,error"`
	Timeout  time.Duration     `env:"TIMEOUT" default:"30s" max:"5m"`
	Debug    bool              `env:"DEBUG"`
	Workers  uint8             `env:"WORKERS" default:"4"`
	Ratio    float32           `env:"RATIO" default:"0.5" min:"0" max:"1"`
	Big      int64             `env:"BIG"`
	Origins  []string          `env:"ORIGINS"`
	Tags     tags              `env:"TAGS"`
	Meta     map[string]string `env:"META"`
	Labels   labels            `env:"LABELS"`
	Ignored  string            // no env tag: left alone
	private  string            `env:"PRIVATE"`
	Redis    struct {
		URL  string `env:"URL" required:"true"`
		Size int    `env:"POOL" default:"10"`
	} `envPrefix:"REDIS_"`
	Nested struct {
		Name string `env:"NAME" default:"n"`
	}
}

func TestLoadFillsAStruct(t *testing.T) {
	r := config.FromMap(map[string]string{
		"DATABASE_URL": "postgres://db", "PORT": "9090", "LOG_LEVEL": "warn", "TIMEOUT": "2m", "DEBUG": "true",
		"WORKERS": "16", "RATIO": "0.25", "BIG": "-5000000000", "ORIGINS": "a.com, b.com", "TAGS": "x,y",
		"META": "k=v,k2=v2", "LABELS": "l=1", "REDIS_URL": "redis://r", "PRIVATE": "nope", "NAME": "top-level",
	})
	var c appConfig
	c.Ignored = "keep"
	r.Load(&c)
	if err := r.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	want := appConfig{Port: 9090, DSN: "postgres://db", LogLevel: "warn", Timeout: 2 * time.Minute, Debug: true, Workers: 16,
		Ratio: 0.25, Big: -5000000000, Origins: []string{"a.com", "b.com"}, Tags: tags{"x", "y"},
		Meta: map[string]string{"k": "v", "k2": "v2"}, Labels: labels{"l": "1"}, Ignored: "keep"}
	want.Redis.URL, want.Redis.Size = "redis://r", 10
	want.Nested.Name = "top-level"
	if !reflect.DeepEqual(c, want) {
		t.Errorf("loaded\n%+v\nwant\n%+v", c, want)
	}
}

func TestLoadDefaultsAndMissing(t *testing.T) {
	r := config.FromMap(map[string]string{"DATABASE_URL": "d", "REDIS_URL": "r"})
	var c appConfig
	r.Load(&c)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if c.Port != 8080 || c.LogLevel != "info" || c.Timeout != 30*time.Second || c.Workers != 4 || c.Ratio != 0.5 || c.Debug || c.Origins != nil || c.Redis.Size != 10 {
		t.Errorf("defaults: %+v", c)
	}

	empty := config.FromMap(nil)
	var c2 appConfig
	empty.Load(&c2)
	err := empty.Err()
	for _, want := range []string{"missing required setting: DATABASE_URL", "missing required setting: REDIS_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if !errors.Is(err, config.ErrMissing) || errors.Is(err, config.ErrInvalid) {
		t.Errorf("error kinds: %v", err)
	}
}

func TestLoadReportsEveryProblemWithoutLeaking(t *testing.T) {
	r := config.FromMap(map[string]string{
		"DATABASE_URL": "d", "REDIS_URL": "r",
		"PORT": "70000", "LOG_LEVEL": secret, "TIMEOUT": "10m", "WORKERS": "300", "RATIO": "7", "BIG": secret,
		"DEBUG": secret, "META": "novalue",
	})
	var c appConfig
	r.Load(&c)
	err := r.Err()
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"PORT must be an integer from 1 to 65535", "LOG_LEVEL must be one of debug, info, warn, error",
		"TIMEOUT must be a duration of at most 5m0s", "WORKERS must be a non-negative integer", "RATIO must be a number from 0 to 1",
		"BIG must be an integer", "DEBUG must be a boolean", "META must be comma-separated key=value pairs",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	mustNotLeak(t, err)
	// Invalid fields keep their zero value.
	if c.Port != 0 || c.LogLevel != "" || c.Timeout != 0 {
		t.Errorf("invalid fields must stay zero: %+v", c)
	}
}

func TestLoadWithPrefixAndFileSecrets(t *testing.T) {
	pw := envFile(t, "from-a-file\n")
	r := config.FromMap(map[string]string{"APP_REDIS_URL": "r", "APP_DATABASE_URL_FILE": pw}).WithPrefix("APP_").WithFileSecrets()
	var c appConfig
	r.Load(&c)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if c.DSN != "from-a-file" || c.Redis.URL != "r" {
		t.Errorf("c = %+v", c)
	}
	// The prefix is restored after a nested struct, so a later error names the right variable.
	r2 := config.FromMap(nil).WithPrefix("APP_")
	var c2 appConfig
	r2.Load(&c2)
	r2.Require("AFTER")
	if !strings.Contains(r2.Err().Error(), "APP_AFTER") || strings.Contains(r2.Err().Error(), "APP_REDIS_APP_") {
		t.Errorf("err = %v", r2.Err())
	}
}

func TestLoadRejectsBadTargetsAndTags(t *testing.T) {
	for name, v := range map[string]any{"nil": nil, "not a pointer": appConfig{}, "pointer to int": new(int), "nil pointer": (*appConfig)(nil)} {
		r := config.FromMap(nil)
		r.Load(v)
		if !errors.Is(r.Err(), config.ErrInvalid) {
			t.Errorf("%s: err = %v", name, r.Err())
		}
	}

	var unsupported struct {
		Ch   chan int    `env:"CH"`
		List []int       `env:"LIST"`
		M    map[int]int `env:"M"`
	}
	r := config.FromMap(map[string]string{"CH": "x", "LIST": "1", "M": "1"})
	r.Load(&unsupported)
	for _, want := range []string{"CH has the unsupported type chan int", "LIST has the unsupported type []int", "M has the unsupported type map[int]int"} {
		if r.Err() == nil || !strings.Contains(r.Err().Error(), want) {
			t.Errorf("missing %q in %v", want, r.Err())
		}
	}

	var badTags struct {
		A int           `env:"A" min:"low"`
		B time.Duration `env:"B" max:"soon"`
	}
	r = config.FromMap(map[string]string{"A": "1", "B": "1s"})
	r.Load(&badTags)
	if !strings.Contains(r.Err().Error(), "the min tag of A") || !strings.Contains(r.Err().Error(), "the max tag of B") {
		t.Errorf("err = %v", r.Err())
	}

	// A bad default is reported like any bad value.
	var badDefault struct {
		N int `env:"N" default:"many"`
	}
	r = config.FromMap(nil)
	r.Load(&badDefault)
	if !strings.Contains(r.Err().Error(), "N must be an integer") {
		t.Errorf("err = %v", r.Err())
	}
}

func TestLoadBoundsWithOnlyMinOrMax(t *testing.T) {
	var c struct {
		Low  int           `env:"LOW" min:"10"`
		High uint          `env:"HIGH" max:"5"`
		Dur  time.Duration `env:"DUR" min:"1s"`
		F    float64       `env:"F" min:"0.5"`
	}
	r := config.FromMap(map[string]string{"LOW": "9", "HIGH": "6", "DUR": "1ms", "F": "0.1"})
	r.Load(&c)
	for _, want := range []string{"LOW must be an integer of at least 10", "HIGH must be an integer of at most 5", "DUR must be a duration of at least 1s", "F must be a number of at least 0.5"} {
		if !strings.Contains(r.Err().Error(), want) {
			t.Errorf("missing %q in %v", want, r.Err())
		}
	}
	// In range: accepted.
	ok := config.FromMap(map[string]string{"LOW": "10", "HIGH": "5", "DUR": "1s", "F": "0.5"})
	var c2 = c
	ok.Load(&c2)
	if ok.Err() != nil || c2.Low != 10 || c2.High != 5 || c2.Dur != time.Second || c2.F != 0.5 {
		t.Errorf("c2 = %+v, err %v", c2, ok.Err())
	}
}

func TestLoadSurvivesOverflowingIntegerSizes(t *testing.T) {
	var c struct {
		I8  int8   `env:"I8"`
		U16 uint16 `env:"U16"`
	}
	r := config.FromMap(map[string]string{"I8": "200", "U16": "70000"})
	r.Load(&c)
	if !strings.Contains(r.Err().Error(), "I8 must be an integer") || !strings.Contains(r.Err().Error(), "U16 must be a non-negative integer") || c.I8 != 0 || c.U16 != 0 {
		t.Errorf("c = %+v, err %v", c, r.Err())
	}
}
