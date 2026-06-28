package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
	"github.com/labstack/echo/v4"
)

const chunksPageSize = 25

type chunkFilter struct {
	Mode     string
	Query    string
	PathGlob string
	Language string
	Symbol   string
	MinSize  int
	MaxSize  int
	Page     int
}

type chunkRow struct {
	ID         string
	SourceFile string
	Language   string
	Symbol     string
	Name       string
	StartLine  int
	EndLine    int
	Size       int
	Content    string
	Score      float64
}

type chunkBrowserData struct {
	Project store.Project
	Rows    []chunkRow
	Total   int
	Info    rag.CollectionInfo
	Filter  chunkFilter
	Langs   []string
	Symbols []string
}

func (s *Server) uiChunksPanel(c echo.Context) error {
	p, ok := s.projectFromID(c)
	if !ok {
		return nil
	}
	data, err := s.loadChunkBrowserData(c.Request().Context(), p, parseChunkFilter(c))
	if err != nil {
		return c.HTML(500, s.renderChunkError("Could not load chunks", err))
	}
	return c.HTML(200, s.renderChunksPanel(data))
}

func (s *Server) uiChunksTable(c echo.Context) error {
	p, ok := s.projectFromID(c)
	if !ok {
		return nil
	}
	data, err := s.loadChunkBrowserData(c.Request().Context(), p, parseChunkFilter(c))
	if err != nil {
		return c.HTML(500, s.renderChunkError("Could not filter chunks", err))
	}
	return c.HTML(200, s.renderChunksTable(data))
}

func (s *Server) uiChunkDetail(c echo.Context) error {
	p, ok := s.projectFromID(c)
	if !ok {
		return nil
	}
	if s.rag == nil {
		return c.HTML(503, s.renderChunkError("Qdrant is offline", errBackendUnavailable))
	}
	pointID := c.Param("pointID")
	pt, err := s.rag.GetPoint(c.Request().Context(), s.collectionKeyFor(p.Name), pointID)
	if err != nil {
		return c.HTML(404, s.renderChunkError("Chunk not found", err))
	}
	return c.HTML(200, s.renderChunkDetail(p, pointToChunkRow(pt, 0)))
}

func (s *Server) projectFromID(c echo.Context) (store.Project, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		_ = c.HTML(400, "<p class='error'>Invalid project ID</p>")
		return store.Project{}, false
	}
	p, err := s.store.GetProject(c.Request().Context(), id)
	if err != nil {
		_ = c.HTML(404, "<p class='error'>Project not found</p>")
		return store.Project{}, false
	}
	return p, true
}

func (s *Server) loadChunkBrowserData(ctx context.Context, p store.Project, f chunkFilter) (chunkBrowserData, error) {
	if s.rag == nil {
		return chunkBrowserData{}, errBackendUnavailable
	}
	key := s.collectionKeyFor(p.Name)
	info, err := s.rag.CollectionInfo(ctx, key)
	if err != nil {
		return chunkBrowserData{}, err
	}
	rows, err := s.fetchChunkRows(ctx, p.Name, f)
	if err != nil {
		return chunkBrowserData{}, err
	}
	langs, symbols := chunkFacets(rows)
	filtered := filterChunkRows(rows, f)
	if f.Mode == "semantic" && f.Query != "" {
		filtered = s.semanticChunkRows(ctx, p.Name, f, filtered)
	} else {
		sortChunkRows(filtered, false)
	}
	paged := paginateChunkRows(filtered, f.Page)
	return chunkBrowserData{
		Project: p,
		Rows:    paged,
		Total:   len(filtered),
		Info:    info,
		Filter:  f,
		Langs:   langs,
		Symbols: symbols,
	}, nil
}

