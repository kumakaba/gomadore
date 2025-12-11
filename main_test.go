package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	cfg.Cache.CacheLimit = 60
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

		expected := fmt.Sprintf("max-age=%d", srv.config.Cache.CacheLimit)
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

		output, errout := captureOutput(t, func() {
			_ = printURLList(cfg)
		})

		// UnExpected errout
		unexpected := []string{
			"msg",
		}

		validateOutput(t, errout, unexpected, true)

		// Expected output (Sorted)
		expected := []string{
			"http://127.0.0.1:8080/",
			"http://127.0.0.1:8080/about",
			"http://127.0.0.1:8080/sub/deep",
		}

		validateOutput(t, output, expected, false)
	})

	// Subtest: StrictHtmlUrl = true
	t.Run("StrictHtmlUrl=true", func(t *testing.T) {
		cfg.HTML.StrictHtmlUrl = true

		output, _ := captureOutput(t, func() {
			_ = printURLList(cfg)
		})

		// Expected output (Index treated as index.html in Strict mode)
		expected := []string{
			"http://127.0.0.1:8080/about.html",
			"http://127.0.0.1:8080/index.html",
			"http://127.0.0.1:8080/sub/deep.html",
		}

		validateOutput(t, output, expected, false)
	})
}

// Helper function to capture stdout and stderr
func captureOutput(t *testing.T, f func()) (string, string) {
	t.Helper()

	// Backup existing
	oldStdout := os.Stdout
	oldStderr := os.Stderr

	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()

	os.Stdout = wOut
	os.Stderr = wErr

	// Execute function
	f()

	// Restore and read
	wOut.Close()
	wErr.Close()

	os.Stdout = oldStdout
	os.Stderr = oldStderr

	var bufOut, bufErr bytes.Buffer
	if _, err := io.Copy(&bufOut, rOut); err != nil {
		t.Fatalf("Failed to capture stdout: %v", err)
	}
	if _, err := io.Copy(&bufErr, rErr); err != nil {
		t.Fatalf("Failed to capture stderr: %v", err)
	}

	return bufOut.String(), bufErr.String()
}

// Helper function to validate output content
func validateOutput(t *testing.T, gotRaw string, expected []string, invert bool) {
	t.Helper()

	// Split by line and trim (remove trailing empty lines)
	lines := strings.Split(strings.TrimSpace(gotRaw), "\n")

	if invert {
		for i, line := range lines {
			for j, exp := range expected {
				// t.Errorf("line:%s exp: %s", line, exp)
				if strings.Contains(line, exp) {
					t.Errorf("Line %d:%d match:\ngot:  %s", i, j, line)
				}
			}
		}
	} else {
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
	srv.config.Cache.HotReload = true

	// Start Watcher in a separate goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.watchFiles(ctx)

	// Wait for Watcher to start (considering OS lag)
	time.Sleep(100 * time.Millisecond)

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
	time.Sleep(200 * time.Millisecond)

	// Verify: Check if cache is cleared
	srv.cache.RLock()
	count := len(srv.cache.items)
	srv.cache.RUnlock()

	if count != 0 {
		t.Errorf("HotReload failed: Cache should be cleared after file modification. Item count: %d", count)
	}
}

func TestCacheCleanup(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.cache.Lock()

	// Case 1: Expired item (1 hour ago)
	srv.cache.items["/expired"] = CacheItem{
		Content: []byte("expired data"),
		Expires: time.Now().Add(-1 * time.Hour),
	}
	// Case 2: Valid item (1 hour later)
	srv.cache.items["/valid"] = CacheItem{
		Content: []byte("valid data"),
		Expires: time.Now().Add(1 * time.Hour),
	}
	srv.cache.Unlock()

	// Execute cleanup manually
	srv.cleanup()

	// Verify
	srv.cache.RLock()
	defer srv.cache.RUnlock()

	// Expired item should be removed
	if _, ok := srv.cache.items["/expired"]; ok {
		t.Error("Expired item was not removed")
	}

	// Valid item should remain
	if _, ok := srv.cache.items["/valid"]; !ok {
		t.Error("Valid item was incorrectly removed")
	}
}

func TestCacheCleaner_Integration(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.cache.Lock()
	srv.cache.items["/auto-expired"] = CacheItem{
		Content: []byte("data"),
		Expires: time.Now().Add(-1 * time.Hour),
	}
	srv.cache.Unlock()

	// Start cleaner with a very short interval (e.g., 10ms) for testing
	// Note: We bypass the "minimum 60s" logic in main() by calling the method directly.
	go srv.startCacheCleaner(context.Background(), 10*time.Millisecond)

	// Wait for the cleaner to run (slightly longer than the interval)
	time.Sleep(50 * time.Millisecond)

	// Verify
	srv.cache.RLock()
	_, found := srv.cache.items["/auto-expired"]
	srv.cache.RUnlock()

	if found {
		t.Error("Background cleaner failed to remove expired item")
	}
}

func TestMaxCacheItems(t *testing.T) {
	srv, dir := setupTestServer(t)

	createFile(t, dir, "page1.md", "# Page 1")
	createFile(t, dir, "page2.md", "# Page 2")
	createFile(t, dir, "page3.md", "# Page 3")

	srv.config.Cache.MaxCacheItems = 2

	// Request page1 (Cache: 1/2)
	req1 := httptest.NewRequest("GET", "/page1", nil)
	srv.handleRequest(httptest.NewRecorder(), req1)

	// Request page2 (Cache: 2/2 -> Full)
	req2 := httptest.NewRequest("GET", "/page2", nil)
	srv.handleRequest(httptest.NewRecorder(), req2)

	srv.cache.RLock()
	if len(srv.cache.items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(srv.cache.items))
	}
	srv.cache.RUnlock()

	// Request page3 (Cache Overflow -> Should evict one old item)
	req3 := httptest.NewRequest("GET", "/page3", nil)
	srv.handleRequest(httptest.NewRecorder(), req3)

	// Verify results
	srv.cache.RLock()
	defer srv.cache.RUnlock()

	// Check count (Must stay at 2)
	if len(srv.cache.items) != 2 {
		t.Errorf("Cache size exceeded limit. Expected 2, got %d", len(srv.cache.items))
	}

	// Check if the new item is present
	if _, found := srv.cache.items["/page3"]; !found {
		t.Error("The newest item (/page3) should be in the cache")
	}

}

