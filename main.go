package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/net/html"
)

const baseURL = "http://localhost:4004"
const pageSize = 10
const colWidth = 20

// ── OData XML structs ─────────────────────────────────────────────────────────

type Edmx struct {
	XMLName      xml.Name     `xml:"Edmx"`
	DataServices DataServices `xml:"DataServices"`
}
type DataServices struct{ Schemas []Schema `xml:"Schema"` }
type Schema struct{ EntityTypes []EntityType `xml:"EntityType"` }
type EntityType struct {
	Name       string      `xml:"Name,attr"`
	Key        EntityKey   `xml:"Key"`
	Properties []Property  `xml:"Property"`
}
type EntityKey struct {
	PropertyRefs []PropertyRef `xml:"PropertyRef"`
}
type PropertyRef struct {
	Name string `xml:"Name,attr"`
}
type Property struct {
	Name string `xml:"Name,attr"`
	Type string `xml:"Type,attr"`
}

type ODataResponse struct {
	Value []map[string]interface{} `json:"value"`
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func patch(url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest("PATCH", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// ── HTML scraping ─────────────────────────────────────────────────────────────

func scrapeMetadataLinks(body []byte) []string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}
	var links []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" && strings.Contains(attr.Val, "$metadata") {
					links = append(links, attr.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return links
}

// ── Metadata parsing ──────────────────────────────────────────────────────────

type entityMeta struct {
	keyFields    []string          // OData key property names
	propTypes    map[string]string // property name → EDM type
}

func parseMetadata(body []byte) map[string]entityMeta {
	result := map[string]entityMeta{}

	var edmx Edmx
	if err := xml.Unmarshal(body, &edmx); err != nil {
		// fallback: no key info available
		return result
	}
	for _, schema := range edmx.DataServices.Schemas {
		for _, et := range schema.EntityTypes {
			meta := entityMeta{propTypes: map[string]string{}}
			for _, ref := range et.Key.PropertyRefs {
				meta.keyFields = append(meta.keyFields, ref.Name)
			}
			for _, prop := range et.Properties {
				meta.propTypes[prop.Name] = prop.Type
			}
			result[et.Name] = meta
		}
	}
	return result
}

func parseEntityNames(body []byte) []string {
	var names []string
	var edmx Edmx
	if err := xml.Unmarshal(body, &edmx); err == nil {
		for _, schema := range edmx.DataServices.Schemas {
			for _, et := range schema.EntityTypes {
				if et.Name != "" {
					names = append(names, et.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		re := regexp.MustCompile(`(?i)<EntityType[^>]+Name="([^"]+)"`)
		for _, m := range re.FindAllSubmatch(body, -1) {
			names = append(names, string(m[1]))
		}
	}
	return names
}

// ── Entity loading ────────────────────────────────────────────────────────────

type EntityEntry struct {
	DisplayName string
	URL         string
	EntityName  string            // bare name, e.g. "Books"
	Meta        entityMeta
}

func loadEntities() ([]EntityEntry, error) {
	body, err := get(baseURL + "/")
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", baseURL, err)
	}
	metadataLinks := scrapeMetadataLinks(body)
	if len(metadataLinks) == 0 {
		return nil, fmt.Errorf("no $metadata links found on the index page")
	}
	var entries []EntityEntry
	for _, link := range metadataLinks {
		serviceBase := strings.TrimSuffix(link, "/$metadata")
		metaBody, err := get(baseURL + link)
		if err != nil {
			continue
		}
		metas := parseMetadata(metaBody)
		for _, name := range parseEntityNames(metaBody) {
			url := baseURL + serviceBase + "/" + name
			display := strings.TrimPrefix(serviceBase, "/") + " › " + name
			entries = append(entries, EntityEntry{
				DisplayName: display,
				URL:         url,
				EntityName:  name,
				Meta:        metas[name],
			})
		}
	}
	return entries, nil
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	hintStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	editStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11"))
	colHdrStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	selColStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")).Underline(true).Background(lipgloss.Color("236"))
	selRowStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
	selCellStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")).Background(lipgloss.Color("236"))
)

// ── Pager mode ────────────────────────────────────────────────────────────────

type pagerMode int

const (
	modeNav  pagerMode = iota // normal navigation
	modeEdit                  // editing a cell
)

// ── Pager model ───────────────────────────────────────────────────────────────

type pagerModel struct {
	entity    EntityEntry
	skip      int
	rows      [][]string
	header    []string
	fetchErr  string
	done      bool
	selRow    int
	selCol    int
	colOff    int
	winCols   int
	mode      pagerMode
	editBuf   []rune // current edit buffer
	statusMsg string // shown in status bar (ok or error after PATCH)
	statusErr bool   // true = red, false = green
}

func newPager(e EntityEntry) pagerModel {
	m := pagerModel{entity: e, winCols: 120}
	m.fetch()
	return m
}

func (m *pagerModel) fetch() {
	url := fmt.Sprintf("%s?$top=%d&$skip=%d", m.entity.URL, pageSize, m.skip)
	body, err := get(url)
	if err != nil {
		m.fetchErr = err.Error()
		return
	}
	var resp ODataResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		m.fetchErr = string(body)
		return
	}
	m.fetchErr = ""
	m.header = nil
	m.rows = nil
	if len(resp.Value) == 0 {
		return
	}
	for k := range resp.Value[0] {
		m.header = append(m.header, k)
	}
	sort.Strings(m.header)
	for _, row := range resp.Value {
		var cells []string
		for _, k := range m.header {
			v := row[k]
			if v == nil {
				cells = append(cells, "<null>")
			} else {
				cells = append(cells, fmt.Sprintf("%v", v))
			}
		}
		m.rows = append(m.rows, cells)
	}
	if m.selRow >= len(m.rows) {
		m.selRow = imax(0, len(m.rows)-1)
	}
	if len(m.header) > 0 && m.selCol >= len(m.header) {
		m.selCol = imax(0, len(m.header)-1)
	}
}

// buildPatchURL constructs the OData key-predicate URL for the selected row.
// e.g. /Books(ID=1)  or  /Books(1)  for single key.
func (m *pagerModel) buildPatchURL() (string, error) {
	keys := m.entity.Meta.keyFields
	if len(keys) == 0 {
		return "", fmt.Errorf("no key fields found in metadata for %s", m.entity.EntityName)
	}
	row := m.rows[m.selRow]
	var parts []string
	for _, k := range keys {
		idx := -1
		for i, h := range m.header {
			if h == k {
				idx = i
				break
			}
		}
		if idx == -1 {
			return "", fmt.Errorf("key field %q not found in response columns", k)
		}
		val := row[idx]
		// Quote strings, leave numbers bare
		edm := m.entity.Meta.propTypes[k]
		if isStringType(edm) {
			parts = append(parts, fmt.Sprintf("%s='%s'", k, val))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%s", k, val))
		}
	}
	predicate := strings.Join(parts, ",")
	// Single-key shorthand: just the value
	if len(keys) == 1 {
		val := row[func() int {
			for i, h := range m.header {
				if h == keys[0] {
					return i
				}
			}
			return 0
		}()]
		edm := m.entity.Meta.propTypes[keys[0]]
		if isStringType(edm) {
			predicate = fmt.Sprintf("'%s'", val)
		} else {
			predicate = val
		}
	}
	return fmt.Sprintf("%s(%s)", m.entity.URL, predicate), nil
}

func isStringType(edm string) bool {
	return strings.Contains(strings.ToLower(edm), "string") || edm == ""
}

func (m *pagerModel) commitEdit() {
	newVal := string(m.editBuf)
	col := m.header[m.selCol]

	patchURL, err := m.buildPatchURL()
	if err != nil {
		m.statusMsg = "PATCH error: " + err.Error()
		m.statusErr = true
		m.mode = modeNav
		return
	}

	payload := map[string]interface{}{col: newVal}
	body, _ := json.Marshal(payload)
	status, respBody, err := patch(patchURL, body)
	if err != nil {
		m.statusMsg = "PATCH error: " + err.Error()
		m.statusErr = true
		m.mode = modeNav
		return
	}
	if status < 200 || status >= 300 {
		// Try to extract OData error message
		var odataErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := fmt.Sprintf("HTTP %d", status)
		if json.Unmarshal(respBody, &odataErr) == nil && odataErr.Error.Message != "" {
			msg += ": " + odataErr.Error.Message
		}
		m.statusMsg = msg
		m.statusErr = true
		m.mode = modeNav
		return
	}

	// Success — update local cell and re-fetch to stay in sync
	m.rows[m.selRow][m.selCol] = newVal
	m.statusMsg = fmt.Sprintf("✓ %s updated", col)
	m.statusErr = false
	m.mode = modeNav
	m.fetch()
}

func (m *pagerModel) visibleCols() int {
	if m.winCols <= 0 {
		return len(m.header)
	}
	return imax(1, m.winCols/(colWidth+2))
}

func (m *pagerModel) panToCol() {
	vis := m.visibleCols()
	if m.selCol < m.colOff {
		m.colOff = m.selCol
	}
	if m.selCol >= m.colOff+vis {
		m.colOff = m.selCol - vis + 1
	}
}

func (m pagerModel) Init() tea.Cmd { return nil }

func (m pagerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.winCols = msg.Width

	case tea.KeyMsg:
		// ── edit mode ─────────────────────────────────────────────────
		if m.mode == modeEdit {
			switch msg.String() {
			case "enter":
				m.commitEdit()
			case "esc":
				m.mode = modeNav
				m.statusMsg = ""
			case "ctrl+c":
				os.Exit(0)
			case "backspace":
				if len(m.editBuf) > 0 {
					m.editBuf = m.editBuf[:len(m.editBuf)-1]
				}
			default:
				// Append printable runes
				if r := []rune(msg.String()); len(r) == 1 && r[0] >= 32 {
					m.editBuf = append(m.editBuf, r[0])
				}
			}
			return m, nil
		}

		// ── nav mode ──────────────────────────────────────────────────
		switch msg.String() {
		case "enter":
			if len(m.rows) > 0 {
				// seed edit buffer with current cell value
				m.editBuf = []rune(m.rows[m.selRow][m.selCol])
				m.statusMsg = ""
				m.mode = modeEdit
			}
		case "n":
			if len(m.rows) == pageSize {
				m.skip += pageSize
				m.fetch()
			}
		case "p":
			if m.skip >= pageSize {
				m.skip -= pageSize
				m.fetch()
			}
		case "k": // row up
			if m.selRow > 0 {
				m.selRow--
			} else if m.skip >= pageSize {
				m.skip -= pageSize
				m.fetch()
				m.selRow = len(m.rows) - 1
			}
		case "j": // row down
			if m.selRow < len(m.rows)-1 {
				m.selRow++
			} else if len(m.rows) == pageSize {
				m.skip += pageSize
				m.fetch()
				m.selRow = 0
			}
		case "h": // col left
			if m.selCol > 0 {
				m.selCol--
				m.panToCol()
			}
		case "l": // col right
			if m.selCol < len(m.header)-1 {
				m.selCol++
				m.panToCol()
			}
		case "r":
			m.fetch()
		case "b", "esc":
			m.done = true
			return m, tea.Quit
		case "ctrl+c":
			os.Exit(0)
		}
	}
	return m, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func padOrTrunc(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		if w > 1 {
			return string(r[:w-1]) + "…"
		}
		return "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}

func (m pagerModel) View() string {
	var sb strings.Builder

	page := m.skip/pageSize + 1
	sb.WriteString(titleStyle.Render(fmt.Sprintf("Entity: %s  (Page %d)", m.entity.DisplayName, page)))
	sb.WriteString("\n")

	if m.mode == modeEdit {
		col := ""
		if m.selCol < len(m.header) {
			col = m.header[m.selCol]
		}
		sb.WriteString(hintStyle.Render(fmt.Sprintf("  EDIT [%s]  Enter=save  Esc=cancel", col)))
	} else {
		sb.WriteString(hintStyle.Render("  [n/p] page  [r] refresh  [b] back  [j/k] row ↑/↓  [h/l] col ←/→  [Enter] edit"))
	}
	sb.WriteString("\n\n")

	if m.fetchErr != "" {
		sb.WriteString(errStyle.Render("Error: " + m.fetchErr))
		return sb.String()
	}
	if len(m.rows) == 0 {
		sb.WriteString(hintStyle.Render("--- No data ---"))
		return sb.String()
	}

	vis := m.visibleCols()
	end := imin(m.colOff+vis, len(m.header))
	visHeaders := m.header[m.colOff:end]

	// header row
	for ci, h := range visHeaders {
		absCI := m.colOff + ci
		cell := padOrTrunc(h, colWidth)
		if absCI == m.selCol {
			sb.WriteString(selColStyle.Render(cell))
		} else {
			sb.WriteString(colHdrStyle.Render(cell))
		}
		sb.WriteString("  ")
	}
	sb.WriteString("\n")

	// separator
	for range visHeaders {
		sb.WriteString(hintStyle.Render(strings.Repeat("─", colWidth)))
		sb.WriteString("  ")
	}
	sb.WriteString("\n")

	// data rows
	for ri, row := range m.rows {
		for ci := range visHeaders {
			absCI := m.colOff + ci
			var cell string
			// In edit mode, show live buffer for the active cell
			if m.mode == modeEdit && ri == m.selRow && absCI == m.selCol {
				raw := string(m.editBuf) + "█"
				cell = padOrTrunc(raw, colWidth)
				sb.WriteString(editStyle.Render(cell))
			} else {
				cell = padOrTrunc(row[absCI], colWidth)
				switch {
				case ri == m.selRow && absCI == m.selCol:
					sb.WriteString(selCellStyle.Render(cell))
				case ri == m.selRow:
					sb.WriteString(selRowStyle.Render(cell))
				case absCI == m.selCol:
					sb.WriteString(selColStyle.Render(cell))
				default:
					sb.WriteString(cell)
				}
			}
			sb.WriteString("  ")
		}
		sb.WriteString("\n")
	}

	// status bar / pan indicator
	sb.WriteString("\n")
	if m.statusMsg != "" {
		if m.statusErr {
			sb.WriteString(errStyle.Render("  " + m.statusMsg))
		} else {
			sb.WriteString(okStyle.Render("  " + m.statusMsg))
		}
	} else if len(m.header) > vis {
		sb.WriteString(hintStyle.Render(fmt.Sprintf(
			"  cols %d–%d of %d  (col: %s)",
			m.colOff+1, end, len(m.header), m.header[m.selCol],
		)))
	}

	return sb.String()
}

// ── Menu ──────────────────────────────────────────────────────────────────────

type item struct{ entry EntityEntry }

func (i item) Title() string       { return i.entry.DisplayName }
func (i item) Description() string { return i.entry.URL }
func (i item) FilterValue() string { return i.entry.DisplayName }

type menuModel struct {
	list     list.Model
	selected *EntityEntry
	quitting bool
}

func newMenu(entries []EntityEntry) menuModel {
	items := make([]list.Item, len(entries))
	for i, e := range entries {
		items[i] = item{e}
	}
	l := list.New(items, list.NewDefaultDelegate(), 80, 24)
	l.Title = "CAP OData Browser"
	l.Styles.Title = titleStyle
	return menuModel{list: l}
}

func (m menuModel) Init() tea.Cmd { return nil }

func (m menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if i, ok := m.list.SelectedItem().(item); ok {
				e := i.entry
				m.selected = &e
				return m, tea.Quit
			}
		case "ctrl+c", "b":
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-2)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m menuModel) View() string {
	if m.quitting {
		return ""
	}
	return m.list.View()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func imax(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func imin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("Loading OData entities from", baseURL, "…")
	entries, err := loadEntities()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "No entities found.")
		os.Exit(1)
	}
	fmt.Printf("Found %d entities.\n\n", len(entries))

	for {
		menu := newMenu(entries)
		p := tea.NewProgram(menu, tea.WithAltScreen())
		result, err := p.Run()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		m, ok := result.(menuModel)
		if !ok || m.quitting || m.selected == nil {
			fmt.Println("Goodbye.")
			return
		}

		pager := newPager(*m.selected)
		p2 := tea.NewProgram(pager, tea.WithAltScreen())
		if _, err := p2.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