func (s *Server) fetchChunkRows(ctx context.Context, project string, f chunkFilter) ([]chunkRow, error) {
	filter := qdrantChunkFilter(f)
	var rows []chunkRow
	offset := ""
	for {
		res, err := s.rag.Scroll(ctx, s.collectionKeyFor(project), rag.ScrollOpts{
			Offset: offset,
			Limit:  500,
			Filter: filter,
		})
		if err != nil {
			return nil, err
		}
		for _, pt := range res.Points {
			rows = append(rows, pointToChunkRow(pt, 0))
		}
		if res.NextOffset == "" {
			break
		}
		offset = res.NextOffset
	}
	return rows, nil
}

func (s *Server) semanticChunkRows(ctx context.Context, project string, f chunkFilter, filtered []chunkRow) []chunkRow {
	results, err := s.doSearch(ctx, project, f.Query, 200)
	if err != nil {
		sortChunkRows(filtered, false)
		return filtered
	}
	allowed := make(map[string]chunkRow, len(filtered))
	for _, r := range filtered {
		allowed[r.ID] = r
	}
	out := make([]chunkRow, 0, len(results))
	for _, r := range results {
		row, ok := allowed[r.ID]
		if !ok {
			row = resultToChunkRow(r)
			if !matchesChunkFilter(row, f, false) {
				continue
			}
		}
		row.Score = r.Score
		out = append(out, row)
	}
	sortChunkRows(out, true)
	return out
}

func parseChunkFilter(c echo.Context) chunkFilter {
	page, _ := strconv.Atoi(c.QueryParam("page"))
	if page < 1 {
		page = 1
	}
	minSize, _ := strconv.Atoi(c.QueryParam("min_size"))
	maxSize, _ := strconv.Atoi(c.QueryParam("max_size"))
	mode := c.QueryParam("mode")
	if mode != "semantic" {
		mode = "keyword"
	}
	return chunkFilter{
		Mode:     mode,
		Query:    strings.TrimSpace(c.QueryParam("q")),
		PathGlob: strings.TrimSpace(c.QueryParam("path")),
		Language: strings.TrimSpace(c.QueryParam("lang")),
		Symbol:   strings.TrimSpace(c.QueryParam("symbol")),
		MinSize:  minSize,
		MaxSize:  maxSize,
		Page:     page,
	}
}

func qdrantChunkFilter(f chunkFilter) map[string]any {
	var must []map[string]any
	if f.Language != "" {
		must = append(must, map[string]any{"key": "language", "match": map[string]any{"value": f.Language}})
	}
	if f.Symbol != "" {
		must = append(must, map[string]any{"key": "symbol", "match": map[string]any{"value": f.Symbol}})
	}
	if len(must) == 0 {
		return nil
	}
	return map[string]any{"must": must}
}

func pointToChunkRow(pt rag.Point, score float64) chunkRow {
	return chunkRow{
		ID:         pt.ID,
		SourceFile: orFallback(pt.Meta["source_file"], "(unknown)"),
		Language:   pt.Meta["language"],
		Symbol:     pt.Meta["symbol"],
		Name:       pt.Meta["name"],
		StartLine:  atoiDefault(pt.Meta["start_line"], 0),
		EndLine:    atoiDefault(pt.Meta["end_line"], 0),
		Size:       len(pt.Content),
		Content:    pt.Content,
		Score:      score,
	}
}

func resultToChunkRow(r rag.Result) chunkRow {
	return chunkRow{
		ID:         r.ID,
		SourceFile: orFallback(r.Meta["source_file"], "(unknown)"),
		Language:   r.Meta["language"],
		Symbol:     r.Meta["symbol"],
		Name:       r.Meta["name"],
		StartLine:  atoiDefault(r.Meta["start_line"], 0),
		EndLine:    atoiDefault(r.Meta["end_line"], 0),
		Size:       len(r.Content),
		Content:    r.Content,
		Score:      r.Score,
	}
}

func filterChunkRows(rows []chunkRow, f chunkFilter) []chunkRow {
	out := make([]chunkRow, 0, len(rows))
	for _, row := range rows {
		if matchesChunkFilter(row, f, true) {
			out = append(out, row)
		}
	}
	return out
}

