// Contract export needs no database, credentials or running server.
package main

import (
	"encoding/json"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"os"
)

func main() {
	_, api := httpapi.New(nil, "https://localhost:5173")
	e := json.NewEncoder(os.Stdout).Encode(api.OpenAPI())
	if e != nil {
		panic(e)
	}
}
