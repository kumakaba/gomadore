package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/go-playground/validator/v10"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

var (
	Version    = "v1.0.1"           // VERSION_STR
	Revision   = "preview20251211b" // VERSION_STR
	Maintainer = "kumakaba"
)

// --- Configuration Struct ---
type Config struct {
	General struct {
		ListenAddr string `toml:"listen_addr" validate:"required"`
		ListenPort int    `toml:"listen_port" validate:"required"`
		LogLevel   string `toml:"log_level" validate:"omitempty,oneof=debug info error"`
		LogType    string `toml:"log_type" validate:"omitempty,oneof=text json"`
	} `toml:"general"`
	HTML struct {
		MarkdownRootDir string `toml:"markdown_rootdir" validate:"required"`
		SiteTitle       string `toml:"site_title"`
		SiteLang        string `toml:"site_lang"`
		SiteAuthor      string `toml:"site_author"`
		BaseCSSUrl      string `toml:"base_css_url"`
		ScreenCSSUrl    string `toml:"screen_css_url"`
		PrintCSSUrl     string `toml:"print_css_url"`
		StrictHtmlUrl   bool   `toml:"strict_html_url"`
	} `toml:"html"`
	Cache struct {
		HotReload     bool `toml:"hot_reload"`
		CacheLimit    int  `toml:"cache_limit"`
		MaxCacheItems int  `toml:"max_cache_items"`
	} `toml:"cache"`
}

// --- Cache Structs ---
type CacheItem struct {
	Content []byte
	Expires time.Time
}

type Cache struct {
	sync.RWMutex
	items map[string]CacheItem
}

// --- Server Struct ---
type Server struct {
	config Config
	cache  *Cache
	md     goldmark.Markdown
	tmpl   *template.Template
}

// Default HTML Template
const defaultHtmlTmpl = `<!DOCTYPE html>
<html lang="{{ .Language }}">
<head>
    <meta charset="UTF-8">
    <title>{{ .Title }}</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <link rel="stylesheet" href="{{ .BaseCSS }}">
    <link rel="stylesheet" href="{{ .ScreenCSS }}" media="screen">
    <link rel="stylesheet" href="{{ .PrintCSS }}" media="print">
</head>
<body id="{{ .Filename }}">
    <div class="container markdown-body">
        {{ .Body }}
    </div>
    <div class="author">{{ .Author }}</div>
</body>
</html>`

// MAIN =========================================

