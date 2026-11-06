package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMainExportsContractWithoutDatabase(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "contract-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = original }()
	main()
	if _, err = file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var contract struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err = json.NewDecoder(file).Decode(&contract); err != nil {
		t.Fatal("command did not export JSON", err)
	}
	if contract.OpenAPI != "3.1.0" {
		t.Fatal("unexpected OpenAPI version", contract.OpenAPI)
	}
	for _, path := range []string{"/api/v1/session/login", "/api/v1/account/setup", "/api/v1/organizations"} {
		if len(contract.Paths[path]) == 0 {
			t.Fatalf("missing implemented operation %s", path)
		}
	}
	if len(contract.Paths["/worker/v1/poll"]) != 0 {
		t.Fatal("exported unimplemented worker endpoint")
	}
}

func TestMainFailsWhenContractCannotBeWritten(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "closed-output"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = original }()
	defer func() {
		if recover() == nil {
			t.Error("contract command ignored an output failure")
		}
	}()
	main()
}
