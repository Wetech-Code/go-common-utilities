package logger_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/Wetech-Code/go-common-utilities/logger"
)

func Example() {
	// Output defaults to os.Stderr; Stdout is used here so the example can be checked.
	log, err := logger.New(logger.Config{Format: "text", Output: os.Stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	log.Info("server started", "port", 8080, "password", "hunter2")
	// The time attribute is not deterministic, so only the log call is shown here.
}

// Send records to several destinations: any slog.Handler can go in Extra. The
// level and the secret redaction apply to all of them.
func ExampleConfig_extra() {
	var sink strings.Builder
	other := slog.NewJSONHandler(&sink, nil) // stand-in for a Loki or OTLP handler
	log, err := logger.New(logger.Config{Output: io.Discard, Extra: []slog.Handler{other}})
	if err != nil {
		return
	}
	log.Info("hello", "token", "abc123")
	fmt.Println(strings.Contains(sink.String(), "hello"), strings.Contains(sink.String(), "abc123"))
	// Output: true false
}
