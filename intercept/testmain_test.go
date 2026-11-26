package intercept

import (
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/internal/testpg"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dsn, _, err := config.PostgresDSN()
	if err != nil {
		// Tests that need PostgreSQL skip themselves when it is not configured.
		os.Exit(m.Run())
	}
	os.Exit(testpg.Run(m, dsn))
}
