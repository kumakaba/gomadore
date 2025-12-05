package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

// Helper to create a server instance for testing
func setupTestServer(t *testing.T) (*Server, string) {
	t.Helper() // Mark as test helper

	// Create temporary directory for testing
	tempDir := t.TempDir()

	// Create Markdown files for testing
	// file: /index.md
	createFile(t, tempDir, "index.md", "# Top Page\nHello World")
	// file: /about.md
	createFile(t, tempDir, "about.md", "# About\nThis is about page")

	// file: /sub/deep.md
	subDir := filepath.Join(tempDir, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Failed to create directory %s: %v", subDir, err)
	}
	createFile(t, tempDir, "sub/deep.md", "# Deep Page\nDeep content")

	// file: /t1/cococo.md (for attack scenario)
	t1Dir := filepath.Join(tempDir, "t1")
	if err := os.Mkdir(t1Dir, 0755); err != nil {
		t.Fatalf("Failed to create directory %s: %v", t1Dir, err)
	}
	createFile(t, tempDir, "t1/cococo.md", "# Target Page\nSecret content")

	// Initialize Server struct
	cfg := Config{}
	cfg.HTML.MarkdownRootDir = tempDir
	cfg.HTML.CacheLimit = 60
	cfg.HTML.StrictHtmlUrl = false // Set to false for testing (default behavior)

	tmpl, _ := template.New("base").Parse(`{{.Body}}`) // Simple template

	srv := &Server{
		config: cfg,
		cache:  &Cache{items: make(map[string]CacheItem)},
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		),
		tmpl: tmpl,
	}

	return srv, tempDir
}

func createFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	path := filepath.Join(dir, filename)
	err := os.WriteFile(path, []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create file %s: %v", filename, err)
	}
}

func TestHandleRequest(t *testing.T) {
	srv, _ := setupTestServer(t)

	tests := []struct {
		name           string
		requestPath    string
		wantStatusCode int
		wantLocation   string // Redirect location (expected)
	}{
		// --- Normal Cases ---
		{
			name:           "Normal: Index",
			requestPath:    "/",
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "Normal: About page",
			requestPath:    "/about",
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "Normal: Sub directory file",
			requestPath:    "/sub/deep",
			wantStatusCode: http.StatusOK,
		},

		// --- Verify Directory Traversal Protection ---
		{
			name:           "Security: Traversal via .. (ACL Bypass Attempt)",
			requestPath:    "/t1/t11/../cococo.html",    // Path that might be allowed as /t1/t11/ in Nginx
			wantStatusCode: http.StatusMovedPermanently, // 301 Redirect
			wantLocation:   "/t1/cococo.html",           // Redirect to normalized path
		},
		{
			name:           "Security: Traversal via .. (Standard)",
			requestPath:    "/sub/../about",
			wantStatusCode: http.StatusMovedPermanently,
			wantLocation:   "/about",
		},
		{
			name:           "Security: Traversal without extension",
			requestPath:    "/t1/dummy/../cococo",
			wantStatusCode: http.StatusMovedPermanently,
			wantLocation:   "/t1/cococo",
		},
		{
			name:           "Security: Double slash normalization",
			requestPath:    "/t1//cococo",
			wantStatusCode: http.StatusMovedPermanently,
			wantLocation:   "/t1/cococo",
		},

		// --- Error Cases ---
		{
			name:           "Error: Not Found",
			requestPath:    "/notfound",
			wantStatusCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.requestPath, nil)
			w := httptest.NewRecorder()

			srv.handleRequest(w, req)

			resp := w.Result()
			defer resp.Body.Close()

			// Check status code
			if resp.StatusCode != tt.wantStatusCode {
				t.Errorf("StatusCode mismatch: got %d, want %d", resp.StatusCode, tt.wantStatusCode)
			}

			// Check Location header on redirect
			if tt.wantStatusCode == http.StatusMovedPermanently {
				loc, _ := resp.Location()
				if loc.Path != tt.wantLocation {
					t.Errorf("Redirect Location mismatch: got %s, want %s", loc.Path, tt.wantLocation)
				}
			}
		})
	}
}
func TestCacheLogic(t *testing.T) {
	srv, rootDir := setupTestServer(t)
	createFile(t, rootDir, "cache.md", "# Cache Test")

	reqPath := "/cache"

	// First Request (Verify Cache Miss)
	t.Run("First Request (Cache Miss)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, reqPath, nil)
		w := httptest.NewRecorder()
		srv.handleRequest(w, req)

		if w.Result().Header.Get("X-Cache") != "MISS" {
			t.Errorf("Expected X-Cache: MISS, got %s", w.Result().Header.Get("X-Cache"))
		}
	})

	// Second Request (Verify Cache Hit)
	t.Run("Second Request (Cache Hit)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, reqPath, nil)
		w := httptest.NewRecorder()
		srv.handleRequest(w, req)

		if w.Result().Header.Get("X-Cache") != "HIT" {
			t.Errorf("Expected X-Cache: HIT, got %s", w.Result().Header.Get("X-Cache"))
		}
	})

	// Verify Cache Control Header
	t.Run("Cache Control Header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, reqPath, nil)
		w := httptest.NewRecorder()
		srv.handleRequest(w, req)

		expected := fmt.Sprintf("max-age=%d", srv.config.HTML.CacheLimit)
		if got := w.Result().Header.Get("Cache-Control"); got != expected {
			t.Errorf("Cache-Control: got %s, want %s", got, expected)
		}
	})
}

