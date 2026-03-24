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

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/net/html"
)

// Version is set at build time via -ldflags "-X main.version=x.y.z"
var version = "dev"

var baseURL = "http://localhost:4004"

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
	Name       string     `xml:"Name,attr"`
	Key        EntityKey  `xml:"Key"`
	Properties []Property `xml:"Property"`
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

func post(url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
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

func del(url string) (int, []byte, error) {
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return 0, nil, err
	}
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
	keyFields []string          // OData key property names
	propTypes map[string]string // property name → EDM type
}

func parseMetadata(body []byte) map[string]entityMeta {
	result := map[string]entityMeta{}
	var edmx Edmx
	if err := xml.Unmarshal(body, &edmx); err != nil {
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
	EntityName  string
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
	modeNav    pagerMode = iota
	modeEdit             // editing a single cell (PATCH)
	modeInsert           // insert-form: filling fields for a new entity (POST)
)

// ── Pager model ───────────────────────────────────────────────────────────────

// insertField holds the label and current value for one field in the insert form.
type insertField struct {
	name    string
	buf     []rune
	editOff int
	editing bool
}

type pagerModel struct {
	entity       EntityEntry
	skip         int
	rows         [][]string
	header       []string
	fetchErr     string
	done         bool
	selRow       int
	selCol       int
	colOff       int
	winCols      int
	winRows      int
	mode         pagerMode
	editBuf      []rune
	editOff      int // first visible rune index in the edit viewport (cell edit)
	statusMsg    string
	statusErr    bool
	insertFields []insertField // fields for insert form
	insertSel    int           // selected field index in insert form
}

func newPager(e EntityEntry) pagerModel {
	m := pagerModel{entity: e, winCols: 120, winRows: 40}
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

// buildPatchURL constructs the OData key-predicate URL for the selected row,
// e.g. /Books(1) for a single numeric key, /Books('foo') for a string key,
// or /Orders(CustomerID=1,ProductID=2) for compound keys.
func (m *pagerModel) buildPatchURL() (string, error) {
	keys := m.entity.Meta.keyFields
	if len(keys) == 0 {
		return "", fmt.Errorf("no key fields found in metadata for %s", m.entity.EntityName)
	}
	row := m.rows[m.selRow]

	// Map each key name to its column index
	keyIdx := make(map[string]int, len(keys))
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
		keyIdx[k] = idx
	}

	var predicate string
	if len(keys) == 1 {
		// Single-key shorthand: just the bare value (quoted if string)
		k := keys[0]
		val := row[keyIdx[k]]
		if isStringType(m.entity.Meta.propTypes[k]) {
			predicate = fmt.Sprintf("'%s'", val)
		} else {
			predicate = val
		}
	} else {
		// Compound key: Name=value,Name=value,...
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			val := row[keyIdx[k]]
			if isStringType(m.entity.Meta.propTypes[k]) {
				parts = append(parts, fmt.Sprintf("%s='%s'", k, val))
			} else {
				parts = append(parts, fmt.Sprintf("%s=%s", k, val))
			}
		}
		predicate = strings.Join(parts, ",")
	}
	return fmt.Sprintf("%s(%s)", m.entity.URL, predicate), nil
}

func isStringType(edm string) bool {
	return strings.Contains(strings.ToLower(edm), "string") || edm == ""
}

