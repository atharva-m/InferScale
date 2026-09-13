package migrations

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestEmbeddedMigrationsAreGooseCompatible(t *testing.T) {
	files, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no forward migrations were embedded")
	}
	for _, name := range files {
		contents, err := FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(contents), "-- +goose Up\n") {
			t.Fatalf("%s does not begin with the Goose Up directive", name)
		}
	}

	goose.SetBaseFS(FS)
	migrations, err := goose.CollectMigrations(".", 0, int64(^uint64(0)>>1))
	if err != nil {
		t.Fatalf("collect embedded migrations: %v", err)
	}
	if len(migrations) != len(files) {
		t.Fatalf("collected %d migrations from %d embedded files", len(migrations), len(files))
	}
}