func TestPrintURLList_Error(t *testing.T) {
	tempDir := t.TempDir()

	// Case 1: Directory does not exist
	t.Run("Root Not Exist", func(t *testing.T) {
		cfg := Config{}
		cfg.HTML.MarkdownRootDir = filepath.Join(tempDir, "non_existent")

		err := printURLList(cfg)
		if err == nil {
			t.Error("Expected error for non-existent root, got nil")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("Unexpected error message: %v", err)
		}
	})

	// Case 2: Root is a file
	t.Run("Root is File", func(t *testing.T) {
		filePath := filepath.Join(tempDir, "file.txt")
		createFile(t, tempDir, "file.txt", "content")

		cfg := Config{}
		cfg.HTML.MarkdownRootDir = filePath

		err := printURLList(cfg)
		if err == nil {
			t.Error("Expected error for file root, got nil")
		}
		if !strings.Contains(err.Error(), "is not a directory") {
			t.Errorf("Unexpected error message: %v", err)
		}
	})
}

func TestGcCacheNeverExpires(t *testing.T) {
	srv, dir := setupTestServer(t)

	// Set CacheLimit to 0 => "never expires" mode in request logic
	srv.config.Cache.CacheLimit = 0

	createFile(t, dir, "never.md", "# Never expires")
	reqPath := "/never"

	// First request: should be MISS and populate cache
	w1 := httptest.NewRecorder()
	srv.handleRequest(w1, httptest.NewRequest("GET", reqPath, nil))
	if got := w1.Result().Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("precondition: expected first request X-Cache=MISS, got %q", got)
	}

	// Manually set Expires to the past to ensure expiration would normally remove it
	srv.cache.Lock()
	if item, ok := srv.cache.items[reqPath]; ok {
		item.Expires = time.Now().Add(-1 * time.Hour)
		srv.cache.items[reqPath] = item
	} else {
		srv.cache.Unlock()
		t.Fatal("precondition: cache item missing after first request")
	}
	srv.cache.Unlock()

	// Second request: Because CacheLimit == 0, handler should treat cached item as valid (HIT)
	w2 := httptest.NewRecorder()
	srv.handleRequest(w2, httptest.NewRequest("GET", reqPath, nil))
	if got := w2.Result().Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("expected X-Cache=HIT for never-expire mode, got %q", got)
	}
}

