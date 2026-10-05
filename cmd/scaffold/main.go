// Command scaffold is the local verification CLI for the code generation
// engine. It never accesses the network: plans are read from a local file and
// all files are written under a local root directory.
//
// Usage:
//
//	scaffold plan-example [-o plan.json]
//	scaffold validate -f plan.json
//	scaffold run -f plan.json -root ./out [-workers 4]
//
// On success exit code is 0; validation, render, write and interruption
// failures use distinct non-zero exit codes and the full report (processing
// order, per-target status and digests) is printed as JSON.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/engine"
	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ce *engine.ClassifiedErr
		switch {
		case errors.As(err, &ce):
			os.Exit(exitForKind(ce.Kind))
		}
		os.Exit(10)
	}
}

func exitForKind(kind string) int {
	switch kind {
	case engine.KindInvalidPlan:
		return 2
	case engine.KindRender:
		return 3
	case engine.KindWrite, engine.KindManifest:
		return 4
	case engine.KindInterrupted:
		return 5
	case engine.KindPartial:
		return 6
	case engine.KindLocked:
		return 7
	default:
		return 10
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no subcommand given")
	}
	switch args[0] {
	case "plan-example":
		return cmdPlanExample(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "dry-run":
		return cmdDryRun(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `scaffold - local declarative code generation engine

Commands:
  plan-example [-o path]   write an example plan JSON to path (default stdout)
  validate   -f plan.json  validate a plan and print classified issues
  dry-run    -f plan.json -root DIR [-workers N]  simulate a run, write nothing
  status     -f plan.json -root DIR  show last run state and workspace drift
  run        -f plan.json -root DIR [-workers N] [-no-wait]  execute a plan

Exit codes: 0 ok, 2 invalid plan, 3 render error, 4 write/manifest error,
5 interrupted, 6 partial failure, 7 workspace locked (with -no-wait),
10 other error.
`)
}

func cmdPlanExample(args []string) error {
	fs := flag.NewFlagSet("plan-example", flag.ContinueOnError)
	out := fs.String("o", "", "output file (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := examplePlan()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	return os.WriteFile(*out, append(data, '\n'), 0o644)
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	file := fs.String("f", "plan.json", "plan file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := loadPlan(*file)
	if err != nil {
		return err
	}
	issues := plan.Validate(p)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if len(issues) > 0 {
		_ = enc.Encode(map[string]any{"valid": false, "issues": issues})
		return &engine.ClassifiedErr{
			Kind: engine.KindInvalidPlan, Message: "plan is invalid", Issues: issues,
		}
	}
	return enc.Encode(map[string]any{"valid": true})
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	file := fs.String("f", "plan.json", "plan file")
	root := fs.String("root", "out", "output root directory")
	workers := fs.Int("workers", 4, "max parallel workers")
	noWait := fs.Bool("no-wait", false, "fail with 'locked' instead of queueing when the workspace is busy")
	quiet := fs.Bool("quiet", false, "suppress the JSON report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := loadPlan(*file)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := engine.New(*root, engine.Options{Workers: *workers, NoWait: *noWait})
	report, err := e.Run(ctx, p)
	if report != nil && !*quiet {
		_ = writeReport(os.Stdout, report)
	}
	return err
}

func cmdDryRun(args []string) error {
	fs := flag.NewFlagSet("dry-run", flag.ContinueOnError)
	file := fs.String("f", "plan.json", "plan file")
	root := fs.String("root", "out", "output root directory")
	workers := fs.Int("workers", 4, "max parallel workers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := loadPlan(*file)
	if err != nil {
		return err
	}

	e := engine.New(*root, engine.Options{Workers: *workers})
	report, err := e.DryRun(context.Background(), p)
	if report != nil {
		_ = writeReport(os.Stdout, report)
	}
	return err
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	file := fs.String("f", "plan.json", "plan file")
	root := fs.String("root", "out", "output root directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := loadPlan(*file)
	if err != nil {
		return err
	}

	e := engine.New(*root, engine.Options{})
	report, err := e.Inspect(context.Background(), p)
	if report != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	}
	return err
}

func writeReport(w io.Writer, r *engine.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func loadPlan(path string) (*plan.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan %s: %w", path, err)
	}
	var p plan.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse plan %s: %w", path, err)
	}
	return &p, nil
}

func examplePlan() *plan.Plan {
	return &plan.Plan{
		Name:   "example",
		Params: map[string]string{"module": "example.com/myapp"},
		Targets: map[string]*plan.Target{
			"go-mod": {
				Path:     "go.mod",
				Template: "module {{.params.module}}\n\ngo 1.25\n",
			},
			"main": {
				Path:           "main.go",
				Template:       "package main\n\n// Generated for module {{.params.module}}\n// go.mod contents:\n// {{deps \"go-mod\"}}\nfunc main() {}\n",
				Deps:           []string{"go-mod"},
				RequiredParams: []string{"module"},
			},
			"readme": {
				Path:     "README.md",
				Template: "# {{.params.module}}\n\nGenerated by scaffold.\n",
			},
		},
	}
}
