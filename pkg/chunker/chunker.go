// Package chunker implements AST-aware source code chunking using tree-sitter.
//
// Strategy (ADR-0004): parse each source file with the appropriate grammar,
// walk the AST, and emit one chunk per top-level definition (function, method,
// type/class/struct, etc.). Files whose language has no grammar fall back to
// naive fixed-size line chunking.
//
// The output is a slice of Chunk structs carrying the text + metadata needed
// for embedding and indexing into Qdrant.
package chunker

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/css"
	"github.com/smacker/go-tree-sitter/dockerfile"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/html"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/lua"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/protobuf"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/swift"
	tstype "github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"
)

// Chunk is a single indexable unit of source code.
type Chunk struct {
	Content    string            // raw source text of the chunk
	SourceFile string            // absolute or project-relative path
	Language   string            // detected language ("go", "python", ...)
	Symbol     string            // node type (e.g. "function_declaration")
	Name       string            // best-effort symbol name (first identifier child)
	StartLine  int               // 1-based start line
	EndLine    int               // 1-based end line
	Meta       map[string]string // extra metadata merged into Qdrant payload
}

// Chunker splits source files into embeddable chunks.
type Chunker struct {
	MaxChunkLines  int // fallback chunk size for languages without a grammar (default 60)
	MaxChunkBytes  int // hard limit: chunks exceeding this are split by lines (default 6000 ≈ 1500 tokens)
}

// New creates a Chunker with sensible defaults.
// MaxChunkBytes: 4500 ≈ 1600 tokens (llama embedder limit is 2048 tokens;
// code averages ~2.8 bytes/token. 4500 stays safely under the limit).
func New() *Chunker {
	return &Chunker{MaxChunkLines: 60, MaxChunkBytes: 4500}
}

// languageInfo maps a language to its tree-sitter language + the AST node types
// we treat as top-level definition boundaries.
type languageInfo struct {
	lang   *sitter.Language
	name   string
	// nodeTypes that represent a coherent, self-contained code unit worth indexing.
	// Order matters: more specific first (e.g. method before function).
	defTypes []string
}

var _ = sitter.Language{} // keep import

// langByExt resolves a language by file extension.
func langByExt(ext string) (*languageInfo, bool) {
	switch strings.ToLower(ext) {
	// --- Core 8 (original) ---
	case ".go":
		return &languageInfo{lang: golang.GetLanguage(), name: "go", defTypes: []string{
			"function_declaration", "method_declaration", "type_declaration",
		}}, true
	case ".py":
		return &languageInfo{lang: python.GetLanguage(), name: "python", defTypes: []string{
			"function_definition", "class_definition", "decorated_definition",
		}}, true
	case ".js", ".jsx", ".mjs", ".cjs":
		return &languageInfo{lang: javascript.GetLanguage(), name: "javascript", defTypes: []string{
			"function_declaration", "class_declaration", "method_definition",
			"lexical_declaration", "variable_declaration",
		}}, true
	case ".ts", ".tsx":
		return &languageInfo{lang: tstype.GetLanguage(), name: "typescript", defTypes: []string{
			"function_declaration", "class_declaration", "method_definition",
			"interface_declaration", "type_alias_declaration",
		}}, true
	case ".rs":
		return &languageInfo{lang: rust.GetLanguage(), name: "rust", defTypes: []string{
			"function_item", "struct_item", "enum_item", "trait_item", "impl_item",
		}}, true
	case ".java":
		return &languageInfo{lang: java.GetLanguage(), name: "java", defTypes: []string{
			"method_declaration", "class_declaration", "interface_declaration",
		}}, true
	case ".cs":
		return &languageInfo{lang: csharp.GetLanguage(), name: "csharp", defTypes: []string{
			"method_declaration", "class_declaration", "interface_declaration",
		}}, true
	case ".cpp", ".cc", ".cxx", ".hpp", ".h", ".hh", ".hxx":
		return &languageInfo{lang: cpp.GetLanguage(), name: "cpp", defTypes: []string{
			"function_definition", "class_specifier", "struct_specifier",
		}}, true

	// --- Jalur B additions (AST-aware) ---
	case ".rb":
		return &languageInfo{lang: ruby.GetLanguage(), name: "ruby", defTypes: []string{
			"method", "class", "module",
		}}, true
	case ".php":
		return &languageInfo{lang: php.GetLanguage(), name: "php", defTypes: []string{
			"function_definition", "class_declaration", "method_declaration", "interface_declaration",
		}}, true
	case ".sh", ".bash":
		return &languageInfo{lang: bash.GetLanguage(), name: "bash", defTypes: []string{
			"function_definition",
		}}, true
	case ".sql":
		return &languageInfo{lang: sql.GetLanguage(), name: "sql", defTypes: []string{
			"create_table_statement", "create_function_statement", "create_view_statement",
			"create_index_statement", "insert_statement", "select_statement",
		}}, true
	case ".swift":
		return &languageInfo{lang: swift.GetLanguage(), name: "swift", defTypes: []string{
			"function_declaration", "class_definition", "struct_declaration", "protocol_declaration",
		}}, true
	case ".kt":
		return &languageInfo{lang: kotlin.GetLanguage(), name: "kotlin", defTypes: []string{
			"function_declaration", "class_declaration", "object_declaration",
		}}, true
	case ".scala":
		return &languageInfo{lang: scala.GetLanguage(), name: "scala", defTypes: []string{
			"function_definition", "class_definition", "object_definition", "trait_definition",
		}}, true
	case ".lua":
		return &languageInfo{lang: lua.GetLanguage(), name: "lua", defTypes: []string{
			"function_definition", "function_declaration",
		}}, true
	case ".proto":
		return &languageInfo{lang: protobuf.GetLanguage(), name: "protobuf", defTypes: []string{
			"message", "service", "rpc", "enum",
		}}, true
	case ".css", ".scss", ".less":
		return &languageInfo{lang: css.GetLanguage(), name: "css", defTypes: []string{
			"rule_set",
		}}, true
	case ".html":
		return &languageInfo{lang: html.GetLanguage(), name: "html", defTypes: []string{
			"element",
		}}, true
	case ".tf", ".hcl":
		return &languageInfo{lang: hcl.GetLanguage(), name: "hcl", defTypes: []string{
			"block", "attribute",
		}}, true
	case ".yaml", ".yml":
		return &languageInfo{lang: yaml.GetLanguage(), name: "yaml", defTypes: []string{
			"block_mapping_pair",
		}}, true
	}
	// Dockerfile handled by basename (no extension).
	return nil, false
}

