// Package architecture enforces the dependency rule inside every service
// (see docs/adr/0007-service-code-structure.md):
//
//	domain   -> imports nothing from the service and no infrastructure libraries
//	app      -> may import domain, never adapters
//	adapters -> may import domain and app
//
// Cross-service imports are already impossible: each service keeps its code
// under services/<name>/internal, which the Go compiler makes private to it.
package architecture

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/HuzaifaMH/go-ecommerce-microservices"

type pkg struct {
	ImportPath string
	Imports    []string
}

// infrastructure lists import prefixes the domain layer must never depend on.
var infrastructure = []string{
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
	"github.com/nats-io/",
	"github.com/jackc/pgx",
	"database/sql",
	"net/http",
	module + "/gen",
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list failed: %v", err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// layerOf returns the layer ("domain", "app", "adapters") for an import path
// inside services/<name>/internal, or "" if it is not part of a layered service.
func layerOf(importPath string) string {
	rest, ok := strings.CutPrefix(importPath, module+"/services/")
	if !ok {
		return ""
	}
	_, after, ok := strings.Cut(rest, "/internal/")
	if !ok {
		return ""
	}
	layer, _, _ := strings.Cut(after, "/")
	return layer
}

func TestDependencyRule(t *testing.T) {
	for _, p := range listPackages(t) {
		layer := layerOf(p.ImportPath)
		if layer == "" {
			continue
		}
		for _, imp := range p.Imports {
			target := layerOf(imp)
			switch layer {
			case "domain":
				if target == "app" || target == "adapters" || target == "config" {
					t.Errorf("%s (domain) must not import %s (%s)", p.ImportPath, imp, target)
				}
				for _, bad := range infrastructure {
					if strings.HasPrefix(imp, bad) {
						t.Errorf("%s (domain) must not import infrastructure package %s", p.ImportPath, imp)
					}
				}
			case "app":
				if target == "adapters" {
					t.Errorf("%s (app) must not import adapter %s", p.ImportPath, imp)
				}
			}
		}
	}
}
