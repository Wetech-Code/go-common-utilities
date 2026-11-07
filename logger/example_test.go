package logger_test

import (
	"fmt"
	"os"

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
