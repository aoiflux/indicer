package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/vmihailenco/msgpack/v5"
)

type ListModel struct {
	files     []FileInfo
	db        *badger.DB
	restoreFn func(hash string, restorePath string) error
	width     int
	height    int
	loading   bool
	spinner   spinner.Model
	err       error
	selected  int
	status    string
	restoring bool
}

type FileInfo struct {
	Hash string
	Size string
	Time string
}

type listRestoreCompletedMsg struct {
	err  error
	path string
}

func NewListModel(db *badger.DB, restoreFn func(hash string, restorePath string) error) *ListModel {
	s := spinner.New()
	s.Spinner = spinner.Dot

	return &ListModel{
		db:        db,
		restoreFn: restoreFn,
		width:     80,
		height:    24,
		loading:   true,
		spinner:   s,
	}
}

func (m *ListModel) Resize(width int, height int) {
	m.width = width
	m.height = height
}

func (m ListModel) Init() tea.Cmd {
	m.loading = true
	m.err = nil
	m.files = nil
	m.status = ""
	m.selected = 0
	return m.loadFiles()
}

func (m ListModel) loadFiles() tea.Cmd {
	return func() tea.Msg {
		files, err := loadEvidenceFiles(m.db)
		return FileListLoadedMsg{files: files, err: err}
	}
}

func loadEvidenceFiles(db *badger.DB) ([]FileInfo, error) {
	files := make([]FileInfo, 0)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 256
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(cnst.EviFileNamespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			val, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(val, nil)
			if err == nil {
				val = decoded
			}

			var evi structs.EvidenceFile
			if err := msgpack.Unmarshal(val, &evi); err != nil {
				return err
			}
			if !evi.Completed {
				continue
			}

			hash := bytes.TrimPrefix(key, prefix)
			hashStr := base64.StdEncoding.EncodeToString(hash)

			files = append(files, FileInfo{
				Hash: hashStr,
				Size: humanize.Bytes(uint64(evi.Size)),
				Time: "",
			})
		}

		return nil
	})

	return files, err
}

type FileListLoadedMsg struct {
	files []FileInfo
	err   error
}

func (m ListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case listRestoreCompletedMsg:
		m.restoring = false
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
		} else {
			m.status = "Restored to " + msg.path
		}
		return m, nil
	case tea.KeyMsg:
		if m.loading {
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "esc", "q":
			return m, tea.Quit
		case "up", "k":
			if len(m.files) > 0 && m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if len(m.files) > 0 && m.selected < len(m.files)-1 {
				m.selected++
			}
		case "c":
			if len(m.files) == 0 {
				return m, nil
			}
			hash := m.files[m.selected].Hash
			if err := clipboard.WriteAll(hash); err != nil {
				m.status = "Error copying hash: " + err.Error()
			} else {
				m.status = "Copied hash to clipboard"
			}
		case "r", "enter":
			if len(m.files) == 0 || m.restoring {
				return m, nil
			}
			if m.restoreFn == nil {
				m.status = "Error: restore action not configured"
				return m, nil
			}
			hash := m.files[m.selected].Hash
			path := defaultRestorePath(hash)
			m.restoring = true
			m.status = "Restoring selected file..."
			return m, runListRestoreCmd(m.restoreFn, hash, path)
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case FileListLoadedMsg:
		m.loading = false
		m.files = msg.files
		m.err = msg.err
		if m.selected >= len(m.files) {
			m.selected = 0
		}
	}

	return m, nil
}

func (m ListModel) View() tea.View {
	title := TitleStyle.Render("Files in DUES Database")

	if m.loading {
		return tea.NewView(BorderStyle.Render(
			lipgloss.JoinVertical(
				lipgloss.Left,
				title,
				"",
				m.spinner.View()+" Loading files...",
			),
		))
	}

	if m.err != nil {
		return tea.NewView(BorderStyle.Render(
			lipgloss.JoinVertical(
				lipgloss.Left,
				title,
				"",
				ErrorStyle.Render(fmt.Sprintf("Error: %v", m.err)),
			),
		))
	}

	if len(m.files) == 0 {
		return tea.NewView(BorderStyle.Render(
			lipgloss.JoinVertical(
				lipgloss.Left,
				title,
				"",
				InfoStyle.Render("No files found in database"),
			),
		))
	}

	// Build file list
	fileEntries := []string{
		lipgloss.NewStyle().
			Bold(true).
			Render(fmt.Sprintf("%-60s %-15s", "Hash", "Size")),
	}

	for i, f := range m.files {
		entry := fmt.Sprintf("%-60s %-15s", f.Hash, f.Size)
		if i == m.selected {
			entry = SelectedItemStyle.Render(entry)
		}
		fileEntries = append(fileEntries, entry)
	}

	fileList := lipgloss.JoinVertical(lipgloss.Left, fileEntries...)

	help := HelpStyle.Render("up/down: Select | enter/r: Restore selected | c: Copy hash | esc: Back")
	status := ""
	if m.status != "" {
		status = InfoStyle.Render(m.status)
	}

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		fileList,
		"",
		status,
		"",
		help,
	)

	frame := BorderStyle
	if m.width > 0 {
		frame = frame.Width(max(40, m.width-4))
	}
	if m.height > 0 {
		frame = frame.MaxHeight(max(12, m.height-2))
	}

	return tea.NewView(frame.Render(content))
}

func defaultRestorePath(hash string) string {
	name := hash
	if len(name) > 16 {
		name = name[:16]
	}
	replacer := strings.NewReplacer("/", "_", "+", "-", "=", "")
	return "restored_" + replacer.Replace(name) + ".bin"
}

func runListRestoreCmd(restoreFn func(hash string, restorePath string) error, hash string, path string) tea.Cmd {
	return func() tea.Msg {
		return listRestoreCompletedMsg{err: restoreFn(hash, path), path: path}
	}
}
