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
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("forward move migration: %v", err)
	}
	if err := db.QueryRow(`SELECT publication_name FROM deployments WHERE id=?`, id).Scan(&name); err != nil || name != id {
		t.Fatalf("move migration relocated legacy destination: %q %v", name, err)
	}
	var uniqueIndex string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='deployment_publication_name_unique'`).Scan(&uniqueIndex); err != nil || !strings.Contains(uniqueIndex, "WHERE publication_name<>''") {
		t.Fatalf("publication name index must remain partial under goose: %q %v", uniqueIndex, err)
	}
	legacyID := strings.Repeat("e", 32)
	if _, err := db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,created_at,updated_at) VALUES(?,?,'older-client','',1,1)`, legacyID, project); err != nil {
		t.Fatalf("older client insert must pass the partial index and receive ID binding: %v", err)
	}
	if err := db.QueryRow(`SELECT publication_name FROM deployments WHERE id=?`, legacyID).Scan(&name); err != nil || name != legacyID {
		t.Fatalf("older client destination=%q, err=%v", name, err)
	}
	if _, err := db.Exec(`INSERT INTO publication_moves(deployment_id,source_name,destination_name,old_dev,old_ino,created_at) VALUES(?,?, 'new-site',1,2,1)`, id, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,publication_name,created_at,updated_at) VALUES(?,?,'reserved','', 'new-site',1,1)`, strings.Repeat("d", 32), project); err == nil {
		t.Fatal("migration allowed a create to claim an unresolved move destination")
	}
}
