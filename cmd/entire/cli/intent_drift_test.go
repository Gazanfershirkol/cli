package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apicheckpoint "github.com/entireio/cli/api/checkpoint"
)

func TestAnalyzeIntentDriftCompleteContextWithSupportingEvidence(t *testing.T) {
	root := t.TempDir()
	writeIntentDriftFixture(t, root, "feature.go")
	writeIntentDriftFixture(t, root, "feature_test.go")
	report := analyzeIntentDrift(t.Context(), root, completeIntentDriftSummary(), completeIntentDriftMetadata(), availableIntentDriftGraph)
	if report.contextStatus != intentDriftComplete || report.verdict != intentDriftPass {
		t.Fatalf("got context=%s verdict=%s, want COMPLETE/PASS", report.contextStatus, report.verdict)
	}
	if report.filesFound != 1 || report.testsFound != 1 || len(report.symbols) != 1 || len(report.testFunctions) != 1 || !report.graph.available {
		t.Fatalf("expected source, test, and graph evidence: %#v", report)
	}
}

func TestAnalyzeIntentDriftCompleteContextWithOpenItemIsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeIntentDriftFixture(t, root, "feature.go")
	writeIntentDriftFixture(t, root, "feature_test.go")
	metadata := completeIntentDriftMetadata()
	metadata.Summary.OpenItems = []string{"document migration"}
	report := analyzeIntentDrift(t.Context(), root, completeIntentDriftSummary(), metadata, availableIntentDriftGraph)
	if report.contextStatus != intentDriftComplete || report.verdict != intentDriftUnresolved {
		t.Fatalf("got context=%s verdict=%s, want COMPLETE/UNRESOLVED", report.contextStatus, report.verdict)
	}
}

func TestAnalyzeIntentDriftRedactedIntentIsPartialAndNonAuthoritative(t *testing.T) {
	metadata := completeIntentDriftMetadata()
	metadata.Summary.Intent = "[REDACTED]"
	report := analyzeIntentDrift(t.Context(), t.TempDir(), completeIntentDriftSummary(), metadata, availableIntentDriftGraph)
	if report.contextStatus != intentDriftMissing || report.verdict != intentDriftInsufficientContext {
		t.Fatalf("got context=%s verdict=%s, want MISSING/INSUFFICIENT_CONTEXT", report.contextStatus, report.verdict)
	}
}

func TestAnalyzeIntentDriftMissingTranscriptIsPartialAndNonAuthoritative(t *testing.T) {
	summary := completeIntentDriftSummary()
	summary.Sessions[0].Transcript = ""
	report := analyzeIntentDrift(t.Context(), t.TempDir(), summary, completeIntentDriftMetadata(), availableIntentDriftGraph)
	if report.contextStatus != intentDriftPartial || report.verdict != intentDriftInsufficientContext {
		t.Fatalf("got context=%s verdict=%s, want PARTIAL/INSUFFICIENT_CONTEXT", report.contextStatus, report.verdict)
	}
}

func TestAnalyzeIntentDriftWhitespaceOnlySessionReferencesArePartial(t *testing.T) {
	summary := completeIntentDriftSummary()
	summary.Sessions[0].Prompt = " \t"
	summary.Sessions[0].Transcript = "\n"
	report := analyzeIntentDrift(t.Context(), t.TempDir(), summary, completeIntentDriftMetadata(), availableIntentDriftGraph)
	if report.contextStatus != intentDriftPartial || report.verdict != intentDriftInsufficientContext {
		t.Fatalf("got context=%s verdict=%s, want PARTIAL/INSUFFICIENT_CONTEXT", report.contextStatus, report.verdict)
	}
	if len(report.unavailable) != 2 {
		t.Fatalf("got unavailable=%v, want both session references unavailable", report.unavailable)
	}
}

func TestAnalyzeIntentDriftMissingContextIsInsufficient(t *testing.T) {
	report := analyzeIntentDrift(t.Context(), t.TempDir(), &apicheckpoint.CheckpointSummary{}, &apicheckpoint.Metadata{}, availableIntentDriftGraph)
	if report.contextStatus != intentDriftMissing || report.verdict != intentDriftInsufficientContext {
		t.Fatalf("got context=%s verdict=%s, want MISSING/INSUFFICIENT_CONTEXT", report.contextStatus, report.verdict)
	}
}

func TestAnalyzeIntentDriftInconclusiveGraphEvidenceIsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeIntentDriftFixture(t, root, "feature.go")
	writeIntentDriftFixture(t, root, "feature_test.go")
	report := analyzeIntentDrift(t.Context(), root, completeIntentDriftSummary(), completeIntentDriftMetadata(), func(context.Context, string, []intentDriftSymbol) intentDriftGraphEvidence {
		return intentDriftGraphEvidence{detail: "unavailable"}
	})
	if report.contextStatus != intentDriftComplete || report.verdict != intentDriftUnresolved {
		t.Fatalf("got context=%s verdict=%s, want COMPLETE/UNRESOLVED", report.contextStatus, report.verdict)
	}
}

func TestAnalyzeIntentDriftFileExistenceWithoutSymbolOrTestIsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeIntentDriftContent(t, root, "feature.go", "package fixture\n")
	writeIntentDriftContent(t, root, "feature_test.go", "package fixture\n")
	report := analyzeIntentDrift(t.Context(), root, completeIntentDriftSummary(), completeIntentDriftMetadata(), availableIntentDriftGraph)
	if report.contextStatus != intentDriftComplete || report.verdict != intentDriftUnresolved {
		t.Fatalf("got context=%s verdict=%s, want COMPLETE/UNRESOLVED", report.contextStatus, report.verdict)
	}
	if len(report.symbols) != 0 || len(report.testFunctions) != 0 {
		t.Fatalf("file existence must not invent symbol or test evidence: %#v", report)
	}
}

func completeIntentDriftSummary() *apicheckpoint.CheckpointSummary {
	return &apicheckpoint.CheckpointSummary{
		FilesTouched: []string{"feature.go"},
		Sessions:     []apicheckpoint.SessionFilePaths{{Prompt: "prompt.txt", Transcript: "full.jsonl"}},
	}
}

func completeIntentDriftMetadata() *apicheckpoint.Metadata {
	return &apicheckpoint.Metadata{Summary: &apicheckpoint.Summary{
		Intent:  "Add a local feature",
		Outcome: "Feature implemented",
	}}
}

func availableIntentDriftGraph(context.Context, string, []intentDriftSymbol) intentDriftGraphEvidence {
	return intentDriftGraphEvidence{available: true, detail: "local graph inspected files"}
}

func writeIntentDriftFixture(t *testing.T, root, name string) {
	t.Helper()
	content := "package fixture\n\nfunc Feature() {}\n"
	if strings.HasSuffix(name, "_test.go") {
		content = "package fixture\n\nfunc TestFeature() {}\n"
	}
	writeIntentDriftContent(t, root, name, content)
}

func writeIntentDriftContent(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
