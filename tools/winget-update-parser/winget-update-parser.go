// Command winget-update-parser lists the Windows packages winget can upgrade
// (read-only, see internal/winget).
//
//	go run ./tools/winget-update-parser [-json] [-timeout 60s] [-source winget|msstore]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"emlyupdater/internal/winget"
)

func main() {
	asJSON := flag.Bool("json", false, "print the packages as JSON instead of a table")
	timeout := flag.Duration("timeout", 60*time.Second, "maximum time allowed for PowerShell")
	source := flag.String("source", "", "only show packages from this source (e.g. winget, msstore)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pkgs, err := winget.ListUpgradable(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	pkgs = winget.FilterBySource(pkgs, *source)

	if *asJSON {
		err = printJSON(os.Stdout, pkgs)
	} else {
		err = printTable(os.Stdout, pkgs)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printJSON(w io.Writer, pkgs []winget.Package) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(pkgs)
}

func printTable(w io.Writer, pkgs []winget.Package) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tINSTALLED\tAVAILABLE\tSOURCE")
	for _, p := range pkgs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.ID, p.InstalledVersion, p.Available, p.Source)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%d aggiornamenti disponibili\n", len(pkgs))
	return err
}
