package knowledge

import (
	"context"
	"errors"
	"testing"
)

// 002-metadata-filter：RetrieveFilter 的纯函数行为（判空与校验）与 Retrieve
// 入口的开关闸门。这个文件刻意不碰数据库——判空/校验是纯函数，闸门也发生在
// 任何数据库调用之前，三者都能在零依赖下断言。真实 PostgreSQL 的下推行为在
// integration_test.go。

func intPtr(v int) *int { return &v }

func TestRetrieveFilterIsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		filter RetrieveFilter
		want   bool
	}{
		{"零值", RetrieveFilter{}, true},
		// 长度为 0 的非 nil 切片必须与 nil 同样被判为空——调用方从一个空的
		// 请求体里构造过滤器时拿到的正是它，如果这里判成"非空"，一个什么都
		// 没限定的请求会被当成限定，进而在开关关闭时报错。
		{"空的非 nil 切片", RetrieveFilter{DocumentIDs: []string{}}, true},
		{"有文档 ID", RetrieveFilter{DocumentIDs: []string{"doc-1"}}, false},
		{"只有页码下界", RetrieveFilter{PageMin: intPtr(3)}, false},
		{"只有页码上界", RetrieveFilter{PageMax: intPtr(9)}, false},
		{"两者都有", RetrieveFilter{DocumentIDs: []string{"doc-1"}, PageMin: intPtr(1)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.IsEmpty(); got != tc.want {
				t.Fatalf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetrieveFilterValidate(t *testing.T) {
	tooMany := make([]string, maxFilterDocumentIDs+1)
	for i := range tooMany {
		tooMany[i] = "doc"
	}
	exactlyMax := make([]string, maxFilterDocumentIDs)
	for i := range exactlyMax {
		exactlyMax[i] = "doc"
	}

	cases := []struct {
		name    string
		filter  RetrieveFilter
		wantErr error
	}{
		{"空过滤器永远合法", RetrieveFilter{}, nil},
		{"恰好等于上限", RetrieveFilter{DocumentIDs: exactlyMax}, nil},
		{"超出上限", RetrieveFilter{DocumentIDs: tooMany}, ErrTooManyFilterDocuments},
		{"合法闭区间", RetrieveFilter{PageMin: intPtr(3), PageMax: intPtr(7)}, nil},
		{"单端下界", RetrieveFilter{PageMin: intPtr(3)}, nil},
		{"单端上界", RetrieveFilter{PageMax: intPtr(7)}, nil},
		{"起止页相同", RetrieveFilter{PageMin: intPtr(5), PageMax: intPtr(5)}, nil},
		// 页码是 1-indexed（parse.go 的 pdfPage.Number 从 1 开始），0 和负数
		// 不是"不限"而是无意义——真想表达"不限"的调用方该把字段留成 nil。
		{"页码为 0", RetrieveFilter{PageMin: intPtr(0)}, ErrInvalidPageRange},
		{"页码为负", RetrieveFilter{PageMax: intPtr(-1)}, ErrInvalidPageRange},
		{"起始页大于结束页", RetrieveFilter{PageMin: intPtr(9), PageMax: intPtr(2)}, ErrInvalidPageRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRetrieveFilterValidateRejectsRatherThanTruncates 是 FR-015 的核心断言：
// 超限必须是一个明确的失败，不允许"截断到前 50 个然后继续"。静默截断会让调用
// 方拿到一个它没有要求过的范围——比"报错"严重得多，因为它不可见。
func TestRetrieveFilterValidateRejectsRatherThanTruncates(t *testing.T) {
	ids := make([]string, maxFilterDocumentIDs+5)
	for i := range ids {
		ids[i] = "doc"
	}
	f := RetrieveFilter{DocumentIDs: ids}
	if err := f.Validate(); !errors.Is(err, ErrTooManyFilterDocuments) {
		t.Fatalf("Validate() = %v, want ErrTooManyFilterDocuments", err)
	}
	// Validate 是值接收者，且不得以任何方式改写调用方的切片。
	if len(f.DocumentIDs) != maxFilterDocumentIDs+5 {
		t.Fatalf("Validate 截断了调用方的过滤器：len = %d, want %d", len(f.DocumentIDs), maxFilterDocumentIDs+5)
	}
}

// TestRetrieveFilterDisabledRejectsNonEmptyFilterWithoutTouchingDB 锁定
// research.md R4 的判断：开关关闭 + 非空过滤器 = 明确报错，**不是**忽略过滤器
// 照常做无过滤检索。
//
// "没有发生任何数据库调用"用一个 repo 为 nil 的 service 来证明：Retrieve 只要
// 往下走到任何一次仓储调用（getKnowledgeBase 是第一个）就会因为空指针 panic。
// 测试正常返回错误即证明闸门在所有数据库调用之前就拦住了。
func TestRetrieveFilterDisabledRejectsNonEmptyFilterWithoutTouchingDB(t *testing.T) {
	s := &service{metadataFilterEnabled: false}
	opts := RetrieveOptions{Filter: RetrieveFilter{DocumentIDs: []string{"doc-1"}}}

	got, err := s.Retrieve(context.Background(), []string{"kb-1"}, "查询", 5, opts)
	if !errors.Is(err, ErrMetadataFilterDisabled) {
		t.Fatalf("Retrieve() err = %v, want ErrMetadataFilterDisabled", err)
	}
	if got != nil {
		t.Fatalf("开关关闭时必须不返回任何结果，got %d 条", len(got))
	}
}

// TestRetrieveFilterDisabledStillAllowsEmptyFilter：开关关闭时**空**过滤器
// 完全不受影响——这是 FR-006/FR-013"空过滤器等价于本功能上线前行为"的入口
// 保证。这里同样用 repo 为 nil 的 service：空 kbIDs 让 Retrieve 在闸门之后、
// 任何数据库调用之前就正常短路返回，若闸门错误地拦下了空过滤器，就会拿到
// ErrMetadataFilterDisabled 而不是 nil。
func TestRetrieveFilterDisabledStillAllowsEmptyFilter(t *testing.T) {
	s := &service{metadataFilterEnabled: false}

	got, err := s.Retrieve(context.Background(), nil, "查询", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("空过滤器在开关关闭时必须无错误，got %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %d 条", len(got))
	}
}

// TestRetrieveFilterEnabledValidatesBeforeTouchingDB：开关开启时，非法过滤器
// 同样必须在任何数据库调用之前被拒（repo 为 nil，走到仓储就会 panic）。
func TestRetrieveFilterEnabledValidatesBeforeTouchingDB(t *testing.T) {
	s := &service{metadataFilterEnabled: true}
	opts := RetrieveOptions{Filter: RetrieveFilter{PageMin: intPtr(9), PageMax: intPtr(2)}}

	if _, err := s.Retrieve(context.Background(), []string{"kb-1"}, "查询", 5, opts); !errors.Is(err, ErrInvalidPageRange) {
		t.Fatalf("Retrieve() err = %v, want ErrInvalidPageRange", err)
	}
}

// TestFilterToPGParamsNormalizesEmptySlice 锁定 repository.go 里那条容易被
// "简化"掉的归一化：不限定文档时必须传 nil 切片而不是空切片。sqlc 生成的代码
// 用 pq.Array 传参——nil 切片是 SQL NULL（谓词恒真短路 = 不过滤），空的非 nil
// 切片则是 '{}'，于是 document_id = ANY('{}') 恒为 FALSE，会把所有候选挡光。
// 两者在 Go 里 len() 都是 0，差别只在这一层显现。
func TestFilterToPGParamsNormalizesEmptySlice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []string
	}{
		{"nil 切片", nil},
		{"空的非 nil 切片", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, pageMin, pageMax := filterToPGParams(RetrieveFilter{DocumentIDs: tc.input})
			if ids != nil {
				t.Fatalf("不限定文档时必须传 nil（→ SQL NULL），got %#v", ids)
			}
			if pageMin.Valid || pageMax.Valid {
				t.Fatalf("未指定页码时两端都必须是 NULL，got min=%+v max=%+v", pageMin, pageMax)
			}
		})
	}

	ids, pageMin, pageMax := filterToPGParams(RetrieveFilter{
		DocumentIDs: []string{"doc-1", "doc-2"},
		PageMin:     intPtr(3),
		PageMax:     intPtr(7),
	})
	if len(ids) != 2 {
		t.Fatalf("DocumentIDs 未透传：%#v", ids)
	}
	if !pageMin.Valid || pageMin.Int32 != 3 || !pageMax.Valid || pageMax.Int32 != 7 {
		t.Fatalf("页码未透传：min=%+v max=%+v", pageMin, pageMax)
	}
}
