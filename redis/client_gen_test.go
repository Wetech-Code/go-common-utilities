package redis

import (
	"reflect"
	"sort"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

// Every go-redis command method must have a Client wrapper. If this fails after
// upgrading go-redis, run `go generate ./redis` and review the new methods.
func TestEveryGoRedisCommandIsWrapped(t *testing.T) {
	native := reflect.TypeOf(&goredis.Client{})
	ours := reflect.TypeOf(&Client{})

	// go-redis implements these three as panic("not implemented"); wrapping them
	// would make Client panic, so they are deliberately absent.
	stubs := map[string]bool{"Quit": true, "Sync": true, "JSONDebugMemory": true}

	var missing []string
	commands := 0
	for i := 0; i < native.NumMethod(); i++ {
		m := native.Method(i)
		if m.Type.NumOut() != 1 || m.Type.Out(0).Kind() != reflect.Ptr {
			continue
		}
		if _, ok := m.Type.Out(0).MethodByName("Result"); !ok {
			continue // not a command (Pipeline, Subscribe, Monitor, ...)
		}
		if stubs[m.Name] {
			if _, wrapped := ours.MethodByName(m.Name); wrapped {
				t.Errorf("%s is a go-redis stub that panics and must not be wrapped", m.Name)
			}
			continue
		}
		commands++
		if _, ok := ours.MethodByName(m.Name); !ok {
			missing = append(missing, m.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d of %d go-redis commands have no wrapper: %v", len(missing), commands, missing)
	}
	t.Logf("all %d go-redis commands are wrapped (%d methods on Client)", commands, ours.NumMethod())
}
