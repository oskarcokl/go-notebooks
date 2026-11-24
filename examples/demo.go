// %% [markdown]
// # go-notebooks demo
//
// This notebook is an **ordinary Go file**. Open it with
//
//     go-notebooks serve examples/demo.go
//
// or compile it like any other program:
//
//     go build examples/demo.go && ./demo
//
// Every `// %%` comment starts a cell. Cells above `func main` hold
// declarations; cells inside it hold statements. Press **Shift+Enter** to run
// a cell.

package main

// %% Imports
import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// %% [markdown]
// ## Types and functions
//
// Try this once everything has run: add a field `Email string` to `User` and
// re-run its cell. The kernel keeps going: `users` is marked *stale*, cells
// that do not use it still work, and cells that do tell you which cell to
// re-run.

// %% A struct
type User struct {
	Name     string `json:"name"`
	Score    int    `json:"score"`
	LastName string `json":last_name"`
}

// %% Methods and functions
func (u User) Level() string {
	if u.Score >= 80 {
		return "expert"
	}
	return "learner"
}

func rank(users []User) []User {
	out := append([]User{}, users...)
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func main() {
	// %% [markdown]
	// ## Live memory
	//
	// Variables stay in memory between cells. They need not be used until the
	// file is built.

	// %% Create some users
	users := []User{
		{Name: "Ada", Score: 95},
		{Name: "Grace", Score: 88},
		{Name: "Linus", Score: 61},
	}

	// %% An unrelated counter
	runs := 0

	// %% Use them
	runs++
	for i, u := range rank(users) {
		fmt.Printf("%d. %-6s %3d  %s\n", i+1, u.Name, u.Score, u.Level())
	}
	fmt.Println("this cell ran", runs, "time(s)")

	// %% [markdown]
	// ## Anything Go can do
	//
	// Goroutines, channels and the standard library work as usual.

	// %% Goroutines and channels
	results := make(chan string)
	for _, u := range users {
		go func() {
			time.Sleep(time.Duration(100-u.Score) * time.Millisecond)
			results <- strings.ToUpper(u.Name)
		}()
	}
	for range users {
		fmt.Println(<-results)
	}

	// %% JSON
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	// %%
	users[2].Score = 51
}
