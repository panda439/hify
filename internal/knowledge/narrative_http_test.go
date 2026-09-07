package knowledge

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"hify/internal/db/pggen"
	"hify/internal/platform/jwt"
)

// Actual HTTP + JWT + file storage + Redis enqueue + MySQL/PG publication.
// Embedding is deterministic. Processing invokes the same Service entry point
// used by the worker, without consuming other tests' shared Redis queue.
func TestNarrativeHTTPUploadToPublishedSource(t *testing.T) {
	repo := setupIntegration(t)
	svc := NewService(repo, newFakeProvider(), newTestAsynqClient(t), t.TempDir(), false, "", time.Second, false)
	seedKB(t, repo, "kb-narr-http", "m3", "u1", true)
	if _, err := repo.db.ExecContext(t.Context(), "UPDATE knowledge_bases SET chunk_size=40,chunk_overlap=8 WHERE id='kb-narr-http'"); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/api/v1"), NewHandler(svc), "narrative-test-secret")
	server := httptest.NewServer(router)
	defer server.Close()
	token, err := jwt.Issue("narrative-test-secret", jwt.Claims{UserID: "u1", Role: "member"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pdfPath := writeTestPDF(t, [][]testLine{{{Text: "Alpha walks toward the river and"}}, {{Text: "meets Beta by the river."}, {Text: "***"}, {Text: "Another scene starts here."}}})
	pdf, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{{"story.txt", []byte("第一章 初见\n甲。\n※\n乙。\n第二章 别离\n乙。")}, {"story.md", []byte("# 第一章 初见\n甲。\n```\n第二章 代码\n***\n```\n# 第二章 别离\n乙。")}, {"story.pdf", pdf}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, err := mw.CreateFormFile("file", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = fw.Write(tc.body); err != nil {
				t.Fatal(err)
			}
			if err = mw.WriteField("narrative_mode", "true"); err != nil {
				t.Fatal(err)
			}
			if err = mw.Close(); err != nil {
				t.Fatal(err)
			}
			url := server.URL + "/api/v1/knowledge-bases/kb-narr-http/documents"
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, &buf)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 200 {
				t.Fatalf("upload status=%d body=%s", res.StatusCode, raw)
			}
			var uploaded documentResponse
			if err = json.Unmarshal(raw, &uploaded); err != nil {
				t.Fatal(err)
			}
			if !uploaded.IsNarrative {
				t.Fatal("flag lost during HTTP upload")
			}
			if err = svc.ProcessDocument(t.Context(), uploaded.ID, uploaded.Version); err != nil {
				t.Fatal(err)
			}
			req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/"+uploaded.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			res, err = server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			var ready documentResponse
			if err = json.Unmarshal(raw, &ready); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 200 || ready.Status != StatusReady {
				t.Fatalf("not published: %s", raw)
			}
			rows, err := repo.pgQueries.ListPublishedNarrativeChunks(t.Context(), pggen.ListPublishedNarrativeChunksParams{DocumentID: uploaded.ID, DocumentVersion: uploaded.Version, Limit: 2000})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Fatal("no published sources")
			}
			source := strings.ReplaceAll(string(tc.body), "\r\n", "\n")
			if tc.name == "story.pdf" {
				parsed, err := parseFile(pdfPath, FileTypePDF)
				if err != nil {
					t.Fatal(err)
				}
				source, _ = narrativePDFSource(parsed.Pages)
			}
			normalized := []rune(source)
			for _, row := range rows {
				meta, err := decodeNarrativeMetadata(row.NarrativeMetadata)
				if err != nil || meta == nil {
					t.Fatalf("missing metadata: %v", err)
				}
				if err = validateNarrativeMetadata(*meta, len([]rune(row.Content))); err != nil {
					t.Fatal(err)
				}
				for _, seg := range meta.Segments {
					if seg.DocumentStart == nil {
						continue
					}
					if string(normalized[*seg.DocumentStart:*seg.DocumentEnd]) != string([]rune(row.Content)[seg.ChunkStart:seg.ChunkEnd]) {
						t.Fatal("published citation points at different text")
					}
				}
			}
		})
	}
}
