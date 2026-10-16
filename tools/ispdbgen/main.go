// SPDX-License-Identifier: AGPL-3.0-or-later

// Command ispdbgen converts a checkout of Mozilla's ISPDB
// (github.com/thunderbird/autoconfig, MPL-2.0) into nought's built-in
// provider snapshot:
//
//	go run ./tools/ispdbgen /path/to/autoconfig > internal/autoconfig/ispdb.json.gz
package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pluja/nought/internal/autoconfig"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ispdbgen <autoconfig-checkout>")
		os.Exit(2)
	}
	files, err := filepath.Glob(filepath.Join(os.Args[1], "ispdb", "*.xml"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no ispdb/*.xml under %s\n", os.Args[1])
		os.Exit(1)
	}
	slices.Sort(files)
	var providers []autoconfig.Provider
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			fail(err)
		}
		p, err := autoconfig.ParseXML(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", filepath.Base(file), err)
			continue
		}
		if p.ID == "" {
			p.ID = strings.TrimSuffix(filepath.Base(file), ".xml")
		}
		if len(p.Domains) > 0 && len(p.IMAP) > 0 {
			providers = append(providers, p)
		}
	}
	zw := gzip.NewWriter(os.Stdout)
	enc := json.NewEncoder(zw)
	if err := enc.Encode(providers); err != nil {
		fail(err)
	}
	if err := zw.Close(); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "%d providers\n", len(providers))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
