package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sqlite-trigger <database> <install|remove>")
		os.Exit(2)
	}
	db, err := sql.Open("sqlite3", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	var query string
	switch os.Args[2] {
	case "install":
		query = `CREATE TRIGGER inject_credential_replacement_failure BEFORE UPDATE OF registration_record ON administrator BEGIN SELECT RAISE(ABORT, 'injected credential replacement failure'); END`
	case "remove":
		query = `DROP TRIGGER IF EXISTS inject_credential_replacement_failure`
	default:
		fmt.Fprintln(os.Stderr, "unknown action")
		os.Exit(2)
	}
	if _, err := db.Exec(query); err != nil {
		panic(err)
	}
}
