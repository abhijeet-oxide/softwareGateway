package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "file:./dev/swgw.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE package_security SET state='synced', started_at=NULL WHERE state='syncing'`)
	if err != nil {
		panic(err)
	}
	n, _ := res.RowsAffected()
	fmt.Println("released stale claims:", n)
}