func TestPrintURLList(t *testing.T) {
	// Create directories and files for testing
	tempDir := t.TempDir()
	createFile(t, tempDir, "index.md", "")
	createFile(t, tempDir, "about.md", "")

	subDir := filepath.Join(tempDir, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Failed to create dir: %v", err)
	}
	createFile(t, tempDir, "sub/deep.md", "")

	// Basic configuration
	cfg := Config{}
	cfg.General.ListenAddr = "127.0.0.1"
	cfg.General.ListenPort = 8080
	cfg.HTML.MarkdownRootDir = tempDir

	// Subtest: StrictHtmlUrl = false (Default)
	t.Run("StrictHtmlUrl=false", func(t *testing.T) {
		cfg.HTML.StrictHtmlUrl = false

		output := captureStdout(t, func() {
			printURLList(cfg)
		})

		// Expected output (Sorted)
		expected := []string{
			"http://127.0.0.1:8080/",
			"http://127.0.0.1:8080/about",
			"http://127.0.0.1:8080/sub/deep",
		}

		validateOutput(t, output, expected)
	})

	// Subtest: StrictHtmlUrl = true
	t.Run("StrictHtmlUrl=true", func(t *testing.T) {
		cfg.HTML.StrictHtmlUrl = true

		output := captureStdout(t, func() {
			printURLList(cfg)
		})

		// Expected output (Index treated as index.html in Strict mode)
		expected := []string{
			"http://127.0.0.1:8080/about.html",
			"http://127.0.0.1:8080/index.html",
			"http://127.0.0.1:8080/sub/deep.html",
		}

		validateOutput(t, output, expected)
	})
}

// Helper function to capture stdout
func captureStdout(t *testing.T, f func()) string {
	t.Helper()

	// Backup existing Stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Execute function
	f()

	// Restore and read
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("Failed to capture stdout: %v", err)
	}
	return buf.String()
}

// Helper function to validate output content
func validateOutput(t *testing.T, gotRaw string, expected []string) {
	t.Helper()

	// Split by line and trim (remove trailing empty lines)
	lines := strings.Split(strings.TrimSpace(gotRaw), "\n")

	if len(lines) != len(expected) {
		t.Errorf("Output line count mismatch: got %d, want %d\nGot:\n%s", len(lines), len(expected), gotRaw)
		return
	}

	for i, line := range lines {
		if strings.TrimSpace(line) != expected[i] {
			t.Errorf("Line %d mismatch:\ngot:  %s\nwant: %s", i, line, expected[i])
		}
	}
}

