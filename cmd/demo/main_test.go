// main_test.go — happy-path assertions for the cmd/demo executable.
//
// The test runs `go run ./cmd/demo` as a subprocess (so the binary's actual
// stdout / exit code are exercised, not just internal calls) and asserts the
// five acceptance criteria from Task 3's spec:
//
//	1) Plan 最终状态 == Applied
//	2) AuditEvent 落地 ≥ 4 条 (Create + SubmitDraft + ReviewApprove + Apply)
//	3) 终端 stdout 含「已核对影响面」+「applied」字符串
//	4) Store.ListEvents 顺序按 timestamp 升序
//	5) `go run ./cmd/demo` 退出码 0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/store"
	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// TestDemoEndToEndHappyPath runs the demo as a subprocess and asserts the
// five acceptance criteria.
func TestDemoEndToEndHappyPath(t *testing.T) {
	// Resolve the project root. The test runs in cmd/demo/, so the project
	// root is two levels up (cmd/demo → cmd → projectRoot).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd))

	cmd := exec.Command("go", "run", "./cmd/demo")
	cmd.Dir = projectRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("demo run failed: %v\nstderr: %s", err, stderr.String())
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("demo exit code = %d, want 0\nstderr: %s", cmd.ProcessState.ExitCode(), stderr.String())
	}

	out := stdout.String()

	// (3) stdout assertions: must contain "已核对影响面" + "applied".
	if !strings.Contains(out, "已核对影响面") {
		t.Fatalf("stdout missing 已核对影响面; got:\n%s", out)
	}
	if !strings.Contains(out, "applied") {
		t.Fatalf("stdout missing applied; got:\n%s", out)
	}

	// (1)+(2)+(4): open the in-memory store via internal/store and re-run the
	// same flow against the persistence layer to validate the data plane. We
	// can't share state with the demo subprocess, so we use internal/store
	// directly: this is the contract test for the SQLite surface the demo
	// depends on.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	eng := workflow.NewEngine()
	eng.SetStore(st)
	specJSON, err := os.ReadFile(filepath.Join(projectRoot, "testdata", "platform_response.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec workflow.RulePlanSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	plan, err := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("submit draft: %v", err)
	}
	if _, err := eng.ReviewIssue(plan.PlanID, workflow.ActionApprove, "yang_jb", "已核对影响面"); err != nil {
		t.Fatalf("review approve: %v", err)
	}
	if _, err := eng.Apply(plan.PlanID, "yang_jb", "事务切换 HIS 字典表"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	final, err := st.GetPlan(context.Background(), plan.PlanID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	// (1) Plan 最终状态 == Applied
	if final.Status != workflow.StatusApplied {
		t.Fatalf("Plan final status = %s, want applied", final.Status)
	}

	events, err := st.ListEvents(context.Background(), plan.PlanID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	// (2) AuditEvent 落地 ≥ 4 条 (Create + SubmitDraft + ReviewApprove + Apply)
	if len(events) < 4 {
		t.Fatalf("expected ≥4 audit events, got %d", len(events))
	}

	// (4) Store.ListEvents 顺序按 timestamp 升序
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp.Before(events[i-1].Timestamp) {
			t.Fatalf("events not ordered at index %d: %v before %v", i, events[i].Timestamp, events[i-1].Timestamp)
		}
	}
}

// TestDemoProcessInvocation is a smoke test that the demo process exits 0
// without asserting internal state — useful as a CI gate when the heavy
// in-process assertions above are slow.
func TestDemoProcessInvocation(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd))
	cmd := exec.Command("go", "run", "./cmd/demo")
	cmd.Dir = projectRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("demo failed: %v\nstderr: %s\nstdout: %s", err, stderr.String(), stdout.String())
	}
	elapsed := time.Since(start)
	if elapsed > 60*time.Second {
		t.Fatalf("demo took too long: %v", elapsed)
	}
	if !strings.Contains(stdout.String(), "演示完成") {
		t.Fatalf("missing 演示完成 in stdout; got:\n%s", stdout.String())
	}
}
