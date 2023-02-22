package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var importChanges = map[string]string{
	"github.com/ipfs/bitswap":         "github.com/ipfs/go-libipfs/bitswap",
	"github.com/ipfs/go-ipfs-files":   "github.com/ipfs/go-libipfs/files",
	"github.com/ipfs/tar-utils":       "github.com/ipfs/go-libipfs/tar",
	"gihtub.com/ipfs/go-block-format": "github.com/ipfs/go-libipfs/blocks",
}

type pkgJSON struct {
	Dir            string
	GoFiles        []string
	IgnoredGoFiles []string
	TestGoFiles    []string
	CgoFiles       []string
}

func (p *pkgJSON) allSourceFiles() []string {
	var files []string
	lists := [][]string{p.GoFiles, p.IgnoredGoFiles, p.TestGoFiles, p.CgoFiles}
	for _, l := range lists {
		for _, f := range l {
			files = append(files, filepath.Join(p.Dir, f))
		}
	}
	return files
}

func updateImports(filePath string, dryRun bool) error {
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", filePath, err)
	}

	var fileChanged bool

	ast.Inspect(astFile, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			val := strings.Trim(x.Path.Value, `"`)
			if newVal, ok := importChanges[val]; ok {
				fmt.Printf("changing %s => %s in %s\n", x.Path.Value, newVal, filePath)
				if !dryRun {
					x.Path.Value = fmt.Sprintf(`"%s"`, newVal)
					fileChanged = true
				}
			}
		}
		return true
	})

	if !fileChanged {
		return nil
	}

	f, err := os.Create(filePath)
	if err != nil {
		return err
	}
	err = format.Node(f, fset, astFile)
	if err != nil {
		f.Close()
		return fmt.Errorf("formatting %q: %w", filePath, err)
	}
	err = f.Close()
	if err != nil {
		return fmt.Errorf("closing %q: %w", filePath, err)
	}

	return nil
}

func main() {
	dryrun := os.Getenv("DRYRUN") != ""

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		log.Fatalf("error running 'go list': %s\nstderr: %s", err, stderr)
	}

	dec := json.NewDecoder(stdout)

	for {
		var pkg pkgJSON
		err = dec.Decode(&pkg)
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Fatalf("error decoding JSON: %s", err)
		}
		for _, filePath := range pkg.allSourceFiles() {
			if err := updateImports(filePath, dryrun); err != nil {
				log.Fatalf("error updating file %q: %s", filePath, err)
			}
		}
	}
}