func TestExternalTemplate(t *testing.T) {
	// Setup standard test server
	srv, dir := setupTestServer(t)

	// Create custom template file
	// (Simulate file specified by -h option)
	customTmplContent := `<!DOCTYPE html>
<html lang="{{ .Language }}">
<body>
    <div id="custom-layout-marker">
        <h1>Custom: {{ .Title }}</h1>
        <div class="content">{{ .Body }}</div>
        <footer>Powered by Gomadore</footer>
    </div>
</body>
</html>`

	tmplPath := filepath.Join(dir, "custom_layout.html")
	if err := os.WriteFile(tmplPath, []byte(customTmplContent), 0644); err != nil {
		t.Fatalf("Failed to create custom template: %v", err)
	}

	// Read and parse template from file
	// (Replicate logic from main.go -h option processing)
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("Failed to read template file: %v", err)
	}

	customTmpl, err := template.New("base").Parse(string(tmplBytes))
	if err != nil {
		t.Fatalf("Failed to parse custom template: %v", err)
	}

	// Replace server instance template
	srv.tmpl = customTmpl

	// Send request
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	srv.handleRequest(w, req)

	// Verify: Check if response contains custom template content
	respBody := w.Body.String()

	// Check for marker DIV
	if !strings.Contains(respBody, `<div id="custom-layout-marker">`) {
		t.Error("Response does not contain custom template structure")
	}

	// Check for footer
	if !strings.Contains(respBody, "<footer>Powered by Gomadore</footer>") {
		t.Error("Response does not contain custom footer")
	}

	// Check if markdown content is correctly embedded (index.md content: "Top Page")
	if !strings.Contains(respBody, "Top Page") {
		t.Error("Response does not contain markdown content")
	}
}

func TestHotReload(t *testing.T) {
	// Setup
	srv, dir := setupTestServer(t)

	// Enable HotReload
	srv.config.HTML.HotReload = true

	// Start Watcher in a separate goroutine
	// Note: Since the current implementation has no stop mechanism, it runs until test ends,
	// which is acceptable for a single test process.
	go srv.watchFiles()

	// Wait for Watcher to start (considering OS lag)
	time.Sleep(200 * time.Millisecond)

	// Preparation: Insert dummy data into cache
	targetPath := "/index"
	srv.cache.Lock()
	srv.cache.items[targetPath] = CacheItem{
		Content: []byte("Old Cache"),
		Expires: time.Now().Add(1 * time.Hour),
	}
	srv.cache.Unlock()

	// Verify: Cache exists
	srv.cache.RLock()
	if _, found := srv.cache.items[targetPath]; !found {
		t.Fatal("Precondition failed: Cache should exist")
	}
	srv.cache.RUnlock()

	// Action: Update file
	// Rewrite index.md content (Trigger fsnotify Write event)
	indexPath := filepath.Join(dir, "index.md")
	if err := os.WriteFile(indexPath, []byte("# Updated\nNew Content"), 0644); err != nil {
		t.Fatalf("Failed to update file: %v", err)
	}

	// Wait: Await event processing
	// Since filesystem events are asynchronous, we need to wait for processing to complete.
	// Strictly speaking, synchronization via channels is better, but Sleep is standard for simple implementations.
	// Longer time (e.g., 500ms) might be needed depending on the environment.
	time.Sleep(500 * time.Millisecond)

	// Verify: Check if cache is cleared
	srv.cache.RLock()
	count := len(srv.cache.items)
	srv.cache.RUnlock()

	if count != 0 {
		t.Errorf("HotReload failed: Cache should be cleared after file modification. Item count: %d", count)
	}
}
