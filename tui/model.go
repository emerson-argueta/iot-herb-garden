package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// validPlantID matches identifiers safe for use as MQTT topic segments.
// Allows letters, digits, underscores, and hyphens; no spaces or slashes.
var validPlantID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ── Wire types (match brain's garden/state JSON) ──────────────────────────────

// StatePayload is the master state published by the brain every 5 s.
type StatePayload struct {
	Timestamp            int64                `json:"timestamp"`
	Plants               map[string]PlantState `json:"plants"`
	UnprovisionedDevices []string             `json:"unprovisioned_devices"`
}

// PlantState is the per-plant snapshot inside StatePayload.
type PlantState struct {
	DisplayName   string  `json:"display_name,omitempty"`
	Moisture      float64 `json:"moisture"`
	Temp          float64 `json:"temp"`
	Watering      bool    `json:"watering"`
	InCooldown    bool    `json:"in_cooldown"`
	MoistureAlert bool    `json:"moisture_alert"`
	TempAlert     bool    `json:"temp_alert"`
	AlertMsg      string  `json:"alert_msg,omitempty"`
	LastSeen      int64   `json:"last_seen"` // Unix timestamp
}

// adminPayload is published to garden/admin to provision a new plant.
type adminPayload struct {
	Action          string  `json:"action"`
	MAC             string  `json:"mac"`
	PlantID         string  `json:"plant_id"`
	DisplayName     string  `json:"display_name"`
	MinMoisture     float64 `json:"min_moisture"`
	MaxMoisture     float64 `json:"max_moisture"`
	MinTemp         float64 `json:"min_temp"`
	MaxTemp         float64 `json:"max_temp"`
	CooldownSeconds int     `json:"cooldown_seconds"`
}

// ── Tea messages ──────────────────────────────────────────────────────────────

type stateMsg StatePayload
type connMsg bool           // true = connected, false = disconnected
type provisionDoneMsg string // the new plant_id on success
type provisionErrMsg string  // error message on failure

// ── Provisioning form ─────────────────────────────────────────────────────────

const (
	iPlantID = iota
	iDisplayName
	iMinMoisture
	iMaxMoisture
	iMinTemp
	iMaxTemp
	iCooldown
	numInputs
)

var inputLabels = [numInputs]string{
	"Plant ID        ",
	"Display Name    ",
	"Min Moisture %  ",
	"Max Moisture %  ",
	"Min Temp °C     ",
	"Max Temp °C     ",
	"Cooldown (sec)  ",
}

var inputDefaults = [numInputs]string{
	"", "", "20", "60", "15", "30", "1200",
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	alertStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	pumpOnStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	helpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	statusStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	errorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
)

// ── Model ─────────────────────────────────────────────────────────────────────

type model struct {
	stateCh <-chan StatePayload
	connCh  <-chan bool
	mqtt    pahomqtt.Client

	state     StatePayload
	updated   time.Time
	connected bool // false = connecting / reconnecting

	// Cursor for the unprovisioned-devices list.
	cursor int

	// Provisioning form state.
	provisioning bool
	provMAC      string
	inputs       [numInputs]textinput.Model
	focusIdx     int

	showHelp  bool
	statusMsg string
	isErr     bool
	width     int
}

func newModel(stateCh <-chan StatePayload, connCh <-chan bool, mqttClient pahomqtt.Client) model {
	return model{stateCh: stateCh, connCh: connCh, mqtt: mqttClient}
}

// waitForState returns a Cmd that blocks until the next state arrives.
func waitForState(ch <-chan StatePayload) tea.Cmd {
	return func() tea.Msg { return stateMsg(<-ch) }
}

// waitForConn returns a Cmd that blocks until the next connect/disconnect event.
func waitForConn(ch <-chan bool) tea.Cmd {
	return func() tea.Msg { return connMsg(<-ch) }
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m model) Init() tea.Cmd {
	return tea.Batch(waitForState(m.stateCh), waitForConn(m.connCh))
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width

	case connMsg:
		m.connected = bool(msg)
		return m, waitForConn(m.connCh)

	case stateMsg:
		m.state = StatePayload(msg)
		m.updated = time.Now()
		sorted := sortedUnprovisioned(m.state)
		if m.cursor >= len(sorted) && len(sorted) > 0 {
			m.cursor = len(sorted) - 1
		}
		return m, waitForState(m.stateCh)

	case provisionDoneMsg:
		m.statusMsg = fmt.Sprintf("Provisioned plant %q", string(msg))
		m.isErr = false
		m.provisioning = false

	case provisionErrMsg:
		m.statusMsg = string(msg)
		m.isErr = true

	case tea.KeyMsg:
		if m.showHelp {
			return m.updateHelp(msg)
		}
		if m.provisioning {
			return m.updateProvisioning(msg)
		}
		return m.updateNormal(msg)
	}

	return m, nil
}

