package knowledge

import (
	"encoding/json"
	"net/http"
	"testing"

	"hify/internal/user"
)

// extraction_budget_ops_test.go 守额度追加的记录与上限（010 T030）。
//
// ⭐ 追加额度是一次**花钱的决定**，必须留痕：追加过多少、追加了几次。
// ⚠️ 不记的话，最终成本里有一部分无法解释——报告只能说"总共花了 N"，
// 而说不出"其中 M 是中途追加的"。而追加恰恰是最需要复盘的动作：
// 一次实验追加了五轮额度，本身就说明第一次的预算估计错了。

func enableFor(t *testing.T, svc Service, kb, doc, key string) {
	t.Helper()
	if code, body := doExtraction(t, svc, http.MethodPost, kb, doc, "enable",
		`{"model_id":"m3","idempotency_key":"`+key+`"}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("enable 失败：%d %s", code, body)
	}
}

func budgetOps(t *testing.T, repo *Repository, docID string) []map[string]any {
	t.Helper()
	var raw []byte
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT CAST(budget_operations AS CHAR) FROM relation_extraction_jobs
		 WHERE document_id = ? ORDER BY run_number DESC LIMIT 1`, docID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		return nil
	}
	var ops []map[string]any
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatalf("budget_operations 不是数组：%v (%s)", err, raw)
	}
	return ops
}

// TestBudgetTopUpIsRecorded——⭐ 每一次追加都留痕。
func TestBudgetTopUpIsRecorded(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-b1")
	enableFor(t, svc, kb, "doc-b1", "k-enable")

	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-b1", "resume",
		`{"idempotency_key":"k-top1","additional_calls":500,"additional_active_seconds":600}`,
		"u1", user.RoleMember); code >= 400 {
		t.Fatalf("resume 追加失败：%d %s", code, body)
	}
	ops := budgetOps(t, repo, "doc-b1")
	if len(ops) != 1 {
		t.Fatalf("追加记录 %d 条, want 1：%v", len(ops), ops)
	}
	op := ops[0]
	if op["idempotency_key"] != "k-top1" {
		t.Errorf("没有记下幂等键：%v", op)
	}
	if op["additional_calls"] != float64(500) {
		t.Errorf("追加的调用额度记错了：%v", op["additional_calls"])
	}
	if op["by_user_id"] != "u1" {
		t.Errorf("没有记下是谁追加的：%v", op["by_user_id"])
	}
	// 额度本身也要真的加上去。
	var callLimit int
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT call_limit FROM relation_extraction_jobs WHERE document_id='doc-b1'`).
		Scan(&callLimit); err != nil {
		t.Fatal(err)
	}
	if callLimit != defaultCallLimit+500 {
		t.Errorf("call_limit = %d, want %d", callLimit, defaultCallLimit+500)
	}
}

// TestResumeDoesNotResetTheLedger——⭐ resume **不重置**已经花掉的账目。
//
// ⚠️ 重置的话，一次实验的成本会变成"最后一段的成本"，而前面几段的钱
// 凭空消失——而追加额度本来就意味着前面已经花掉了不少。
func TestResumeDoesNotResetTheLedger(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	kb, svc := extractionFixture(t, repo, "doc-b2")
	enableFor(t, svc, kb, "doc-b2", "k-enable")

	var jobID string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT id FROM relation_extraction_jobs WHERE document_id='doc-b2'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	// 手工造一点账目。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs
		 SET reserved_calls=7, confirmed_dispatches=5, unknown_attempts=2, active_ms_used=1234
		 WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-b2", "resume",
		`{"idempotency_key":"k-top","additional_calls":100}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("resume 失败：%d %s", code, body)
	}
	var reserved, confirmed, unknown int
	var activeMs int64
	if err := repo.db.QueryRowContext(ctx,
		`SELECT reserved_calls, confirmed_dispatches, unknown_attempts, active_ms_used
		 FROM relation_extraction_jobs WHERE id=?`, jobID).
		Scan(&reserved, &confirmed, &unknown, &activeMs); err != nil {
		t.Fatal(err)
	}
	if reserved != 7 || confirmed != 5 || unknown != 2 || activeMs != 1234 {
		t.Errorf("resume 重置了账目：reserved=%d confirmed=%d unknown=%d active_ms=%d",
			reserved, confirmed, unknown, activeMs)
	}
}

// TestRestartKeepsTheOldRunsLedger——⭐ restart 建新 run，**旧账目保留**。
//
// ⚠️ 删掉旧 run 的账目会让"这份文档一共花了多少"变成"最后一次跑花了多少"。
// 而 restart 往往正是因为前一次跑坏了——那次的钱是实实在在花掉的。
func TestRestartKeepsTheOldRunsLedger(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	kb, svc := extractionFixture(t, repo, "doc-b3")
	enableFor(t, svc, kb, "doc-b3", "k-enable")

	var oldJob string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT id FROM relation_extraction_jobs WHERE document_id='doc-b3'`).Scan(&oldJob); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET confirmed_dispatches=42, active_ms_used=9999 WHERE id=?`,
		oldJob); err != nil {
		t.Fatal(err)
	}
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-b3", "restart",
		`{"model_id":"m3","idempotency_key":"k-restart"}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("restart 失败：%d %s", code, body)
	}
	// 旧 run 还在，账目原样。
	var confirmed int
	var activeMs int64
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT confirmed_dispatches, active_ms_used, state FROM relation_extraction_jobs WHERE id=?`,
		oldJob).Scan(&confirmed, &activeMs, &state); err != nil {
		t.Fatal(err)
	}
	if confirmed != 42 || activeMs != 9999 {
		t.Errorf("旧 run 的账目被动了：confirmed=%d active_ms=%d", confirmed, activeMs)
	}
	if state != jobStateSuperseded {
		t.Errorf("旧 run 的状态 = %q, want superseded", state)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_jobs WHERE document_id='doc-b3'`); n != 2 {
		t.Errorf("作业 %d 个, want 2（旧 run 必须保留）", n)
	}
}

// TestControlOperationsAreCapped——⭐ 每个 run 最多 100 次控制操作。
//
// ⚠️ 上限存在的理由不是省空间：一个被追加了几十轮额度的 run，它的"预算"
// 已经和最初批准的那个数字没有关系了。到那个程度应该 restart 并重新
// 说明要花多少，而不是继续在同一个 run 上加。
func TestControlOperationsAreCapped(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-b4")
	enableFor(t, svc, kb, "doc-b4", "k-enable")

	for i := 0; i < maxBudgetOperations; i++ {
		code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-b4", "resume",
			`{"idempotency_key":"k`+itoa(i)+`","additional_calls":1}`, "u1", user.RoleMember)
		if code >= 400 {
			t.Fatalf("第 %d 次追加失败：%d %s", i, code, body)
		}
	}
	code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-b4", "resume",
		`{"idempotency_key":"k-over","additional_calls":1}`, "u1", user.RoleMember)
	if code != http.StatusConflict {
		t.Errorf("超过控制操作上限：code = %d (%s), want 409", code, body)
	}
}
