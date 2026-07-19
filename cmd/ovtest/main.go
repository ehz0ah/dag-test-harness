// Command ovtest runs the e2e cases on the dag kernel.
//
//	ovtest                              # run all cases
//	ovtest ov-memory                    # run one
//	ovtest --repeat 5 ov-memory         # N independent runs -> pass-rate
//	ovtest --out runs.jsonl ...         # append one JSON record per run
//	ovtest -list                        # list cases
//	ovtest report runs.jsonl [--case X] # pass-rate / attempts / latency / flake trends
//	ovtest release [all|case-id]         # frozen-source managed release gate
//	ovtest reset-local-state            # delete local runtime state, keep config
//	ovtest bootstrap-openviking-user    # recreate local test user key after reset
//
// Reads ARK_API_KEY + OVTEST_JUDGE_MODEL (or ARK_MODEL fallback) from the
// environment for the judge.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"code.byted.org/data-arch/ovtest/cases"
	"code.byted.org/data-arch/ovtest/internal/cli"
	"code.byted.org/data-arch/ovtest/internal/localstate"
	"code.byted.org/data-arch/ovtest/runner"
)

var resetLocalState = localstate.Reset
var bootstrapOpenVikingUser = cli.BootstrapVikingTestUser
var preflightOpenVikingUser = func() error {
	res, err := cli.PreflightOpenVikingUser()
	if err != nil {
		return err
	}
	if res.SkippedReason == "missing user config" {
		return fmt.Errorf("missing OpenViking user config")
	}
	return nil
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "reset-local-state" {
		os.Exit(runResetLocalState(args[1:]))
	}
	if len(args) > 0 && args[0] == "bootstrap-openviking-user" {
		os.Exit(runBootstrapOpenVikingUser(args[1:]))
	}
	if len(args) > 0 && args[0] == "report" {
		os.Exit(runReport(args[1:]))
	}
	if len(args) > 0 && args[0] == "release" {
		os.Exit(runRelease(args[1:]))
	}
	if len(args) == 1 && (args[0] == "-list" || args[0] == "--list") {
		for _, n := range cases.Names() {
			fmt.Println(n)
		}
		return
	}
	if len(args) == 1 && (args[0] == "-list-suites" || args[0] == "--list-suites") {
		for _, name := range cases.SuiteNames() {
			ids, _ := cases.Suite(name)
			fmt.Printf("%s\t%s\n", name, strings.Join(ids, ","))
		}
		return
	}
	os.Exit(run(args))
}

func parseArgs(args []string) (repeat int, out string, names []string, err error) {
	repeat = 1
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repeat":
			if i+1 >= len(args) {
				return 0, "", nil, fmt.Errorf("--repeat requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				return 0, "", nil, fmt.Errorf("--repeat must be a positive integer")
			}
			repeat = n
		case "--out":
			if i+1 >= len(args) {
				return 0, "", nil, fmt.Errorf("--out requires a value")
			}
			i++
			out = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return 0, "", nil, fmt.Errorf("unknown flag %q", args[i])
			}
			names = append(names, args[i])
		}
	}
	return
}

func run(args []string) int {
	defer cli.Cleanup()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	repeat, out, names, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if len(names) == 0 || (len(names) == 1 && names[0] == "all") {
		names = cases.DefaultNames()
	} else if len(names) == 1 && names[0] == "smoke" {
		names = cases.SmokeNames()
	}
	anyFail := false
	selected := make([]runner.Case, 0, len(names))
	for _, n := range names {
		c, ok := cases.All[n]
		if !ok {
			fmt.Printf("unknown case %q (try -list)\n", n)
			anyFail = true
			continue
		}
		selected = append(selected, c)
	}
	if out != "" && len(selected) > 0 {
		if err := ensureRecordWritable(out); err != nil {
			fmt.Fprintf(os.Stderr, "could not write record %q: %v\n", out, err)
			return 1
		}
	}
	if needsOpenVikingPreflight(selected) {
		if err := preflightOpenVikingUser(); err != nil {
			fmt.Fprintf(os.Stderr, "OpenViking preflight: %v\n", err)
			return 1
		}
	}
	for _, c := range selected {
		passes := 0
		for i := 0; i < repeat; i++ {
			res := runner.RunCaseContext(ctx, c)
			runner.PrintResult(res)
			if res.Status == runner.StatusPass {
				passes++
			} else {
				anyFail = true
			}
			if out != "" {
				if err := appendRecord(out, res); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not write record: %v\n", err)
					anyFail = true
				}
			}
		}
		if repeat > 1 {
			fmt.Printf("\n### %s: pass-rate %d/%d\n", c.ID, passes, repeat)
		}
	}
	if len(selected) > 0 && strings.EqualFold(os.Getenv("OV_TEST_CLEANUP_MODE"), "api") {
		cleanupIO, cleanupErr := runner.CleanupViking()
		runner.PrintCleanupResult(cleanupIO, cleanupErr)
		if cleanupErr != "" {
			anyFail = true
		}
	}
	if anyFail {
		return 1
	}
	return 0
}