func matchesChunkFilter(row chunkRow, f chunkFilter, applyKeyword bool) bool {
	if applyKeyword && f.Mode == "keyword" && f.Query != "" {
		needle := strings.ToLower(f.Query)
		haystack := strings.ToLower(row.Content + "\n" + row.SourceFile + "\n" + row.Symbol + "\n" + row.Name)
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	if f.PathGlob != "" && !matchPathFilter(f.PathGlob, row.SourceFile) {
		return false
	}
	if f.Language != "" && !strings.EqualFold(f.Language, row.Language) {
		return false
	}
	if f.Symbol != "" && !strings.EqualFold(f.Symbol, row.Symbol) {
		return false
	}
	if f.MinSize > 0 && row.Size < f.MinSize {
		return false
	}
	if f.MaxSize > 0 && row.Size > f.MaxSize {
		return false
	}
	return true
}

func matchPathFilter(pattern, file string) bool {
	if pattern == "" {
		return true
	}
	if strings.Contains(pattern, "**") {
		needle := strings.Trim(pattern, "*")
		needle = strings.Trim(needle, "/")
		return needle == "" || strings.Contains(file, needle)
	}
	ok, err := path.Match(pattern, file)
	if err == nil && ok {
		return true
	}
	return strings.Contains(strings.ToLower(file), strings.ToLower(pattern))
}

func sortChunkRows(rows []chunkRow, byScore bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		if byScore && rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].SourceFile == rows[j].SourceFile {
			return rows[i].StartLine < rows[j].StartLine
		}
		return rows[i].SourceFile < rows[j].SourceFile
	})
}

func paginateChunkRows(rows []chunkRow, page int) []chunkRow {
	if page < 1 {
		page = 1
	}
	start := (page - 1) * chunksPageSize
	if start >= len(rows) {
		return nil
	}
	end := start + chunksPageSize
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end]
}

