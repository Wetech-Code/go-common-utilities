package config_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/config"
)

func TestReadsTypedValues(t *testing.T) {
	r := config.FromMap(map[string]string{
		"S": " hello ", "I": "42", "B": "true", "D": "1m30s", "L": "a, b,,c ",
	})
	if got := r.String("S", "x"); got != "hello" {
		t.Errorf("String = %q", got)
	}
	if got := r.Int("I", 0); got != 42 {
		t.Errorf("Int = %d", got)
	}
	if got := r.Bool("B", false); !got {
		t.Error("Bool = false")
	}
	if got := r.Duration("D", 0); got != 90*time.Second {
		t.Errorf("Duration = %v", got)
	}
	if got := r.Strings("L", nil); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("Strings = %q", got)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
}

func TestDefaultsForUnsetAndEmpty(t *testing.T) {
	r := config.FromMap(map[string]string{"EMPTY": "  "})
	if r.String("EMPTY", "d") != "d" || r.String("NOPE", "d") != "d" {
		t.Error("String default not used")
	}
	if r.Int("NOPE", 7) != 7 || !r.Bool("NOPE", true) || r.Duration("NOPE", time.Second) != time.Second {
		t.Error("default not used")
	}
	if got := r.Strings("NOPE", []string{"z"}); len(got) != 1 || got[0] != "z" {
		t.Errorf("Strings default = %q", got)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
}

func TestRequireAndInvalidAreCollected(t *testing.T) {
	r := config.FromMap(map[string]string{"PORT": "s3cr3t-not-a-number", "DEBUG": "maybe", "TO": "soon"})
	r.Require("DATABASE_URL")
	r.Int("PORT", 1)
	r.Bool("DEBUG", false)
	r.Duration("TO", 0)

	err := r.Err()
	if !errors.Is(err, config.ErrMissing) || !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("want ErrMissing and ErrInvalid, got %v", err)
	}
	for _, key := range []string{"DATABASE_URL", "PORT", "DEBUG", "TO"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
	if strings.Contains(err.Error(), "s3cr3t-not-a-number") {
		t.Errorf("error leaks the value: %v", err)
	}
}

func TestRequireReturnsValue(t *testing.T) {
	r := config.FromMap(map[string]string{"K": "v"})
	if got := r.Require("K"); got != "v" || r.Err() != nil {
		t.Errorf("Require = %q, err %v", got, r.Err())
	}
}

func TestNewReadsEnvironment(t *testing.T) {
	t.Setenv("GCU_TEST_CONFIG_KEY", "from-env")
	if got := config.New().String("GCU_TEST_CONFIG_KEY", ""); got != "from-env" {
		t.Errorf("got %q", got)
	}
}
