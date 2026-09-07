package knowledge

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// Frozen from commit 1f37c1d: 13,230 input cases, legacy content/order/page/title only.
func TestDefaultOffMatchesPre010Snapshot(t *testing.T) {
	texts := []string{"", "甲。\n\n乙。", "# 标题\n正文。\n\n## 子题\n后文。", "第一章　甲\r\n内容。\r\n\r\n重复。", "```txt\n# 假标题\n***\n```\n正文", "甲！乙？丙。丁！", "| a | b |\n|--|--|\n| c | d |", "  甲  \n \n 乙\n", "- 甲\n- 乙\n\n末尾", strings.Repeat("无标点的长句", 100)}
	rng := rand.New(rand.NewSource(010))
	alphabet := []rune("甲乙 abc\n\r。！？#*\t")
	for i := 0; i < 80; i++ {
		r := make([]rune, rng.Intn(120)+1)
		for j := range r {
			r[j] = alphabet[rng.Intn(len(alphabet))]
		}
		texts = append(texts, string(r))
	}
	f := sha256.New()
	enc := json.NewEncoder(f)
	n := 0
	type P struct {
		Content      string
		PageNumber   *int
		PageEnd      *int
		SectionTitle *string
	}
	for i, txt := range texts {
		for _, size := range []int{1, 2, 5, 10, 20, 50, 200} {
			for _, ov := range []int{-1, 0, 1, size / 2, size - 1, size, size + 1} {
				for _, ft := range []string{FileTypeTxt, FileTypeMD, FileTypePDF} {
					parsed := parsedContent{Text: txt}
					if ft == FileTypePDF {
						parsed = parsedContent{Pages: []pdfPage{{Number: 1, Text: txt}, {Number: 2, Text: "下一页内容。"}}}
					}
					p := chunkDocument(ft, parsed, size, ov, false)
					v := []P{}
					for _, x := range p {
						v = append(v, P{x.Content, x.PageNumber, x.PageEnd, x.SectionTitle})
					}
					enc.Encode(struct {
						Key    string
						Pieces []P
					}{fmt.Sprintf("%d/%s/%d/%d", i, ft, size, ov), v})
					n++
				}
			}
		}
	}
	t.Logf("compared input cases=%d", n)
	if got := fmt.Sprintf("%x", f.Sum(nil)); got != "2ec0b0722b2f2fb1f769ec3b145344c699acf4d4608249f9371680d4c156f3f4" {
		t.Fatalf("default-off snapshot differs from pre-010 commit 1f37c1d: %s", got)
	}
}