func main() {
	configPath := flag.String("c", "config.toml", "Path to configuration file")
	tmplPath := flag.String("h", "", "Path to HTML template file (optional)")
	listMode := flag.Bool("l", false, "List available URLs and exit")
	versionFlag := flag.Bool("v", false, "print the version and exit")
	flag.Parse()

	// Return Version and exit
	if *versionFlag {
		fmt.Printf("%s/gomadore (%s-%s)\n", Maintainer, Version, Revision)
		os.Exit(0)
	}

	// Load configuration
	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("Failed to load configuration file (%s): %v", *configPath, err)
	}

	// Setup Logger(slog)
	setupLogger(os.Stderr, cfg.General.LogLevel, cfg.General.LogType)

	if !*listMode {
		slog.Info("Setup gomadore", "version", Version, "revision", Revision)
	}

	// Validation
	validate := validator.New()
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		// get toml-tag
		name := strings.SplitN(fld.Tag.Get("toml"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	verr := validate.Struct(cfg)
	if verr != nil {
		slog.Error("Configuration validation failed", "config_path", *configPath, "err", verr)
		os.Exit(1)
	}

	// URL list mode
	if *listMode {
		if err := printURLList(cfg); err != nil {
			slog.Error("Failed to list URLs", "err", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if cfg.Cache.CacheLimit < 0 {
		cfg.Cache.CacheLimit = 0
	}
	if cfg.Cache.MaxCacheItems < 1 {
		cfg.Cache.MaxCacheItems = 1000
	}

	// Initialize server
	srv := &Server{
		config: cfg,
		cache:  &Cache{items: make(map[string]CacheItem)},
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM), // Enable GitHub Flavored Markdown
			goldmark.WithParserOptions(
				parser.WithAutoHeadingID(),
			),
		),
	}

	// Parse template
	var t *template.Template
	var err error

	if *tmplPath != "" {
		// Load from file if -h is provided
		tmplBytes, readErr := os.ReadFile(*tmplPath)
		if readErr != nil {
			slog.Error("Failed to read template file", "tmpl_path", *tmplPath, "err", readErr)
			os.Exit(1)
		}
		t, err = template.New("base").Parse(string(tmplBytes))
	} else {
		// Use default embedded template if not provided
		t, err = template.New("base").Parse(defaultHtmlTmpl)
	}

	if err != nil {
		slog.Error("Failed to parse template", "err", err)
		os.Exit(1)
	}
	srv.tmpl = t

	// Context for managing lifecycle of background goroutines (watcher, cleaner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start background cache cleaner (Garbage Collection)
	// Only start if CacheLimit is positive.
	// If CacheLimit <= 0, cache is treated as indefinite (never expires), so GC is not needed.
	if cfg.Cache.CacheLimit > 0 {
		// Set cleanup interval to half of the cache limit.
		// Enforce a minimum interval of 60 seconds to prevent excessive locking overhead.
		cleanupInterval := time.Duration(cfg.Cache.CacheLimit) * time.Second / 2
		if cleanupInterval < 60*time.Second {
			cleanupInterval = 60 * time.Second
		}
		go srv.startCacheCleaner(ctx, cleanupInterval)
	}

	// Setup Hot Reload if enabled
	if cfg.Cache.HotReload {
		go srv.watchFiles(ctx)
	}

	// HTTP Server setup
	mux := http.NewServeMux()
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /", srv.handleRequest)
	addr := fmt.Sprintf("%s:%d", cfg.General.ListenAddr, cfg.General.ListenPort)

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server
	go func() {
		slog.Info("Server starting", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server launch failed", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for signals
	quit := make(chan os.Signal, 1)
	// Monitor SIGINT (Ctrl+C) and SIGTERM (kill)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // Block until signal received
	slog.Info("Shutting down server...")

	// Shutdown with 5-second timeout
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()

	if err := httpSrv.Shutdown(sctx); err != nil {
		slog.Error("Server forced to shutdown", "err", err)
		os.Exit(1)
	}

	slog.Info("Server exiting")
}

// --- Logic to print available URLs ---
func printURLList(cfg Config) error {
	root := cfg.HTML.MarkdownRootDir

	// Check if root directory exists and is a directory
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("markdown root directory does not exist: %s", root)
		}
		return fmt.Errorf("accessing Markdown root directory: %v", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("markdown root is not a directory: %s", root)
	}

	host := cfg.General.ListenAddr
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	baseURL := fmt.Sprintf("http://%s:%d", host, cfg.General.ListenPort)

	// Slice to store URLs
	var urls []string

	// Walk through directory
	err = filepath.WalkDir(root, func(pathStr string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Process only files with .md extension
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			// Get relative path
			rel, err := filepath.Rel(root, pathStr)
			if err != nil {
				return nil
			}

			// Convert path separators
			urlPath := filepath.ToSlash(rel)

			// Remove extension
			urlPath = strings.TrimSuffix(urlPath, ".md")

			// Handle index files
			if !cfg.HTML.StrictHtmlUrl {
				if urlPath == "index" {
					urlPath = ""
				} else if strings.HasSuffix(urlPath, "/index") {
					urlPath = strings.TrimSuffix(urlPath, "index")
				}
			}

			// Construct full URL
			var fullURL string
			if urlPath == "" {
				fullURL = fmt.Sprintf("%s/", baseURL)
			} else {
				prefix := "/"
				if strings.HasPrefix(urlPath, "/") {
					prefix = ""
				}
				if cfg.HTML.StrictHtmlUrl {
					fullURL = fmt.Sprintf("%s%s%s.html", baseURL, prefix, urlPath)
				} else {
					fullURL = fmt.Sprintf("%s%s%s", baseURL, prefix, urlPath)
				}
			}

			// Add to list (do not print yet)
			urls = append(urls, fullURL)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("directory walk error: %v", err)
	}

	// Sort and print
	// Simple string sort ensures shorter paths (parent dir/index) come first
	slices.Sort(urls)

	for _, u := range urls {
		fmt.Println(u)
	}
	return nil
}

// --- Request Handler ---
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {

	// Security Check: URL Normalization
	// Use 'path' package for URL path manipulation, NOT 'filepath'.
	cleanedPath := path.Clean(r.URL.Path)

	// Preserve trailing slash if original request had it (and it's not root)
	if strings.HasSuffix(r.URL.Path, "/") && cleanedPath != "/" {
		cleanedPath += "/"
	}

	// If the cleaned path differs from the original (e.g. contained ".." or "//"),
	// redirect to the canonical path to prevent ACL bypass in upstream proxies (like nginx).
	if cleanedPath != r.URL.Path {
		http.Redirect(w, r, cleanedPath, http.StatusMovedPermanently)
		return
	}

	rawPath := r.URL.Path

	// If StrictHtmlUrl mode is enabled, only accept URLs ending in ".html"
	if s.config.HTML.StrictHtmlUrl {
		if !strings.HasSuffix(rawPath, ".html") {
			http.NotFound(w, r)
			return
		}
	}

	// If URL ends with slash, append "index" (directory support)
	if strings.HasSuffix(rawPath, "/") {
		rawPath += "index"
	}

	// Remove ".html" suffix if present
	rawPath = strings.TrimSuffix(rawPath, ".html")

	// Normalize path again for internal processing
	reqPath := path.Clean(rawPath)
	if reqPath == "." {
		reqPath = "/index"
	}
	filename := path.Base(reqPath)
	if filename == "" || filename == "." {
		filename = "default"
	}

	// Check cache
	s.cache.RLock()
	item, found := s.cache.items[reqPath]
	s.cache.RUnlock()

	// Determine if the cached item is valid.
	// If CacheLimit > 0, check the expiration time.
	// If CacheLimit <= 0, the cache never expires (valid until restart).
	isCacheValid := found
	if s.config.Cache.CacheLimit > 0 {
		isCacheValid = found && time.Now().Before(item.Expires)
	}

	// Return cached content if hit and valid
	if isCacheValid {
		w.Header().Set("X-Cache", "HIT")

		// Set browser cache (max-age)
		if s.config.Cache.CacheLimit > 0 {
			w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", s.config.Cache.CacheLimit))
		} else {
			// For indefinite server-side cache, instruct the browser to cache for a long duration (e.g., 1 day).
			w.Header().Set("Cache-Control", "max-age=86400")
		}

		if _, err := w.Write(item.Content); err != nil {
			slog.Debug("Failed to write response (cache hit)", "err", err)
		}
		return
	}

	// --- Markdown File Processing ---

	// Construct file system path
	// Use filepath.FromSlash to ensure compatibility with Windows if needed (though running in container usually implies Linux)
	staticPath := filepath.Join(s.config.HTML.MarkdownRootDir, filepath.FromSlash(reqPath))
	fullPath := staticPath + ".md"

	absRoot, err := filepath.Abs(s.config.HTML.MarkdownRootDir)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		slog.Info("Attack attempt detected", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		http.NotFound(w, r)
		return
	}

	// Check if file exists
	mdContent, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Markdown Processing: Parse -> Extract H1 -> Render

	// Parse to AST
	reader := text.NewReader(mdContent)
	doc := s.md.Parser().Parse(reader)

	// AST Traversal: Find the first H1
	var pageTitle string
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		// If node is Heading and Level is 1
		if h, ok := n.(*ast.Heading); ok && h.Level == 1 {
			pageTitle = string(h.Lines().Value(mdContent))
			break // Stop after finding the first one
		}
	}

	// Build title string
	finalTitle := s.config.HTML.SiteTitle
	if pageTitle != "" {
		finalTitle = fmt.Sprintf("%s - %s", pageTitle, finalTitle)
	}

	// Render to HTML
	var buf bytes.Buffer
	if err := s.md.Renderer().Render(&buf, mdContent, doc); err != nil {
		http.Error(w, "Markdown conversion failed", http.StatusInternalServerError)
		return
	}

	// Assemble HTML
	var finalHTML bytes.Buffer
	err = s.tmpl.Execute(&finalHTML, map[string]interface{}{
		"Title":     finalTitle,
		"Language":  s.config.HTML.SiteLang,
		"Author":    s.config.HTML.SiteAuthor,
		"Filename":  filename,
		"BaseCSS":   s.config.HTML.BaseCSSUrl,
		"ScreenCSS": s.config.HTML.ScreenCSSUrl,
		"PrintCSS":  s.config.HTML.PrintCSSUrl,
		"Body":      template.HTML(buf.String()),
	})
	if err != nil {
		http.Error(w, "Template execution failed", http.StatusInternalServerError)
		return
	}

	respBody := finalHTML.Bytes()

	// Save to cache
	s.cache.Lock()

	// Enforce Maximum Cache Items limit.
	// If the cache is full and we are adding a new item, evict one item to make space.
	// Note: We use random eviction (Go's map iteration is random) which is simple and effective enough.
	if s.config.Cache.MaxCacheItems > 0 && len(s.cache.items) >= s.config.Cache.MaxCacheItems {
		if _, exists := s.cache.items[reqPath]; !exists {
			for k := range s.cache.items {
				delete(s.cache.items, k)
				break // Delete one item and exit
			}
		}
	}

	s.cache.items[reqPath] = CacheItem{
		Content: respBody,
		Expires: time.Now().Add(time.Duration(s.config.Cache.CacheLimit) * time.Second),
	}
	s.cache.Unlock()

	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", s.config.Cache.CacheLimit))

	// Check for write errors
	if _, err := w.Write(respBody); err != nil {
		slog.Info("Failed to write response (fresh)", "err", err)
	}
}

// --- File Watcher (Hot Reload) ---

func (s *Server) watchFiles(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("Watcher error", "err", err)
		return
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			slog.Error("Failed to close watcher", "err", err)
		}
	}()

	// Function to add subdirectories recursively
	addWatchRecursive := func(root string) {
		err := filepath.WalkDir(root, func(pathStr string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			pathStr = filepath.ToSlash(filepath.Clean(pathStr))
			if d.IsDir() {
				if err := watcher.Add(pathStr); err != nil {
					slog.Error("Failed to add to watcher", "path", pathStr, "err", err)
				} else {
					slog.Debug("Watching dir", "path", pathStr)
				}
			}
			return nil
		})
		if err != nil {
			slog.Error("Directory walk error", "err", err)
		}
	}

	slog.Info("Hot Reload enabled: Initializing watcher...")
	addWatchRecursive(s.config.HTML.MarkdownRootDir)

	var debounceTimer *time.Timer
	const debounceDuration = 100 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			slog.Info("Stopping file watcher...")
			return

		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			filename := filepath.Base(event.Name)
			if strings.HasPrefix(filename, ".") || strings.HasSuffix(filename, "~") {
				continue
			}

			if event.Has(fsnotify.Create) {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					slog.Debug("New directory detected", "path", event.Name)
					addWatchRecursive(event.Name)
				}
			}

			shouldClear := false

			if strings.HasSuffix(event.Name, ".md") {
				shouldClear = true
			} else if event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				shouldClear = true
			}

			if shouldClear {

				if debounceTimer != nil {
					debounceTimer.Stop()
				}

				debounceTimer = time.AfterFunc(debounceDuration, func() {
					slog.Debug("File/Dir change detected. Clearing cache.", "path", event.Name, "event", event.Op)
					s.cache.Lock()
					clear(s.cache.items)
					s.cache.Unlock()
				})
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("Watcher error", "err", err)
		}
	}
}