func needsOpenVikingPreflight(selected []runner.Case) bool {
	for _, c := range selected {
		id := strings.ToLower(c.ID)
		if strings.HasPrefix(id, "ov-") ||
			strings.HasPrefix(id, "openclaw-") ||
			strings.HasPrefix(id, "hermes-openviking-") ||
			strings.HasPrefix(id, "experiment-ov-") ||
			strings.Contains(id, "-openviking-") {
			return true
		}
	}
	return false
}

func runResetLocalState(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "reset-local-state accepts no arguments")
		return 2
	}
	removed, err := resetLocalState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reset-local-state: %v\n", err)
		return 1
	}
	fmt.Printf("reset-local-state: removed %d path(s)\n", len(removed))
	for _, path := range removed {
		fmt.Println("  " + path)
	}
	return 0
}

func runBootstrapOpenVikingUser(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "bootstrap-openviking-user accepts no arguments")
		return 2
	}
	res, err := bootstrapOpenVikingUser()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap-openviking-user: %v\n", err)
		return 1
	}
	fmt.Printf("bootstrap-openviking-user: account=%s user=%s user_key_length=%d\n",
		res.AccountID, res.UserID, res.UserKeyLength)
	for _, path := range res.UpdatedPaths {
		fmt.Println("  updated " + path)
	}
	return 0
}

func ensureRecordWritable(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func appendRecord(path string, res runner.Result) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(runner.ResultRecord(res))
}

func runReport(args []string) int {
	only, path, err := parseReportArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	loaded, err := runner.LoadRecordsWithStats(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not read records %q: %v\n", path, err)
		return 1
	}
	records := loaded.Records
	if loaded.CorruptRows > 0 {
		fmt.Fprintf(os.Stderr, "warning: ignored %d corrupt JSONL row(s) in %q\n", loaded.CorruptRows, path)
	}
	if loaded.InvalidRows > 0 {
		fmt.Fprintf(os.Stderr, "warning: ignored %d invalid JSONL record row(s) in %q\n", loaded.InvalidRows, path)
	}
	if len(records) == 0 {
		if ignored := ignoredRowsSummary(loaded); ignored != "" {
			fmt.Printf("no valid records in %q (%s ignored)\n", path, ignored)
		} else {
			fmt.Printf("no records in %q\n", path)
		}
		return 1
	}
	if !reportHasCase(records, only) {
		fmt.Printf("no records for case %q\n", only)
		return 1
	}
	fmt.Println(runner.Report(records, only))
	return 0
}

func ignoredRowsSummary(loaded runner.LoadRecordStats) string {
	var parts []string
	if loaded.CorruptRows > 0 {
		parts = append(parts, fmt.Sprintf("%d corrupt JSONL row(s)", loaded.CorruptRows))
	}
	if loaded.InvalidRows > 0 {
		parts = append(parts, fmt.Sprintf("%d invalid JSONL record row(s)", loaded.InvalidRows))
	}
	return strings.Join(parts, ", ")
}

func reportHasCase(records []map[string]any, only string) bool {
	if only == "" {
		return true
	}
	for _, r := range records {
		if id, _ := r["id"].(string); id == only {
			return true
		}
	}
	return false
}

func parseReportArgs(args []string) (only string, path string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--case" {
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--case requires a value")
			}
			i++
			only = args[i]
		} else {
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown flag %q", args[i])
			}
			if path != "" {
				return "", "", fmt.Errorf("report accepts one runs.jsonl path")
			}
			path = args[i]
		}
	}
	if path == "" {
		return "", "", fmt.Errorf("usage: ovtest report <runs.jsonl> [--case <id>]")
	}
	return only, path, nil
}
