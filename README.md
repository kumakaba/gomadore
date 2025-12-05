# gomadore

**gomadore** (GO MArkDOwn REnderer) is a lightweight, high-performance Markdown web server written in Go (1.25+).

> **Note:** "gomadore" stands for **Sesame Dressing**. ("goma" means sesame in Japanese)

It is designed to serve Markdown files as HTML on-the-fly (Server-Side Rendering). It is ideal for internal documentation, personal knowledge bases, or simple blogs, and is intended to run behind a reverse proxy like Nginx or Caddy.

## Features

* **Server-Side Rendering (SSR):** Converts Markdown to HTML dynamically using [goldmark](https://github.com/yuin/goldmark).
* **High Performance:** In-memory caching with configurable expiration time.
* **Hot Reload:** Automatically detects file changes (creation or modification) and invalidates the cache instantly.
* **Directory Support:**
    * Supports nested directories.
    * Automatic index resolution (`/foo/` -> serves `/foo/index.md`).
    * Clean URLs (serves `/foo.md` at `/foo`).
* **Strict Mode:** Optional strict URL handling (requires `.html` extension) for static site generator compatibility.
* **Customizable:**
    * Configurable via TOML.
    * Supports custom HTML templates.
    * Easy integration with Class-less CSS frameworks (e.g., Water.css, MVP.css).
* **Security:**
    * Built-in directory traversal protection.
    * Canonical redirect enforcement to prevent ACL bypass.
* **Graceful Shutdown:** Handles system signals (SIGINT, SIGTERM) for safe termination.

## Prerequisites

* **Go 1.25** or higher

## Installation

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/kumakaba/gomadore.git
    cd gomadore
    ```

2.  **Install dependencies:**
    ```bash
    go mod tidy
    ```

3.  **Build the binary:**
    ```bash
    go build -o gomadore
    ```

## Configuration

Create a `config.toml` file in the root directory.

```toml
[general]
listen_addr = "127.0.0.1"
listen_port = 18085

[html]
# Directory containing your Markdown files and assets
markdown_rootdir = "./docs"

# Site Metadata
site_title = "My Documentation"
site_lang = "en"
site_author = "John Doe"

# CSS Configuration (Class-less CSS recommended)
base_css_url = "https://cdn.jsdelivr.net/npm/water.css@2/out/water.css"
screen_css_url = "" # Optional custom CSS for screen
print_css_url = ""  # Optional custom CSS for print

# Hot Reload: Set true to watch file changes
hot_reload = true

# Cache expiration in seconds
cache_limit = 3600

# Strict HTML URL: If true, URLs must end with ".html"
strict_html_url = false
```

## Usage

### Basic Start
Run the server with the default configuration (`config.toml`):

```bash
./gomadore
```

### Command Line Options

```bash
# Specify config file
./gomadore -c /etc/gomadore/prod.toml

# Specify custom HTML template
./gomadore -h ./templates/layout.html

# List all available URLs (useful for static site generation or debugging)
./gomadore -l

# Print version info
./gomadore -v
```

## Directory Structure Example

Given `markdown_rootdir = "./docs"`:

```text
.
|-- gomadore (binary)
|-- config.toml
|-- docs/
|   |-- index.md          -> http://localhost:18085/
|   |-- about.md          -> http://localhost:18085/about
|   |-- project-a/
|         |-- index.md    -> http://localhost:18085/project-a/
|         |-- manual.md   -> http://localhost:18085/project-a/manual
|
|-- imgs/
     |-- static.png
     |-- static.jpg
```

## Custom Templates

If you want to change the HTML structure, create a template file (e.g., `template.html`). The following variables are available:

* `{{ .Title }}`: Page title (H1 + Site Title)
* `{{ .Body }}`: Rendered HTML content
* `{{ .Language }}`: Site language (from config)
* `{{ .Author }}`: Author name (from config)
* `{{ .BaseCSS }}`: Base CSS URL
* `{{ .ScreenCSS }}`: Screen CSS URL
* `{{ .PrintCSS }}`: Print CSS URL
* `{{ .Filename }}`: Current filename (useful for body ID)

## Nginx Configuration Example

To run `gomadore` behind Nginx:

```nginx
server {
    listen 80;
    server_name docs.example.com;

    # Serve static assets directly via Nginx
    root /var/lib/gomadore;

    location /imgs {
        root /var/lib/gomadore/imgs;
        try_files $uri =404;
        expires 1d;
    }

    location / {
        proxy_pass http://127.0.0.1:18085;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

## License

This project is licensed under the MIT License. See the [LICENSE](/LICENSE) file for details.
