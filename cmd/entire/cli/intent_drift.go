package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	apicheckpoint "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

type intentDriftContextStatus string

const (
	intentDriftComplete intentDriftContextStatus = "COMPLETE"
	intentDriftPartial  intentDriftContextStatus = "PARTIAL"
	intentDriftMissing  intentDriftContextStatus = "MISSING"
)

type intentDriftVerdict string

const (
	intentDriftPass                intentDriftVerdict = "PASS"
	intentDriftUnresolved          intentDriftVerdict = "UNRESOLVED"
	intentDriftInsufficientContext intentDriftVerdict = "INSUFFICIENT_CONTEXT"
)

type intentDriftGraphEvidence struct {
	available bool
	detail    string
}

type intentDriftSymbol struct {
	path string
	line int
	name string
}

type intentDriftReport struct {
	contextStatus intentDriftContextStatus
	verdict       intentDriftVerdict
	intent        string
	available     []string
	unavailable   []string
	filesFound    int
	filesTotal    int
	testsFound    int
	openItems     int
	symbols       []intentDriftSymbol
	registrations []string
	testFunctions []string
	graph         intentDriftGraphEvidence
}

// intentDriftGraphRunner is a seam for tests. The production implementation
// invokes Entire Graph locally through this CLI's executable; it has no network
// or model-service boundary.
var intentDriftGraphRunner = runIntentDriftGraph

func newIntentDriftCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "intent-drift <checkpoint-id>",
		Short: "Compare checkpoint intent with local implementation evidence",
		Long: `Intent Drift compares local Entire Checkpoint context with local source, tests, and Entire Graph evidence.

It never sends raw prompts or transcripts to a service, and it never includes
raw prompt or transcript content in its report. Incomplete checkpoint context
is explicitly marked non-authoritative.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			checkpointID, err := id.NewCheckpointID(args[0])
			if err != nil {
				return fmt.Errorf("invalid checkpoint ID: %w", err)
			}
			repo, err := openRepository(cmd.Context())
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}
			defer repo.Close()
			stores, err := checkpoint.Open(cmd.Context(), repo, checkpoint.OpenOptions{})
			if err != nil {
				return fmt.Errorf("open local checkpoint store: %w", err)
			}
			summary, err := apicheckpoint.ReadCheckpoint(cmd.Context(), stores.Persistent, checkpointID)
			if err != nil {
				return fmt.Errorf("read local checkpoint: %w", err)
			}
			if len(summary.Sessions) == 0 {
				return fmt.Errorf("checkpoint %s has no sessions", checkpointID)
			}
			latest := len(summary.Sessions) - 1
			metadata, err := stores.Persistent.ReadSessionMetadata(cmd.Context(), checkpointID, latest)
			if err != nil {
				return fmt.Errorf("read local checkpoint metadata: %w", err)
			}
			root, err := paths.WorktreeRoot(cmd.Context())
			if err != nil {
				return fmt.Errorf("find worktree root: %w", err)
			}
			report := analyzeIntentDrift(cmd.Context(), root, summary, metadata, intentDriftGraphRunner)
			renderIntentDriftReport(cmd.OutOrStdout(), report)
			return nil
		},
	}
}

func analyzeIntentDrift(ctx context.Context, root string, summary *apicheckpoint.CheckpointSummary, metadata *apicheckpoint.Metadata, graphRunner func(context.Context, string, []intentDriftSymbol) intentDriftGraphEvidence) intentDriftReport {
	report := intentDriftReport{contextStatus: intentDriftMissing, verdict: intentDriftInsufficientContext}
	if summary == nil || metadata == nil || metadata.Summary == nil {
		report.unavailable = append(report.unavailable, "checkpoint summary intent and outcome")
		return report
	}
	intent := strings.TrimSpace(metadata.Summary.Intent)
	if intent == "" || isRedactedIntentDriftValue(intent) {
		report.unavailable = append(report.unavailable, "checkpoint summary intent")
		return report
	}
	report.intent = intent
	report.available = append(report.available, "checkpoint summary intent")
	report.openItems = len(metadata.Summary.OpenItems)

	if len(summary.Sessions) == 0 {
		report.contextStatus = intentDriftPartial
		report.unavailable = append(report.unavailable, "checkpoint session references")
		return report
	}
	latestSession := summary.Sessions[len(summary.Sessions)-1]
	if strings.TrimSpace(metadata.Summary.Outcome) == "" || isRedactedIntentDriftValue(metadata.Summary.Outcome) {
		report.unavailable = append(report.unavailable, "checkpoint summary outcome")
	}
	if strings.TrimSpace(latestSession.Prompt) == "" {
		report.unavailable = append(report.unavailable, "stored prompt reference")
	}
	if strings.TrimSpace(latestSession.Transcript) == "" {
		report.unavailable = append(report.unavailable, "stored transcript reference")
	}
	if len(report.unavailable) == 0 {
		report.contextStatus = intentDriftComplete
	} else {
		report.contextStatus = intentDriftPartial
	}

	files := checkpointEvidenceFiles(summary, metadata)
	report.filesTotal = len(files)
	processedTests := make(map[string]bool)
	for _, name := range files {
		if !safeIntentDriftPath(root, name) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			report.filesFound++
			if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				symbols, registrations := inspectIntentDriftGoFile(path, name)
				report.symbols = append(report.symbols, symbols...)
				report.registrations = append(report.registrations, registrations...)
				testName := strings.TrimSuffix(name, ".go") + "_test.go"
				if !processedTests[testName] {
					processedTests[testName] = true
					testPath := filepath.Join(root, filepath.FromSlash(testName))
					if testInfo, testErr := os.Stat(testPath); testErr == nil && !testInfo.IsDir() {
						report.testsFound++
						report.testFunctions = append(report.testFunctions, inspectIntentDriftTestFunctions(testPath, testName)...)
					}
				}
			}
		}
	}
	if len(report.symbols) > 0 {
		report.graph = graphRunner(ctx, root, report.symbols)
	} else {
		report.graph.detail = "not run: no concrete Go symbol was found in checkpoint-touched files"
	}

	if report.contextStatus != intentDriftComplete {
		return report
	}
	if report.openItems > 0 || report.filesTotal == 0 || report.filesFound != report.filesTotal || len(report.symbols) == 0 || len(report.testFunctions) == 0 || !report.graph.available {
		report.verdict = intentDriftUnresolved
		return report
	}
	report.verdict = intentDriftPass
	return report
}

func checkpointEvidenceFiles(summary *apicheckpoint.CheckpointSummary, metadata *apicheckpoint.Metadata) []string {
	seen := make(map[string]bool)
	var files []string
	for _, name := range append(append([]string{}, summary.FilesTouched...), metadata.FilesTouched...) {
		name = filepath.ToSlash(strings.TrimSpace(name))
		if name != "" && !seen[name] {
			seen[name] = true
			files = append(files, name)
		}
	}
	return files
}

func safeIntentDriftPath(root, name string) bool {
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(name)))
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var (
	intentDriftFunctionPattern = regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)
	intentDriftTestPattern     = regexp.MustCompile(`^\s*func\s+(Test[A-Za-z0-9_]*)\s*\(`)
)

// inspectIntentDriftGoFile captures declarations and conventional registrations
// without interpreting natural language. It deliberately reports only local,
// directly observable evidence.
func inspectIntentDriftGoFile(path, name string) ([]intentDriftSymbol, []string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var symbols []intentDriftSymbol
	var registrations []string
	for index, line := range strings.Split(string(content), "\n") {
		if matches := intentDriftFunctionPattern.FindStringSubmatch(line); len(matches) == 2 {
			symbols = append(symbols, intentDriftSymbol{path: name, line: index + 1, name: matches[1]})
		}
		if strings.Contains(line, "AddCommand(") || strings.Contains(line, "Handle(") || strings.Contains(line, "HandleFunc(") || strings.Contains(line, "Register(") {
			registrations = append(registrations, name+":"+strconv.Itoa(index+1))
		}
	}
	return symbols, registrations
}

func inspectIntentDriftTestFunctions(path, name string) []string {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var tests []string
	for _, line := range strings.Split(string(content), "\n") {
		if matches := intentDriftTestPattern.FindStringSubmatch(line); len(matches) == 2 {
			tests = append(tests, name+":"+matches[1])
		}
	}
	return tests
}

func isRedactedIntentDriftValue(value string) bool {
	upper := strings.ToUpper(strings.TrimSpace(value))
	return strings.Contains(upper, "[REDACTED]") || strings.Contains(upper, "<REDACTED>")
}

func runIntentDriftGraph(ctx context.Context, root string, symbols []intentDriftSymbol) intentDriftGraphEvidence {
	executable, err := os.Executable()
	if err != nil {
		return intentDriftGraphEvidence{detail: "unavailable: cannot locate Entire executable"}
	}
	symbol := symbols[0]
	target := symbol.path + ":" + strconv.Itoa(symbol.line)
	command := exec.CommandContext(ctx, executable, "graph", "impact", "--repo", root, "--symbol", target, "--profile", "fast", "--format", "text", "--depth", "1")
	output, err := command.Output()
	if err != nil {
		return intentDriftGraphEvidence{detail: "unavailable: local Entire Graph could not inspect " + symbol.name}
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Callers (") {
			return intentDriftGraphEvidence{available: true, detail: "local Entire Graph impact for " + symbol.name + ": " + strings.TrimSpace(line)}
		}
	}
	return intentDriftGraphEvidence{available: true, detail: "local Entire Graph impact inspected " + symbol.name}
}

func renderIntentDriftReport(w io.Writer, report intentDriftReport) {
	fmt.Fprintf(w, "Context: %s\nVerdict: %s\n\n", report.contextStatus, report.verdict)
	fmt.Fprintln(w, "Intent evidence:")
	if report.intent != "" {
		fmt.Fprintf(w, "  available: checkpoint summary intent: %s\n", report.intent)
	} else {
		fmt.Fprintln(w, "  available: none")
	}
	for _, unavailable := range report.unavailable {
		fmt.Fprintf(w, "  unavailable: %s\n", unavailable)
	}
	fmt.Fprintf(w, "  requirement candidates: 1 intent statement, %d explicit open item(s)\n\n", report.openItems)
	fmt.Fprintln(w, "Implementation evidence:")
	fmt.Fprintf(w, "  source: %d/%d checkpoint-touched file(s) present locally\n", report.filesFound, report.filesTotal)
	fmt.Fprintf(w, "  symbols: %d concrete Go declaration(s) found\n", len(report.symbols))
	fmt.Fprintf(w, "  registrations: %d conventional registration/handler call(s) found\n", len(report.registrations))
	fmt.Fprintf(w, "  tests: %d related Go test file(s), %d Test function(s) found\n", report.testsFound, len(report.testFunctions))
	for _, symbol := range report.symbols[:minIntentDrift(3, len(report.symbols))] {
		fmt.Fprintf(w, "  symbol: %s:%d %s\n", symbol.path, symbol.line, symbol.name)
	}
	for _, testName := range report.testFunctions[:minIntentDrift(3, len(report.testFunctions))] {
		fmt.Fprintf(w, "  test: %s\n", testName)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Graph evidence:")
	fmt.Fprintf(w, "  %s\n\n", report.graph.detail)
	if report.contextStatus != intentDriftComplete {
		fmt.Fprintln(w, "Why this is not authoritative:")
		fmt.Fprintln(w, "  checkpoint context is incomplete; no missing requirement was invented.")
	}
}

func minIntentDrift(left, right int) int {
	if left < right {
		return left
	}
	return right
}