// --- Cache Cleanup (Garbage Collection) ---

// startCacheCleaner runs a background ticker to remove expired cache items.
func (s *Server) startCacheCleaner(ctx context.Context, interval time.Duration) {

	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic recovered in startCacheCleaner", "err", r)
		}
	}()

	slog.Info("Cache GC started.", "interval_sec", int(interval.Seconds()))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// for Graceful Shutdown
			slog.Info("Cache GC stopping...")
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

// cleanup scans the cache map and removes expired items.
func (s *Server) cleanup() {

	// check clear target on RLock
	s.cache.RLock()
	now := time.Now()
	keysToDelete := make([]string, 0, 10)
	for key, item := range s.cache.items {
		if now.After(item.Expires) {
			keysToDelete = append(keysToDelete, key)
		}
	}
	s.cache.RUnlock()

	// delete cache on Lock
	if len(keysToDelete) > 0 {
		s.cache.Lock()
		count := 0
		for _, key := range keysToDelete {
			delete(s.cache.items, key)
			count++
		}
		s.cache.Unlock()

		if count > 0 {
			slog.Debug("Cache GC finished", "removed_count", count)
		}
	}
}

// --- Logger Setup ---
func setupLogger(w io.Writer, levelStr, typeStr string) {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	switch strings.ToLower(typeStr) {
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default:
		handler = slog.NewTextHandler(w, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
}
