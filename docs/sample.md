# Gomadore Sample Page

This is a sample page to demonstrate **Markdown rendering capabilities**.

---

## 1. Typography & Formatting

You can use **bold text** for emphasis, *italic text* for nuance, and ~~strikethrough~~ for deleted items.

> **Note:** This is a blockquote. It is often used to highlight important information or warnings.
>
> You can also nest blockquotes.

## 2. External Images

Images hosted on external servers (like CDNs or GitHub user content) can be displayed directly.

![Go Gopher](https://go.dev/images/gophers/ladder.svg)

*The Go gopher was designed by Renee French. (Source: go.dev)*

## 3. Lists

### Unordered List
* Server-Side Rendering (SSR)
* Hot Reload support
* Security features
    * Directory traversal protection
    * Strict mode

### Ordered List
1.  Install Go
2.  Clone the repository
3.  Build the binary

### Task List (GFM)
- [x] Implement Basic Server
- [x] Add Caching mechanism
- [ ] Add Search functionality (Future)

## 4. Code Blocks

### Inline Code
You can run the server with `./gomadore` command.

### Fenced Code Block (Go)
Syntax highlighting is supported for code blocks.

```go
package main

import "fmt"

func main() {
    message := "Hello, Gomadore!"
    fmt.Println(message)
}
```

### Fenced Code Block (JSON)

```json
{
  "general": {
    "listen_port": 18085
  },
  "html": {
    "hot_reload": true
  }
}
```

## 5. Tables

Tables are useful for structured data.

| Feature | Status | Description |
| :--- | :---: | :--- |
| **SSR** | ✅ | Renders Markdown to HTML on the server. |
| **Cache** | ✅ | In-memory caching with TTL. |
| **Search** | ❌ | Not implemented yet. |

## 6. Links

* **Repository:** [GitHub - gomadore](https://github.com/kumakaba/gomadore)
* **Library:** [yuin/goldmark](https://github.com/yuin/goldmark)

---

## 7. Have a nice day !

