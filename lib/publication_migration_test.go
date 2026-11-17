package lib

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestReviewedPublicationMigrationKeepsLegacyDestinationWithoutRelocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := migrationSource(Config{})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if _, err := provider.UpTo(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	project := strings.Repeat("b", 32)
	if _, err := db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,'fixture','https://example.test/repo',1)`, project); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,created_at,updated_at) VALUES(?,?,'legacy','',1,1)`, id, project); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow(`SELECT publication_name FROM deployments WHERE id=?`, id).Scan(&name); err != nil || name != id {
		t.Fatalf("legacy binding=%q, err=%v", name, err)
	}
	if _, err := db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,publication_name,created_at,updated_at) VALUES(?,?,'other','',?,1,1)`, strings.Repeat("c", 32), project, id); err == nil {
		t.Fatal("migration allowed a second deployment to claim the legacy destination")
	}
}