// langByBasename resolves languages for extensionless files (Dockerfile, Makefile).
func langByBasename(basename string) (*languageInfo, bool) {
	switch strings.ToLower(basename) {
	case "dockerfile":
		return &languageInfo{lang: dockerfile.GetLanguage(), name: "dockerfile", defTypes: []string{
			"image_spec", "run_instruction", "env_instruction", "cmd_instruction",
			"copy_instruction", "add_instruction",
		}}, true
	}
	return nil, false
}

// ChunkFile parses the source bytes and returns AST-aware chunks.
// If the language has no grammar, it falls back to line-based chunking.
func (ch *Chunker) ChunkFile(ctx context.Context, source []byte, sourceFile string) ([]Chunk, error) {
	ext := filepath.Ext(sourceFile)
	info, ok := langByExt(ext)
	if !ok {
		// Try basename (for Dockerfile, Makefile, etc).
		info, ok = langByBasename(filepath.Base(sourceFile))
	}
	if !ok {
		return ch.chunkNaive(source, sourceFile, ext), nil
	}
	return ch.chunkAST(ctx, source, sourceFile, info)
}

// chunkAST parses with tree-sitter and emits one chunk per top-level definition.
func (ch *Chunker) chunkAST(ctx context.Context, source []byte, sourceFile string, info *languageInfo) ([]Chunk, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(info.lang)
	tree, err := parser.ParseCtx(ctx, nil, source)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", sourceFile, err)
	}
	if tree == nil {
		return nil, fmt.Errorf("parse %s: nil tree (parse error)", sourceFile)
	}
	defer tree.Close()

	root := tree.RootNode()
	defSet := make(map[string]struct{}, len(info.defTypes))
	for _, t := range info.defTypes {
		defSet[t] = struct{}{}
	}

	var chunks []Chunk
	// Walk top-level named children; emit a chunk for each definition node.
	// Non-definition top-level nodes (imports, comments) are attached as context
	// to the following chunk or skipped.
	for i := 0; i < int(root.NamedChildCount()); i++ {
		child := root.NamedChild(i)
		if child == nil {
			continue
		}
		nodeType := child.Type()
		if _, isDef := defSet[nodeType]; !isDef {
			continue
		}
		chunks = append(chunks, ch.nodeToChunk(source, sourceFile, info.name, child)...)
	}

	// If no definitions were found (e.g. a file with only loose statements),
	// fall back to naive line-based chunking.
	if len(chunks) == 0 {
		return ch.chunkNaive(source, sourceFile, "."+info.name), nil
	}
	return chunks, nil
}