func odataErrMsg(body []byte, fallback string) string {
	var e struct {
		Error struct{ Message string `json:"message"` } `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return fallback + ": " + e.Error.Message
	}
	return fallback
}

func (m *pagerModel) deleteEntity() {
	deleteURL, err := m.buildPatchURL() // same key-predicate logic
	if err != nil {
		m.statusMsg = "DELETE error: " + err.Error()
		m.statusErr = true
		return
	}
	status, body, err := del(deleteURL)
	if err != nil {
		m.statusMsg = "DELETE error: " + err.Error()
		m.statusErr = true
		return
	}
	if status < 200 || status >= 300 {
		m.statusMsg = odataErrMsg(body, fmt.Sprintf("HTTP %d", status))
		m.statusErr = true
		return
	}
	m.statusMsg = "✓ Entity deleted"
	m.statusErr = false
	// Clamp cursor then re-fetch so the deleted row disappears
	if m.selRow > 0 {
		m.selRow--
	}
	m.fetch()
}

func (m *pagerModel) openInsertForm() {
	// Build one field per property, pre-populated with empty string.
	// Use the metadata property list when available, otherwise fall back
	// to the current response headers.
	fields := m.entity.Meta.propTypes
	var names []string
	if len(fields) > 0 {
		for k := range fields {
			names = append(names, k)
		}
		sort.Strings(names)
	} else {
		names = append(names, m.header...)
	}
	m.insertFields = make([]insertField, len(names))
	for i, n := range names {
		m.insertFields[i] = insertField{name: n}
	}
	m.insertSel = 0
	m.mode = modeInsert
}

func (m *pagerModel) commitInsert() {
	payload := map[string]interface{}{}
	for _, f := range m.insertFields {
		v := string(f.buf)
		if v != "" {
			payload[f.name] = v
		}
	}
	body, _ := json.Marshal(payload)
	status, respBody, err := post(m.entity.URL, body)
	if err != nil {
		m.statusMsg = "POST error: " + err.Error()
		m.statusErr = true
		m.mode = modeNav
		return
	}
	if status < 200 || status >= 300 {
		m.statusMsg = odataErrMsg(respBody, fmt.Sprintf("HTTP %d", status))
		m.statusErr = true
		m.mode = modeNav
		return
	}
	m.statusMsg = "✓ Entity inserted"
	m.statusErr = false
	m.mode = modeNav
	m.fetch()
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

	// Success — optimistically update cell then re-fetch to stay in sync
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
		m.winRows = msg.Height

	case tea.KeyMsg:
		// ── cell edit mode ────────────────────────────────────────────
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
				if r := []rune(msg.String()); len(r) == 1 && r[0] >= 32 {
					m.editBuf = append(m.editBuf, r[0])
				}
			}
			return m, nil
		}

		// ── insert form mode ──────────────────────────────────────────
		if m.mode == modeInsert {
			f := &m.insertFields[m.insertSel]
			if f.editing {
				switch msg.String() {
				case "enter", "esc":
					f.editing = false
				case "ctrl+c":
					os.Exit(0)
				case "backspace":
					if len(f.buf) > 0 {
						f.buf = f.buf[:len(f.buf)-1]
					}
				default:
					if r := []rune(msg.String()); len(r) == 1 && r[0] >= 32 {
						f.buf = append(f.buf, r[0])
					}
				}
			} else {
				switch msg.String() {
				case "enter":
					f.editing = true
				case "j": // field down
					if m.insertSel < len(m.insertFields)-1 {
						m.insertSel++
					}
				case "k": // field up
					if m.insertSel > 0 {
						m.insertSel--
					}
				case "s": // submit
					m.commitInsert()
				case "b", "esc":
					m.mode = modeNav
					m.statusMsg = ""
				case "ctrl+c":
					os.Exit(0)
				}
			}
			return m, nil
		}

		// ── nav mode ──────────────────────────────────────────────────
		switch msg.String() {
		case "enter":
			if len(m.rows) > 0 {
				m.editBuf = []rune(m.rows[m.selRow][m.selCol])
				m.editOff = 0
				m.statusMsg = ""
				m.mode = modeEdit
			}
		case "x":
			if len(m.rows) > 0 {
				m.deleteEntity()
			}
		case "i":
			m.openInsertForm()
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

// padOrTrunc right-pads or end-truncates s to exactly w runes.
func padOrTrunc(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		if w > 2 {
			return string(r[:w-1]) + "…"
		}
		return string(r[:w])
	}
	return s + strings.Repeat(" ", w-len(r))
}

// midTrunc truncates s to w runes keeping both ends, e.g. "1234…5678".
// Falls back to padOrTrunc when the value fits or w is too small to split.
func midTrunc(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s + strings.Repeat(" ", w-len(r))
	}
	if w < 5 {
		return padOrTrunc(s, w)
	}
	// keep roughly half on each side of the ellipsis
	half := (w - 1) / 2
	left := half
	right := w - 1 - left
	return string(r[:left]) + "…" + string(r[len(r)-right:])
}

// editViewport returns a fixed-width window into the edit buffer that
// always keeps the cursor (end of buf) visible, with a block cursor appended.
func editViewport(buf []rune, off *int, w int) string {
	// Reserve 1 char for the block cursor
	visible := w - 1
	if visible < 1 {
		visible = 1
	}
	// Advance offset if cursor has moved past the right edge
	cursor := len(buf)
	if cursor >= *off+visible {
		*off = cursor - visible
	}
	// Retreat offset if cursor moved before the left edge (e.g. backspace)
	if cursor < *off {
		*off = cursor
	}
	if *off < 0 {
		*off = 0
	}
	end := *off + visible
	if end > len(buf) {
		end = len(buf)
	}
	window := string(buf[*off:end])
	// Pad to fill visible width, then append cursor
	window += strings.Repeat(" ", visible-len([]rune(window)))
	return window + "█"
}

func (m pagerModel) View() string {
	var sb strings.Builder

	page := m.skip/pageSize + 1
	sb.WriteString(titleStyle.Render(fmt.Sprintf("Entity: %s  (Page %d)", m.entity.DisplayName, page)))
	sb.WriteString("\n")
	switch m.mode {
	case modeEdit:
		col := ""
		if m.selCol < len(m.header) {
			col = m.header[m.selCol]
		}
		sb.WriteString(hintStyle.Render(fmt.Sprintf("  EDIT [%s]  Enter=save  Esc=cancel", col)))
	case modeInsert:
		sb.WriteString(hintStyle.Render("  INSERT  j/k=field ↑/↓  Enter=edit field  s=submit  b/Esc=cancel"))
	default:
		sb.WriteString(hintStyle.Render("  [n/p] page  [r] refresh  [b] back  [j/k] row ↑/↓  [h/l] col ←/→  [Enter] edit  [i] insert  [x] delete"))
	}
	sb.WriteString("\n\n")

	// ── insert form (replaces table when active) ──────────────────────────────
	if m.mode == modeInsert {
		sb.WriteString(titleStyle.Render(fmt.Sprintf("  New %s", m.entity.EntityName)))
		sb.WriteString("\n\n")
		labelW := 0
		for _, f := range m.insertFields {
			if len([]rune(f.name)) > labelW {
				labelW = len([]rune(f.name))
			}
		}
		inputW := imax(20, m.winCols-labelW-10)
		for idx, f := range m.insertFields {
			label := padOrTrunc(f.name, labelW)
			var fieldStr string
			if f.editing {
				fieldStr = editStyle.Render(editViewport(f.buf, &m.insertFields[idx].editOff, inputW))
			} else if idx == m.insertSel {
				fieldStr = selCellStyle.Render(midTrunc(string(f.buf)+"█", inputW))
			} else {
				raw := string(f.buf)
				if raw == "" {
					raw = " "
				}
				fieldStr = midTrunc(raw, inputW)
			}
			if idx == m.insertSel {
				sb.WriteString(colHdrStyle.Render("  "+label+"  ") + fieldStr)
			} else {
				sb.WriteString(hintStyle.Render("  "+label+"  ") + fieldStr)
			}
			sb.WriteString("\n")
		}
		// status at bottom of form
		sb.WriteString("\n")
		if m.statusMsg != "" && m.statusErr {
			sb.WriteString(errStyle.Render("  " + m.statusMsg))
		} else {
			sb.WriteString(hintStyle.Render("  s=submit  b/Esc=cancel"))
		}
		return sb.String()
	}

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
			if m.mode == modeEdit && ri == m.selRow && absCI == m.selCol {
				// Show a scrolling viewport into the edit buffer
				cell := editViewport(m.editBuf, &m.editOff, colWidth)
				sb.WriteString(editStyle.Render(cell))
			} else {
				// Use mid-truncation for cells so both ends of long values are visible
				cell := midTrunc(row[absCI], colWidth)
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

	// ── bottom detail / status bar ────────────────────────────────────────────
	sb.WriteString("\n")
	if m.statusMsg != "" {
		// PATCH result takes priority
		if m.statusErr {
			sb.WriteString(errStyle.Render("  " + m.statusMsg))
		} else {
			sb.WriteString(okStyle.Render("  " + m.statusMsg))
		}
	} else if m.mode == modeEdit {
		// In edit mode show the full edit buffer value so the user can see
		// the whole string even though the cell viewport is narrow.
		full := string(m.editBuf)
		maxDetail := m.winCols - 4
		if maxDetail < 10 {
			maxDetail = 10
		}
		r := []rune(full)
		if len(r) > maxDetail {
			full = midTrunc(full, maxDetail)
		}
		sb.WriteString(hintStyle.Render(fmt.Sprintf("  value: %s", full)))
	} else if len(m.rows) > 0 && len(m.header) > 0 {
		// Nav mode: show full cell value for selected cell
		cellVal := m.rows[m.selRow][m.selCol]
		col := m.header[m.selCol]
		maxDetail := m.winCols - len(col) - 12
		if maxDetail < 10 {
			maxDetail = 10
		}
		r := []rune(cellVal)
		display := cellVal
		if len(r) > maxDetail {
			display = midTrunc(cellVal, maxDetail)
		}
		if len(m.header) > vis {
			sb.WriteString(hintStyle.Render(fmt.Sprintf(
				"  cols %d–%d of %d  │  %s: %s",
				m.colOff+1, end, len(m.header), col, display,
			)))
		} else {
			sb.WriteString(hintStyle.Render(fmt.Sprintf("  %s: %s", col, display)))
		}
	}

	return sb.String()
}

// ── Menu ──────────────────────────────────────────────────────────────────────

type item struct{ entry EntityEntry }

func (i item) Title() string       { return i.entry.DisplayName }
func (i item) Description() string { return i.entry.URL }
func (i item) FilterValue() string { return i.entry.DisplayName }

// refreshKey is the keybinding used both to handle the keypress and to
// advertise it in the bubbletea list help footer.
var refreshKey = key.NewBinding(
	key.WithKeys("r"),
	key.WithHelp("r", "refresh"),
)

type menuModel struct {
	list       list.Model
	selected   *EntityEntry
	quitting   bool
	statusMsg  string
	statusErr  bool
}

func newMenu(entries []EntityEntry) menuModel {
	items := make([]list.Item, len(entries))
	for i, e := range entries {
		items[i] = item{e}
	}
	l := list.New(items, list.NewDefaultDelegate(), 80, 24)
	l.Title = "CAP OData Browser  (" + baseURL + ")"
	l.Styles.Title = titleStyle
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{refreshKey}
	}
	return menuModel{list: l}
}

func (m menuModel) Init() tea.Cmd { return nil }

func (m menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, refreshKey):
			entries, err := loadEntities()
			if err != nil {
				// Show error in status bar; keep existing list intact
				m.statusMsg = "Refresh failed: " + err.Error()
				m.statusErr = true
				return m, nil
			}
			if len(entries) == 0 {
				m.statusMsg = "Refresh returned no entities"
				m.statusErr = true
				return m, nil
			}
			items := make([]list.Item, len(entries))
			for i, e := range entries {
				items[i] = item{e}
			}
			// SetItems returns a cmd; pass it through so the list can
			// run its internal filtering/sorting after the update.
			cmd := m.list.SetItems(items)
			m.statusMsg = fmt.Sprintf("✓ Refreshed — %d entities", len(entries))
			m.statusErr = false
			return m, cmd

		case msg.String() == "enter":
			if i, ok := m.list.SelectedItem().(item); ok {
				e := i.entry
				m.selected = &e
				return m, tea.Quit
			}

		case msg.String() == "ctrl+c" || msg.String() == "b":
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
	var sb strings.Builder
	sb.WriteString(m.list.View())
	if m.statusMsg != "" {
		sb.WriteString("\n")
		if m.statusErr {
			sb.WriteString(errStyle.Render("  " + m.statusMsg))
		} else {
			sb.WriteString(okStyle.Render("  " + m.statusMsg))
		}
	}
	return sb.String()
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

func run(url string) {
	baseURL = url
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
		// Carry updated entries back into the next menu iteration
		// so a mid-session refresh persists across pager visits.
		n := m.list.Items()
		entries = make([]EntityEntry, len(n))
		for i, it := range n {
			entries[i] = it.(item).entry
		}

		pager := newPager(*m.selected)
		p2 := tea.NewProgram(pager, tea.WithAltScreen())
		if _, err := p2.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func main() {
	var urlFlag string

	rootCmd := &cobra.Command{
		Use:   "cap_browser [URL]",
		Short: "Interactive terminal browser for CAP OData services",
		Long: `cap_browser connects to a SAP CAP OData service, discovers all
entities from the service metadata, and lets you browse and edit
them in an interactive terminal UI.

The target URL can be supplied as a positional argument or via the
--url flag. The positional argument takes priority.

Key bindings (pager):
  n / p        next / previous page
  j / k        row down / up  (wraps across pages)
  h / l        column left / right
  Enter        edit selected cell  (PATCH on save)
  i            insert new entity  (POST form)
  x            delete selected entity  (DELETE, immediate)
  r            refresh current page
  b / Esc      back to entity menu

Key bindings (menu):
  Enter        open entity
  r            refresh entity list
  b / Ctrl+C   quit`,
		Version: version,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := urlFlag
			if len(args) > 0 {
				url = args[0]
			}
			if url == "" {
				return fmt.Errorf("no URL provided — use --url or pass URL as argument")
			}
			run(url)
			return nil
		},
	}

	rootCmd.Flags().StringVarP(&urlFlag, "url", "u", "http://localhost:4004",
		"OData base URL to connect to")

	// Make --version also respond to -v
	rootCmd.Flags().BoolP("version", "v", false, "Print version and exit")
	rootCmd.SetVersionTemplate("cap_browser {{.Version}}\n")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
