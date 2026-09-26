// Command scaffold-engine generates multi-file project skeletons from a local
// declarative plan. Generation is idempotent, reproducible, concurrency-safe
// and recoverable after partial failure or interruption.
//
// Usage:
//
//	scaffold-engine run    --plan plan.json [--root .] [--workers 4] [--force] [--dry-run]
//	scaffold-engine status --plan plan.json [--root .]
//	scaffold-engine recover                [--root .]
//
// All processing is local; no network or credentials are used.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/xingjianluo/scaffold-codegen-engine-go/engine"
	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
	"github.com/xingjianluo/scaffold-codegen-engine-go/render"
	"github.com/xingjianluo/scaffold-codegen-engine-go/state"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "recover":
		err = cmdRecover(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `scaffold-engine — local declarative code/scaffold generator

usage:
  scaffold-engine run --plan plan.json [--root DIR] [--workers N] [--force] [--dry-run]
  scaffold-engine status --plan plan.json [--root DIR]
  scaffold-engine recover --root DIR
`)
}

type commonFlags struct {
	plan    string
	root    string
	workers int
	force   bool
	dryRun  bool
}

func parseCommon(name string, args []string) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cf := &commonFlags{root: ".", workers: 4}
	fs.StringVar(&cf.plan, "plan", "", "path to the declarative plan JSON file")
	fs.StringVar(&cf.root, "root", ".", "workspace root (outputs are created relative to it)")
	fs.IntVar(&cf.workers, "workers", 4, "maximum concurrent render tasks")
	fs.BoolVar(&cf.force, "force", false, "overwrite user-modified or foreign files instead of refusing")
	fs.BoolVar(&cf.dryRun, "dry-run", false, "classify and render without writing outputs")
	_ = fs.Parse(args)
	return fs, cf
}

// buildPipeline loads and validates the plan, parses templates and checks
// parameter references. All rejection happens before any output is produced.
func buildPipeline(cf *commonFlags) (*plan.Plan, *render.Renderer, *state.Store, error) {
	if cf.plan == "" {
		return nil, nil, nil, fmt.Errorf("--plan is required")
	}
	p, err := plan.Load(cf.plan)
	if err != nil {
		return nil, nil, nil, err
	}
	r, err := render.NewRenderer(p)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := r.CheckParams(); err != nil {
		return nil, nil, nil, err
	}
	st := state.NewStore(cf.root)
	return p, r, st, nil
}

func cmdRun(args []string) error {
	_, cf := parseCommon("run", args)
	p, r, st, err := buildPipeline(cf)
	if err != nil {
		return err
	}
	e := engine.New(p, st, r)
	rep, err := e.RunContext(context.Background(), engine.Options{
		Workers: cf.workers, Force: cf.force, DryRun: cf.dryRun,
	})
	if rep != nil {
		fmt.Printf("plan:        %s\n", rep.PlanName)
		fmt.Printf("order:       %v\n", rep.Order)
		for _, rr := range rep.Results {
			verb := "keep"
			switch {
			case rr.Skipped:
				verb = "skip"
			case rr.Written:
				verb = "write"
			case rr.Status == state.StatusUpToDate:
				verb = "keep"
			}
			fmt.Printf("  %-5s %-20s %-14s %s\n", verb, rr.Name, rr.Status, rr.Reason)
		}
		if rep.RecoveredFromInterruption > 0 {
			fmt.Printf("recovery:    replayed %d journal entries from an interrupted run\n",
				rep.RecoveredFromInterruption)
		}
		if len(rep.RolledBack) > 0 {
			fmt.Printf("rolled back: %v\n", rep.RolledBack)
		}
	}
	return err
}

func cmdStatus(args []string) error {
	_, cf := parseCommon("status", args)
	p, r, st, err := buildPipeline(cf)
	if err != nil {
		return err
	}
	e := engine.New(p, st, r)
	// Dry run computes the same classifications without touching disk.
	rep, err := e.RunContext(context.Background(), engine.Options{Workers: cf.workers, DryRun: true})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func cmdRecover(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	root := fs.String("root", ".", "workspace root")
	_ = fs.Parse(args)
	st := state.NewStore(*root)
	j, err := st.ReadJournal()
	if err != nil {
		return err
	}
	if j == nil {
		fmt.Println("no interrupted run found; workspace is clean")
		return nil
	}
	n, err := st.Recover(j)
	if err != nil {
		return fmt.Errorf("recovery incomplete: %w", err)
	}
	st.CleanupStaging()
	if err := st.RemoveJournal(); err != nil {
		return err
	}
	fmt.Printf("recovered %d entries; workspace restored to the last committed state\n", n)
	return nil
}
