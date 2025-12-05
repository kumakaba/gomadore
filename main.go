package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"sort"
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
	Version    = "v1.0.0"           // VERSION_STR
	Revision   = "preview20251205c" // VERSION_STR
	Maintainer = "kumakaba"
)

// --- Configuration Struct ---
type Config struct {
	General struct {
		ListenAddr string `toml:"listen_addr" validate:"required"`
		ListenPort int    `toml:"listen_port" validate:"required"`
	} `toml:"general"`
	HTML struct {
		MarkdownRootDir string `toml:"markdown_rootdir" validate:"required"`
		SiteTitle       string `toml:"site_title"`
		SiteLang        string `toml:"site_lang"`
		SiteAuthor      string `toml:"site_author"`
		BaseCSSUrl      string `toml:"base_css_url"`
		ScreenCSSUrl    string `toml:"screen_css_url"`
		PrintCSSUrl     string `toml:"print_css_url"`
		HotReload       bool   `toml:"hot_reload"`
		CacheLimit      int    `toml:"cache_limit"`
		StrictHtmlUrl   bool   `toml:"strict_html_url"`
	} `toml:"html"`
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
		log.Fatalf("Configuration validation failed (%s): %v", *configPath, verr)
	}

	// URL list mode
	if *listMode {
		printURLList(cfg)
		os.Exit(0)
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
			log.Fatalf("Failed to read template file (%s): %v", *tmplPath, readErr)
		}
		t, err = template.New("base").Parse(string(tmplBytes))
	} else {
		// Use default embedded template if not provided
		t, err = template.New("base").Parse(defaultHtmlTmpl)
	}

	if err != nil {
		log.Fatalf("Failed to parse template: %v", err)
	}
	srv.tmpl = t

	// Setup Hot Reload if enabled
	if cfg.HTML.HotReload {
		go srv.watchFiles()
	}

	// HTTP Server setup
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", srv.handleRequest)
	addr := fmt.Sprintf("%s:%d", cfg.General.ListenAddr, cfg.General.ListenPort)

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server
	go func() {
		log.Printf("Server starting at %s ...", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server launch failed: %v", err)
		}
	}()

	// Wait for signals
	quit := make(chan os.Signal, 1)
	// Monitor SIGINT (Ctrl+C) and SIGTERM (kill)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // Block until signal received
	log.Println("Shutting down server...")

	// Shutdown with 5-second timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exiting")
}

// --- Logic to print available URLs ---
func printURLList(cfg Config) {
	root := cfg.HTML.MarkdownRootDir
	host := cfg.General.ListenAddr
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	baseURL := fmt.Sprintf("http://%s:%d", host, cfg.General.ListenPort)

	// Slice to store URLs
	var urls []string

	// Walk through directory
	err := filepath.WalkDir(root, func(pathStr string, d fs.DirEntry, err error) error {
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
		log.Fatalf("Directory walk error: %v", err)
	}

	// Sort and print
	// Simple string sort ensures shorter paths (parent dir/index) come first
	sort.Strings(urls)

	for _, u := range urls {
		fmt.Println(u)
	}
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

	// Return cached content if hit and valid
	if found && time.Now().Before(item.Expires) {
		w.Header().Set("X-Cache", "HIT")
		// Set browser cache (max-age)
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", s.config.HTML.CacheLimit))

		if _, err := w.Write(item.Content); err != nil {
			log.Printf("Failed to write response (cache hit): %v", err)
		}
		return
	}

	// --- Markdown File Processing ---

	// Construct file system path
	// Use filepath.FromSlash to ensure compatibility with Windows if needed (though running in container usually implies Linux)
	staticPath := filepath.Join(s.config.HTML.MarkdownRootDir, filepath.FromSlash(reqPath))
	fullPath := staticPath + ".md"

	// Security Check: Prevent directory traversal (ensure path is inside root)
	absRoot, err := filepath.Abs(s.config.HTML.MarkdownRootDir)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}
	if !strings.HasPrefix(absPath, absRoot) {
		log.Printf("Attack attempt detected: %s", r.URL.Path)
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
		http.Error(w, "Internal Server Error", 500)
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
		http.Error(w, "Markdown conversion failed", 500)
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
		http.Error(w, "Template execution failed", 500)
		return
	}

	respBody := finalHTML.Bytes()

	// Save to cache
	s.cache.Lock()
	s.cache.items[reqPath] = CacheItem{
		Content: respBody,
		Expires: time.Now().Add(time.Duration(s.config.HTML.CacheLimit) * time.Second),
	}
	s.cache.Unlock()

	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", s.config.HTML.CacheLimit))

	// Check for write errors
	if _, err := w.Write(respBody); err != nil {
		log.Printf("Failed to write response (fresh): %v", err)
	}
}

// --- File Watcher (Hot Reload) ---
func (s *Server) watchFiles() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("Watcher error:", err)
		return
	}
	// Error handling within defer
	defer func() {
		if err := watcher.Close(); err != nil {
			log.Printf("Failed to close watcher: %v", err)
		}
	}()

	// Function to add subdirectories recursively
	addWatchRecursive := func(root string) {
		err := filepath.WalkDir(root, func(pathStr string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Add directory to watcher
				if err := watcher.Add(pathStr); err != nil {
					log.Printf("Failed to add to watcher: %s, %v", pathStr, err)
				} else {
					log.Printf("Watching dir: %s", pathStr)
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("Directory walk error: %v", err)
		}
	}
	log.Println("Hot Reload enabled: Initializing watcher...")
	addWatchRecursive(s.config.HTML.MarkdownRootDir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Monitor directory structure
			if event.Has(fsnotify.Create) {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					log.Printf("New directory detected: %s", event.Name)
					if err := watcher.Add(event.Name); err != nil {
						log.Printf("Failed to add watcher for new directory: %v", err)
					}
				}
			}
			// Ignore non-md files
			isMdFile := strings.HasSuffix(event.Name, ".md")

			// If written or created
			if (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) && isMdFile {
				log.Printf("File changed: %s. Clearing cache.", event.Name)

				// Clear entire cache
				s.cache.Lock()
				s.cache.items = make(map[string]CacheItem)
				s.cache.Unlock()
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("Watcher error:", err)
		}
	}
}