func chunkFacets(rows []chunkRow) ([]string, []string) {
	langSet := map[string]bool{}
	symbolSet := map[string]bool{}
	for _, row := range rows {
		if row.Language != "" {
			langSet[row.Language] = true
		}
		if row.Symbol != "" {
			symbolSet[row.Symbol] = true
		}
	}
	langs := sortedKeys(langSet)
	symbols := sortedKeys(symbolSet)
	return langs, symbols
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (s *Server) renderChunksPanel(data chunkBrowserData) string {
	p := data.Project
	filter := data.Filter
	var sb strings.Builder
	sb.WriteString("<div class='chunks-inline'>")
	// Inline header strip (replaces the old chunks-topbar)
	sb.WriteString("<div class='chunks-inline-head'>")
	sb.WriteString("<h3>Indexed chunks</h3>")
	sb.WriteString(fmt.Sprintf("<span class='muted small'>%d points · %d dim · %s</span>",
		data.Info.PointsCount, data.Info.VectorsSize, template.HTMLEscapeString(data.Info.Status)))
	sb.WriteString("</div>")
	sb.WriteString(s.renderChunkFilters(p, filter, data.Langs, data.Symbols))
	sb.WriteString("<div id='chunks-table'>")
	sb.WriteString(s.renderChunksTable(data))
	sb.WriteString("</div>")
	sb.WriteString("</div>")
	return sb.String()
}

func (s *Server) renderChunkFilters(p store.Project, f chunkFilter, langs, symbols []string) string {
	endpoint := fmt.Sprintf("/ui/projects/%d/chunks/table", p.ID)
	var sb strings.Builder
	sb.WriteString("<form id='chunk-filter-form' class='chunk-filters' ")
	sb.WriteString(fmt.Sprintf("hx-get='%s' hx-target='#chunks-table' hx-swap='innerHTML' hx-trigger='submit, keyup changed delay:350ms from:input, change from:select'>", endpoint))
	sb.WriteString("<div class='filter-row primary'>")
	sb.WriteString("<div class='mode-toggle'>")
	sb.WriteString(radioMode("keyword", "Keyword", f.Mode == "keyword"))
	sb.WriteString(radioMode("semantic", "Semantic", f.Mode == "semantic"))
	sb.WriteString("</div>")
	sb.WriteString(fmt.Sprintf("<input class='chunk-search-input' name='q' value='%s' placeholder='Search content, file, symbol...'>", template.HTMLEscapeString(f.Query)))
	sb.WriteString("<button type='submit'>Apply</button>")
	sb.WriteString(fmt.Sprintf("<button type='button' class='ghost-btn' hx-get='%s' hx-target='#chunks-table' hx-swap='innerHTML' onclick=\"this.closest('form').reset()\">Clear</button>", endpoint))
	sb.WriteString("</div>")
	sb.WriteString("<details class='advanced-filters' open><summary>Advanced filters</summary>")
	sb.WriteString("<div class='filter-row'>")
	sb.WriteString(fmt.Sprintf("<label>Path glob<input name='path' value='%s' placeholder='rag/* or chunker/*'></label>", template.HTMLEscapeString(f.PathGlob)))
	sb.WriteString("<label>Language<select name='lang'><option value=''>All</option>")
	for _, lang := range langs {
		sel := ""
		if lang == f.Language {
			sel = " selected"
		}
		sb.WriteString(fmt.Sprintf("<option value='%s'%s>%s</option>", template.HTMLEscapeString(lang), sel, template.HTMLEscapeString(lang)))
	}
	sb.WriteString("</select></label>")
	sb.WriteString("<label>Symbol<select name='symbol'><option value=''>All</option>")
	for _, symbol := range symbols {
		sel := ""
		if symbol == f.Symbol {
			sel = " selected"
		}
		sb.WriteString(fmt.Sprintf("<option value='%s'%s>%s</option>", template.HTMLEscapeString(symbol), sel, template.HTMLEscapeString(symbol)))
	}
	sb.WriteString("</select></label>")
	sb.WriteString(fmt.Sprintf("<label>Min chars<input type='number' name='min_size' value='%s' min='0'></label>", intValue(f.MinSize)))
	sb.WriteString(fmt.Sprintf("<label>Max chars<input type='number' name='max_size' value='%s' min='0'></label>", intValue(f.MaxSize)))
	sb.WriteString("</div></details>")
	sb.WriteString("</form>")
	return sb.String()
}

func radioMode(value, label string, checked bool) string {
	ck := ""
	if checked {
		ck = " checked"
	}
	return fmt.Sprintf("<label><input type='radio' name='mode' value='%s'%s><span>%s</span></label>", value, ck, label)
}

func intValue(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}

func (s *Server) renderChunksTable(data chunkBrowserData) string {
	f := data.Filter
	pages := int(math.Ceil(float64(data.Total) / float64(chunksPageSize)))
	if pages < 1 {
		pages = 1
	}
	page := f.Page
	if page > pages {
		page = pages
	}
	queryBase := chunkQueryWithoutPage(f)
	var sb strings.Builder
	sb.WriteString("<div class='chunk-table-card'>")
	sb.WriteString("<div class='chunk-table-status'>")
	sb.WriteString(fmt.Sprintf("<span><strong>%d</strong> matching chunks</span>", data.Total))
	if f.Mode == "semantic" && f.Query != "" {
		sb.WriteString("<span class='badge score-mode'>score sorted</span>")
	} else {
		sb.WriteString("<span class='badge'>file order</span>")
	}
	sb.WriteString("</div>")
	if len(data.Rows) == 0 {
		sb.WriteString("<div class='chunk-empty compact'><h3>No chunks match</h3><p class='muted small'>Clear filters or try a broader query.</p></div></div>")
		return sb.String()
	}
	sb.WriteString("<table class='chunk-table'><thead><tr>")
	sb.WriteString("<th>File</th><th>Lines</th><th>Symbol</th><th>Size</th>")
	if f.Mode == "semantic" && f.Query != "" {
		sb.WriteString("<th>Score</th>")
	}
	sb.WriteString("</tr></thead><tbody>")
	for _, row := range data.Rows {
		sb.WriteString(s.renderChunkRow(data.Project, row, f.Mode == "semantic" && f.Query != ""))
	}
	sb.WriteString("</tbody></table>")
	sb.WriteString("<div class='chunk-pager'>")
	if page > 1 {
		sb.WriteString(fmt.Sprintf("<button hx-get='/ui/projects/%d/chunks/table?%s&page=%d' hx-target='#chunks-table' hx-swap='innerHTML'>← Prev</button>", data.Project.ID, queryBase, page-1))
	} else {
		sb.WriteString("<button disabled>← Prev</button>")
	}
	sb.WriteString(fmt.Sprintf("<span>Page %d of %d</span>", page, pages))
	if page < pages {
		sb.WriteString(fmt.Sprintf("<button hx-get='/ui/projects/%d/chunks/table?%s&page=%d' hx-target='#chunks-table' hx-swap='innerHTML'>Next →</button>", data.Project.ID, queryBase, page+1))
	} else {
		sb.WriteString("<button disabled>Next →</button>")
	}
	sb.WriteString("</div></div>")
	return sb.String()
}

func (s *Server) renderChunkRow(p store.Project, row chunkRow, showScore bool) string {
	name := row.Name
	if name == "" {
		name = row.Symbol
	}
	if name == "" {
		name = "chunk"
	}
	var sb strings.Builder
	// Click row -> swap drawer content + open it via hx-on (htmx 1.9 inline event handler)
	sb.WriteString(fmt.Sprintf("<tr class='chunk-row' data-chunk-id='%s' hx-get='/ui/projects/%d/chunks/%s' hx-target='#chunk-drawer' hx-swap='innerHTML' hx-on::after-request='openDrawer(this)'>",
		template.HTMLEscapeString(row.ID), p.ID, template.URLQueryEscaper(row.ID)))
	sb.WriteString("<td>")
	sb.WriteString(fmt.Sprintf("<div class='chunk-file'>%s</div>", template.HTMLEscapeString(row.SourceFile)))
	sb.WriteString(fmt.Sprintf("<div class='chunk-preview'>%s</div>", template.HTMLEscapeString(oneLine(row.Content, 96))))
	sb.WriteString("</td>")
	sb.WriteString(fmt.Sprintf("<td><span class='line-chip'>%d-%d</span></td>", row.StartLine, row.EndLine))
	sb.WriteString(fmt.Sprintf("<td><span class='symbol-chip'>%s</span><div class='chunk-name'>%s</div></td>", template.HTMLEscapeString(orFallback(row.Symbol, "unknown")), template.HTMLEscapeString(name)))
	sb.WriteString(fmt.Sprintf("<td>%d</td>", row.Size))
	if showScore {
		sb.WriteString(fmt.Sprintf("<td><span class='%s'>%.3f</span></td>", scoreClass(row.Score), row.Score))
	}
	sb.WriteString("</tr>")
	return sb.String()
}

func (s *Server) renderChunkDetail(p store.Project, row chunkRow) string {
	var sb strings.Builder
	// Drawer head
	sb.WriteString("<div class='chunk-drawer-head'>")
	sb.WriteString(fmt.Sprintf("<h3>%s</h3>", template.HTMLEscapeString(orFallback(row.Name, row.Symbol))))
	sb.WriteString("<button type='button' class='chunk-drawer-close' onclick='closeDrawer()' aria-label='Close'>×</button>")
	sb.WriteString("</div>")
	sb.WriteString("<div class='chunk-drawer-body'>")
	sb.WriteString("<div class='chunk-detail-card'>")
	sb.WriteString("<div class='chunk-detail-head'>")
	sb.WriteString(fmt.Sprintf("<div><p class='muted small' style='font-family:var(--mono)'>%s</p></div>", template.HTMLEscapeString(row.SourceFile)))
	sb.WriteString("<div class='detail-badges'>")
	sb.WriteString(fmt.Sprintf("<span class='badge'>%s</span>", template.HTMLEscapeString(orFallback(row.Language, "text"))))
	sb.WriteString(fmt.Sprintf("<span class='badge'>L%d-%d</span>", row.StartLine, row.EndLine))
	sb.WriteString(fmt.Sprintf("<span class='badge'>%d chars</span>", row.Size))
	sb.WriteString("</div></div>")
	sb.WriteString("<pre class='chunk-code'><code>")
	sb.WriteString(template.HTMLEscapeString(row.Content))
	sb.WriteString("</code></pre>")
	sb.WriteString("<div class='chunk-meta'>")
	sb.WriteString(metaLine("Point ID", row.ID))
	sb.WriteString(metaLine("Project", p.Name))
	sb.WriteString(metaLine("Symbol", row.Symbol))
	sb.WriteString(metaLine("Name", row.Name))
	sb.WriteString("</div></div></div>")
	return sb.String()
}

func metaLine(k, v string) string {
	return fmt.Sprintf("<div><span>%s</span><code>%s</code></div>", template.HTMLEscapeString(k), template.HTMLEscapeString(orFallback(v, "-")))
}

func chunkQueryWithoutPage(f chunkFilter) string {
	v := url.Values{}
	v.Set("mode", f.Mode)
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.PathGlob != "" {
		v.Set("path", f.PathGlob)
	}
	if f.Language != "" {
		v.Set("lang", f.Language)
	}
	if f.Symbol != "" {
		v.Set("symbol", f.Symbol)
	}
	if f.MinSize > 0 {
		v.Set("min_size", strconv.Itoa(f.MinSize))
	}
	if f.MaxSize > 0 {
		v.Set("max_size", strconv.Itoa(f.MaxSize))
	}
	return v.Encode()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func scoreClass(score float64) string {
	switch {
	case score >= 0.65:
		return "score-pill score-high"
	case score >= 0.5:
		return "score-pill score-mid"
	default:
		return "score-pill score-low"
	}
}

func (s *Server) renderChunkError(title string, err error) string {
	return fmt.Sprintf("<div class='index-stats'><h3 class='error'>%s</h3><p class='small muted'>%s</p></div>", template.HTMLEscapeString(title), template.HTMLEscapeString(err.Error()))
}

// apiProjectStats returns Qdrant collection stats for a project (JSON API).
// GET /api/v1/projects/:name/stats
func (s *Server) apiProjectStats(c echo.Context) error {
	project := c.Param("name")
	if project == "" {
		return c.JSON(400, map[string]string{"error": "project name is required"})
	}
	key := s.collectionKeyFor(project)
	info, err := s.rag.CollectionInfo(c.Request().Context(), key)
	if err != nil {
		return c.JSON(404, map[string]string{"error": fmt.Sprintf("collection: %v", err)})
	}
	return c.JSON(200, map[string]any{
		"status":       info.Status,
		"points_count": info.PointsCount,
		"vectors_size": info.VectorsSize,
	})
}

// apiGetChunk returns a single chunk by point ID (JSON API).
// GET /api/v1/projects/:name/chunks/:pointID
func (s *Server) apiGetChunk(c echo.Context) error {
	project := c.Param("name")
	pointID := c.Param("pointID")
	if project == "" || pointID == "" {
		return c.JSON(400, map[string]string{"error": "project name and pointID are required"})
	}
	key := s.collectionKeyFor(project)
	pt, err := s.rag.GetPoint(c.Request().Context(), key, pointID)
	if err != nil {
		return c.JSON(404, map[string]string{"error": fmt.Sprintf("get point: %v", err)})
	}
	return c.JSON(200, map[string]any{
		"id":      pointID,
		"content": pt.Content,
		"meta":    pt.Meta,
	})
}