func TestGcCacheTTLBoundary(t *testing.T) {
	srv, dir := setupTestServer(t)

	srv.config.Cache.CacheLimit = 1 // seconds

	createFile(t, dir, "ttl.md", "# TTL test")
	reqPath := "/ttl"

	// First request to populate cache
	w1 := httptest.NewRecorder()
	srv.handleRequest(w1, httptest.NewRequest("GET", reqPath, nil))
	if got := w1.Result().Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("precondition: expected first request X-Cache=MISS, got %q", got)
	}

	// Shorten Expires to very near-future to create a tight boundary
	srv.cache.Lock()
	item, ok := srv.cache.items[reqPath]
	if !ok {
		srv.cache.Unlock()
		t.Fatal("precondition: cache item missing after first request")
	}
	item.Expires = time.Now().Add(200 * time.Millisecond)
	srv.cache.items[reqPath] = item
	srv.cache.Unlock()

	// Immediate request should be HIT
	w2 := httptest.NewRecorder()
	srv.handleRequest(w2, httptest.NewRequest("GET", reqPath, nil))
	if got := w2.Result().Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("expected immediate request X-Cache=HIT, got %q", got)
	}

	time.Sleep(300 * time.Millisecond)

	w3 := httptest.NewRecorder()
	srv.handleRequest(w3, httptest.NewRequest("GET", reqPath, nil))
	if got := w3.Result().Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("expected post-expiry request X-Cache=MISS, got %q", got)
	}
}

func TestGcConcurrentCacheAccess(t *testing.T) {
	srv, dir := setupTestServer(t)

	// Use a non-zero TTL so handler checks Expires path
	srv.config.Cache.CacheLimit = 60

	// Prepare multiple files with deterministic names
	for i := 0; i < 5; i++ {
		filename := fmt.Sprintf("concurrent_%d.md", i)
		createFile(t, dir, filename, "# concurrent")
	}

	var wg sync.WaitGroup
	reqs := []string{"/index", "/concurrent_0", "/concurrent_1", "/concurrent_2", "/concurrent_3"}
	// Fire many concurrent requests
	for i := 0; i < 50; i++ {
		for _, p := range reqs {
			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				w := httptest.NewRecorder()
				req := httptest.NewRequest("GET", path, nil)
				// Call handler; we assert it doesn't panic. Make sure to close response body.
				srv.handleRequest(w, req)
				resp := w.Result()
				if resp != nil && resp.Body != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}(p)
		}
	}
	wg.Wait()

	// Basic sanity: cache should have at least one item
	srv.cache.RLock()
	if len(srv.cache.items) == 0 {
		srv.cache.RUnlock()
		t.Fatal("expected cache to contain items after concurrent requests")
	}
	srv.cache.RUnlock()

	// Demonstrate correct integer->string conversion (if needed elsewhere)
	_ = strconv.Itoa(42)
}

func TestSetupLogger(t *testing.T) {
	tests := []struct {
		name        string
		level       string
		logType     string
		logFunc     func()
		wantContain string
		wantMissing string
	}{
		{
			name:    "Level Info (Debug should be hidden)",
			level:   "info",
			logType: "text",
			logFunc: func() {
				slog.Info("info message")
				slog.Debug("debug message")
			},
			wantContain: "msg=\"info message\"",
			wantMissing: "debug message",
		},
		{
			name:    "Level Debug (Debug should be shown)",
			level:   "debug",
			logType: "text",
			logFunc: func() {
				slog.Debug("debug message")
			},
			wantContain: "msg=\"debug message\"",
		},
		{
			name:    "Format JSON",
			level:   "info",
			logType: "json",
			logFunc: func() {
				slog.Info("json message", "key", "val")
			},
			wantContain: `"msg":"json message","key":"val"`,
		},
		{
			name:    "Invalid Level Fallback (Default to Info)",
			level:   "unknown_level",
			logType: "text",
			logFunc: func() {
				slog.Info("info message")
				slog.Debug("debug message")
			},
			wantContain: "msg=\"info message\"",
			wantMissing: "debug message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			setupLogger(&buf, tt.level, tt.logType)

			tt.logFunc()

			output := buf.String()

			if tt.wantContain != "" && !strings.Contains(output, tt.wantContain) {
				t.Errorf("Log output missing expected content.\nExpected to contain: %s\nGot:\n%s", tt.wantContain, output)
			}

			if tt.wantMissing != "" && strings.Contains(output, tt.wantMissing) {
				t.Errorf("Log output contained unexpected content.\nExpected NOT to contain: %s\nGot:\n%s", tt.wantMissing, output)
			}
		})
	}
}
