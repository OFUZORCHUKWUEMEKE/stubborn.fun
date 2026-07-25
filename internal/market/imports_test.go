package market

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPackageDoesNotImportLedgerOrStake enforces the dependency rule from
// the design: market is pure domain plus persistence, so money movement
// cannot be triggered from inside a state transition. stake and settle
// orchestrate market and ledger together; market must never reach the
// other way. Checked by parsing the package's own source rather than by
// convention, so a future edit that adds the import fails a test instead
// of silently coupling the packages.
func TestPackageDoesNotImportLedgerOrStake(t *testing.T) {
	forbidden := []string{
		"stubborn.fun/internal/ledger",
		"stubborn.fun/internal/stake",
		"stubborn.fun/internal/settle",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages parsed; test is not actually checking anything")
	}

	checked := 0
	for _, pkg := range pkgs {
		for filename, file := range pkg.Files {
			checked++
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: bad import path %s", filename, imp.Path.Value)
				}
				for _, bad := range forbidden {
					if strings.Contains(path, bad) {
						t.Errorf("%s imports %s: market must not depend on it", filename, path)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no files checked; test is not actually checking anything")
	}
	t.Logf("verified %d files in the market package", checked)
}

// TestNoWallClockInDomainLogic enforces "no time.Now() in domain logic":
// every instant that ends up persisted or compared against close_at must
// come from the injected Clock, or the scheduler stops being testable and
// close-time behaviour stops being reproducible.
//
// scheduler.go is exempt only for time.NewTicker, which paces the polling
// loop itself (real elapsed time, never a domain decision) — so this
// checks specifically for time.Now, not all uses of the time package.
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
			continue // tests legitimately use time.Now for their own deadlines
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