// nodeToChunk converts a tree-sitter node into a Chunk.
// If the node exceeds MaxChunkBytes, it is split into multiple line-based sub-chunks.
func (ch *Chunker) nodeToChunk(source []byte, sourceFile, lang string, n *sitter.Node) []Chunk {
	start := int(n.StartByte())
	end := int(n.EndByte())
	if start < 0 {
		start = 0
	}
	if end > len(source) {
		end = len(source)
	}
	content := string(source[start:end])
	symbol := n.Type()
	name := extractName(source, n)
	startLine := int(n.StartPoint().Row) + 1
	endLine := int(n.EndPoint().Row) + 1

	maxBytes := ch.MaxChunkBytes
	if maxBytes <= 0 {
		maxBytes = 6000
	}

	// If within limit, return single chunk.
	if len(content) <= maxBytes {
		return []Chunk{{
			Content:   content,
			SourceFile: sourceFile,
			Language:  lang,
			Symbol:    symbol,
			Name:      name,
			StartLine: startLine,
			EndLine:   endLine,
		}}
	}

	// Split large node by lines into sub-chunks under maxBytes.
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	chunkStart := 0
	currentBytes := 0
	for i, line := range lines {
		lineBytes := len(line) + 1 // +1 for newline
		if currentBytes+lineBytes > maxBytes && i > chunkStart {
			// Flush current chunk.
			chunks = append(chunks, Chunk{
				Content:   strings.Join(lines[chunkStart:i], "\n"),
				SourceFile: sourceFile,
				Language:  lang,
				Symbol:    symbol,
				Name:      fmt.Sprintf("%s (part %d)", name, len(chunks)+1),
				StartLine: startLine + chunkStart,
				EndLine:   startLine + i - 1,
			})
			chunkStart = i
			currentBytes = 0
		}
		currentBytes += lineBytes
	}
	// Flush remaining lines.
	if chunkStart < len(lines) {
		chunks = append(chunks, Chunk{
			Content:   strings.Join(lines[chunkStart:], "\n"),
			SourceFile: sourceFile,
			Language:  lang,
			Symbol:    symbol,
			Name:      fmt.Sprintf("%s (part %d)", name, len(chunks)+1),
			StartLine: startLine + chunkStart,
			EndLine:   endLine,
		})
	}
	return chunks
}

// extractName returns the first identifier-like child as the symbol name.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func extractName(source []byte, n *sitter.Node) string {
	// Check first few named children for an identifier.
	for i := 0; i < int(n.NamedChildCount()) && i < 3; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		t := c.Type()
		if t == "identifier" || t == "type_identifier" || t == "field_identifier" {
			return c.Content(source)
		}
	}
	// Fallback: first identifier in the node's text.
	start := int(n.StartByte())
	end := int(n.EndByte())
	if end > start+200 {
		end = start + 200
	}
	if m := identRe.FindString(string(source[start:end])); m != "" {
		return m
	}
	return ""
}

// chunkNaive splits by fixed line count for unsupported languages.
func (ch *Chunker) chunkNaive(source []byte, sourceFile, ext string) []Chunk {
	lines := strings.Split(string(source), "\n")
	chunkSize := ch.MaxChunkLines
	if chunkSize <= 0 {
		chunkSize = 60
	}
	maxBytes := ch.MaxChunkBytes
	if maxBytes <= 0 {
		maxBytes = 4500
	}
	lang := strings.TrimPrefix(ext, ".")
	if lang == "" {
		lang = "text"
	}
	var chunks []Chunk
	for start := 0; start < len(lines); {
		end := start + chunkSize
		if end > len(lines) {
			end = len(lines)
		}
		content := strings.Join(lines[start:end], "\n")
		// If chunk exceeds byte limit, shrink it line by line.
		for len(content) > maxBytes && end > start+1 {
			end--
			content = strings.Join(lines[start:end], "\n")
		}
		// If single line exceeds limit, truncate it (rare but possible).
		if len(content) > maxBytes {
			content = content[:maxBytes]
		}
		chunks = append(chunks, Chunk{
			Content:   content,
			SourceFile: sourceFile,
			Language:  lang,
			Symbol:    "chunk",
			Name:      fmt.Sprintf("%s:%d-%d", filepath.Base(sourceFile), start+1, end),
			StartLine: start + 1,
			EndLine:   end,
		})
		start = end
	}
	return chunks
}

func countLines(source []byte) int {
	n := 1
	for _, b := range source {
		if b == '\n' {
			n++
		}
	}
	return n
}
