// %% [markdown]
// # SQLite, through cgo
//
// `github.com/mattn/go-sqlite3` is a cgo package. It is compiled once into
// this project's kernel; the `*sql.DB` below then stays open across cells.

package main

// %% Imports
import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// %% Types
type Book struct {
	ID    int
	Title string
	Year  int
}

// %% Queries
func booksSince(db *sql.DB, year int) ([]Book, error) {
	rows, err := db.Query("SELECT id, title, year FROM books WHERE year >= ? ORDER BY year", year)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Year); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func main() {
	// %% Open the database
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// %% Fill it
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS books (id INTEGER PRIMARY KEY, title TEXT, year INT);
		INSERT INTO books (title, year) VALUES
			('The C Programming Language', 1978),
			('The Go Programming Language', 2015),
			('Learning Go', 2021);
	`)
	if err != nil {
		panic(err)
	}

	// %% Query it
	books, err := booksSince(db, 2000)
	if err != nil {
		panic(err)
	}
	for _, b := range books {
		fmt.Println(b.Year, b.Title)
	}
}
