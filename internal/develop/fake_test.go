package develop

import (
	"os"
	"testing"

	"github.com/jefflaplante/cull/internal/develop/lctest"
)

// The tests run this test binary as lightcraft-cli (see lctest).
func TestMain(m *testing.M) {
	if os.Getenv(lctest.Env) == "1" {
		os.Exit(lctest.Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}
