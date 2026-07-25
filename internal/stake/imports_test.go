package stake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoWallClockInDomainLogic mirrors the market package's guard: stake
// timestamps positions and evaluates close_at deadlines, so every instant
// must come from the injected platform.Clock or the deadline tests stop
// being reproducible.
func TestNoWallClockInDomainLogic(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no sources found; test is not actually checking anything")
	}

	checked := 0
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		if strings.Contains(string(body), "time.Now(") {
			t.Errorf("%s calls time.Now(): domain logic must use the injected platform.Clock", name)
		}
	}
	if checked == 0 {
		t.Fatal("no non-test sources checked; test is not actually checking anything")
	}
	t.Logf("verified %d non-test sources use no wall clock", checked)
}
