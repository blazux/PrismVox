// Package privacy keeps conversation content out of routine diagnostics.
package privacy

import (
	"log"
	"os"
	"strings"
)

// Logf is an explicit troubleshooting opt-in. Persistent call records are unaffected.
func Logf(format string, args ...interface{}) {
	v := strings.ToLower(os.Getenv("LOG_SENSITIVE_DATA"))
	if v == "true" || v == "1" {
		log.Printf(format, args...)
	}
}