func (m model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	unprov := sortedUnprovisioned(m.state)
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = true
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(unprov)-1 {
			m.cursor++
		}
	case "p":
		if len(unprov) > 0 {
			m.provMAC = unprov[m.cursor]
			m.inputs = makeInputs()
			m.focusIdx = 0
			m.inputs[0].Focus()
			m.provisioning = true
			m.statusMsg = ""
			m.isErr = false
			return m, textinput.Blink
		}
	}
	return m, nil
}

func (m model) updateHelp(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	m.showHelp = false
	return m, nil
}

func (m model) updateProvisioning(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.provisioning = false
		return m, nil
	case "tab", "down":
		m.inputs[m.focusIdx].Blur()
		m.focusIdx = (m.focusIdx + 1) % numInputs
		m.inputs[m.focusIdx].Focus()
		cmds = append(cmds, textinput.Blink)
	case "shift+tab", "up":
		m.inputs[m.focusIdx].Blur()
		m.focusIdx = (m.focusIdx - 1 + numInputs) % numInputs
		m.inputs[m.focusIdx].Focus()
		cmds = append(cmds, textinput.Blink)
	case "enter":
		if m.focusIdx == numInputs-1 {
			return m, m.submitProvision()
		}
		m.inputs[m.focusIdx].Blur()
		m.focusIdx++
		m.inputs[m.focusIdx].Focus()
		cmds = append(cmds, textinput.Blink)
	default:
		var cmd tea.Cmd
		m.inputs[m.focusIdx], cmd = m.inputs[m.focusIdx].Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m model) submitProvision() tea.Cmd {
	// Capture the values now (closures over model fields).
	mac := m.provMAC
	vals := [numInputs]string{}
	for i := range vals {
		vals[i] = strings.TrimSpace(m.inputs[i].Value())
	}
	mqtt := m.mqtt

	return func() tea.Msg {
		plantID := vals[iPlantID]
		displayName := vals[iDisplayName]
		if plantID == "" || displayName == "" {
			return provisionErrMsg("plant ID and display name are required")
		}
		if !validPlantID.MatchString(plantID) {
			return provisionErrMsg("plant ID may only contain letters, digits, _ and -")
		}
		minMoisture, e1 := strconv.ParseFloat(vals[iMinMoisture], 64)
		maxMoisture, e2 := strconv.ParseFloat(vals[iMaxMoisture], 64)
		minTemp, e3 := strconv.ParseFloat(vals[iMinTemp], 64)
		maxTemp, e4 := strconv.ParseFloat(vals[iMaxTemp], 64)
		cooldown, e5 := strconv.Atoi(vals[iCooldown])
		for _, e := range []error{e1, e2, e3, e4, e5} {
			if e != nil {
				return provisionErrMsg("invalid number: " + e.Error())
			}
		}
		if minMoisture >= maxMoisture {
			return provisionErrMsg("min moisture must be less than max moisture")
		}
		if minTemp >= maxTemp {
			return provisionErrMsg("min temp must be less than max temp")
		}

		p := adminPayload{
			Action:          "new_plant",
			MAC:             mac,
			PlantID:         plantID,
			DisplayName:     displayName,
			MinMoisture:     minMoisture,
			MaxMoisture:     maxMoisture,
			MinTemp:         minTemp,
			MaxTemp:         maxTemp,
			CooldownSeconds: cooldown,
		}
		data, _ := json.Marshal(p)
		tok := mqtt.Publish("garden/admin", 1, false, data)
		tok.Wait()
		if err := tok.Error(); err != nil {
			return provisionErrMsg("MQTT publish failed: " + err.Error())
		}
		return provisionDoneMsg(plantID)
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m model) View() string {
	if m.showHelp {
		return m.viewHelp()
	}
	if m.provisioning {
		return m.viewProvisioning()
	}
	return m.viewNormal()
}

func (m model) viewNormal() string {
	var b strings.Builder
	w := m.width
	if w == 0 {
		w = 80
	}
	divider := strings.Repeat("─", min(w, 72))

	// Header
	connStr := dimStyle.Render("connecting…")
	if m.connected {
		connStr = okStyle.Render("connected")
	}
	ts := ""
	if !m.updated.IsZero() {
		ts = dimStyle.Render("  updated " + m.updated.Format("15:04:05"))
	}
	fmt.Fprintf(&b, "%s  %s%s\n%s\n\n",
		titleStyle.Render("Herb Garden Monitor"), connStr, ts, divider)

	// Plants
	b.WriteString(sectionStyle.Render("PLANTS") + "\n\n")
	plantIDs := sortedPlantIDs(m.state)
	if len(plantIDs) == 0 {
		b.WriteString(dimStyle.Render("  Waiting for data from brain…") + "\n")
	}
	for _, id := range plantIDs {
		b.WriteString(renderPlant(id, m.state.Plants[id]))
	}

	// Unprovisioned devices
	unprov := sortedUnprovisioned(m.state)
	if len(unprov) > 0 {
		fmt.Fprintf(&b, "\n%s\n\n", sectionStyle.Render("UNPROVISIONED DEVICES"))
		for i, mac := range unprov {
			if i == m.cursor {
				fmt.Fprintf(&b, "  %s %s\n", cursorStyle.Render("▶"), cursorStyle.Render(mac))
			} else {
				fmt.Fprintf(&b, "    %s\n", mac)
			}
		}
	}

	// Status / error
	if m.statusMsg != "" {
		b.WriteString("\n")
		if m.isErr {
			b.WriteString(errorStyle.Render(m.statusMsg))
		} else {
			b.WriteString(statusStyle.Render(m.statusMsg))
		}
		b.WriteString("\n")
	}

	// Footer
	b.WriteString("\n" + divider + "\n")
	if len(unprov) > 0 {
		b.WriteString(helpStyle.Render("[q] quit  [↑↓/jk] select  [p] provision  [?] help"))
	} else {
		b.WriteString(helpStyle.Render("[q] quit  [?] help"))
	}

	return b.String()
}

func (m model) viewProvisioning() string {
	var b strings.Builder
	divider := strings.Repeat("─", 52)

	fmt.Fprintf(&b, "%s\n%s\n%s\n\n",
		titleStyle.Render("Provision Device"),
		dimStyle.Render("MAC: "+m.provMAC),
		divider,
	)

	for i := 0; i < numInputs; i++ {
		label := inputLabels[i]
		if i == m.focusIdx {
			fmt.Fprintf(&b, "  %s %s\n", cursorStyle.Render(label), m.inputs[i].View())
		} else {
			fmt.Fprintf(&b, "  %s %s\n", dimStyle.Render(label), m.inputs[i].View())
		}
	}

	b.WriteString("\n")
	if m.statusMsg != "" {
		if m.isErr {
			b.WriteString(errorStyle.Render("Error: "+m.statusMsg) + "\n\n")
		} else {
			b.WriteString(statusStyle.Render(m.statusMsg) + "\n\n")
		}
	}
	b.WriteString(helpStyle.Render("[Tab/↓] next  [Shift+Tab/↑] prev  [Enter] next/submit  [Esc] cancel"))

	return b.String()
}

func (m model) viewHelp() string {
	var b strings.Builder
	divider := strings.Repeat("─", 52)

	fmt.Fprintf(&b, "%s\n%s\n\n", titleStyle.Render("Herb Garden Monitor — Help"), divider)

	b.WriteString(sectionStyle.Render("MAIN VIEW") + "\n\n")
	for _, r := range [][2]string{
		{"[↑] [k]", "Move cursor up in unprovisioned list"},
		{"[↓] [j]", "Move cursor down in unprovisioned list"},
		{"[p]", "Provision selected device"},
		{"[?]", "Show this help screen"},
		{"[q] [Ctrl+C]", "Quit"},
	} {
		fmt.Fprintf(&b, "  %-22s %s\n", dimStyle.Render(r[0]), r[1])
	}

	b.WriteString("\n" + sectionStyle.Render("PROVISIONING FORM") + "\n\n")
	for _, r := range [][2]string{
		{"[Tab] [↓]", "Next field"},
		{"[Shift+Tab] [↑]", "Previous field"},
		{"[Enter]", "Advance field; submit on last"},
		{"[Esc]", "Cancel and return to main view"},
	} {
		fmt.Fprintf(&b, "  %-22s %s\n", dimStyle.Render(r[0]), r[1])
	}

	b.WriteString("\n" + divider + "\n")
	b.WriteString(helpStyle.Render("Press any key to close"))
	return b.String()
}

// ── Render helpers ────────────────────────────────────────────────────────────

func renderPlant(id string, ps PlantState) string {
	var b strings.Builder

	// First line: name (id) + alerts + last-seen
	label := id
	if ps.DisplayName != "" {
		label = ps.DisplayName + " (" + id + ")"
	}

	var alerts []string
	if ps.MoistureAlert {
		alerts = append(alerts, alertStyle.Render("⚠ MOISTURE"))
	}
	if ps.TempAlert {
		alerts = append(alerts, alertStyle.Render("⚠ TEMP"))
	}

	lastSeenStr := ""
	if ps.LastSeen > 0 {
		d := time.Since(time.Unix(ps.LastSeen, 0))
		lastSeenStr = "  " + dimStyle.Render(agoStr(d))
	}

	alertStr := ""
	if len(alerts) > 0 {
		alertStr = "  " + strings.Join(alerts, "  ")
	}
	fmt.Fprintf(&b, "  %s%s%s\n", sectionStyle.Render(label), alertStr, lastSeenStr)

	// Second line: moisture bar + temp + pump
	moistureStyle := okStyle
	if ps.MoistureAlert {
		moistureStyle = alertStyle
	}
	tempStyle := okStyle
	if ps.TempAlert {
		tempStyle = alertStyle
	}

	pumpStr := dimStyle.Render("Pump OFF")
	if ps.Watering {
		label := "Pump ON"
		if ps.InCooldown {
			label = "Pump ON (cooldown)"
		}
		pumpStr = pumpOnStyle.Render(label)
	}

	bar := moistureBar(ps.Moisture, 14)
	fmt.Fprintf(&b, "  Moisture %s%s  Temp %s  %s\n\n",
		moistureStyle.Render(fmt.Sprintf("%5.1f%%", ps.Moisture)),
		bar,
		tempStyle.Render(fmt.Sprintf("%.1f°C", ps.Temp)),
		pumpStr,
	)
	return b.String()
}

func moistureBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100.0 * float64(width))
	return " [" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func agoStr(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return "offline"
	}
}

// ── Form helpers ──────────────────────────────────────────────────────────────

func makeInputs() [numInputs]textinput.Model {
	var inputs [numInputs]textinput.Model
	for i := range inputs {
		t := textinput.New()
		t.SetValue(inputDefaults[i])
		t.CharLimit = 64
		inputs[i] = t
	}
	return inputs
}

// ── Sort helpers ──────────────────────────────────────────────────────────────

func sortedPlantIDs(s StatePayload) []string {
	ids := make([]string, 0, len(s.Plants))
	for id := range s.Plants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedUnprovisioned(s StatePayload) []string {
	out := make([]string, len(s.UnprovisionedDevices))
	copy(out, s.UnprovisionedDevices)
	sort.Strings(out)
	return out
}
