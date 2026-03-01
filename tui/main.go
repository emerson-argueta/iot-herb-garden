package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Master state structures (matches brain output)
type PlantState struct {
	Moisture      float64 `json:"moisture"`
	Temp          float64 `json:"temp"`
	MoistureAlert bool    `json:"moisture_alert"`
	TempAlert     bool    `json:"temp_alert"`
	AlertMsg      string  `json:"alert_msg,omitempty"`
}

type MasterState struct {
	Timestamp            int64                 `json:"timestamp"`
	Plants               map[string]PlantState `json:"plants"`
	UnprovisionedDevices []string              `json:"unprovisioned_devices"`
}

// Message types for Bubble Tea
type stateUpdateMsg MasterState
type mqttErrorMsg error

// Model
type model struct {
	state            MasterState
	mqttClient       mqtt.Client
	provisioningMode bool
	macInput         textinput.Model
	plantIDInput     textinput.Model
	minMoistInput    textinput.Model
	maxMoistInput    textinput.Model
	minTempInput     textinput.Model
	maxTempInput     textinput.Model
	focusedInput     int
	err              error
}

func initialModel(client mqtt.Client) model {
	macInput := textinput.New()
	macInput.Placeholder = "MAC Address"
	macInput.Focus()
	macInput.CharLimit = 17
	macInput.Width = 20

	plantIDInput := textinput.New()
	plantIDInput.Placeholder = "Plant ID (e.g., thyme_1)"
	plantIDInput.CharLimit = 50
	plantIDInput.Width = 30

	minMoistInput := textinput.New()
	minMoistInput.Placeholder = "Min Moisture %"
	minMoistInput.CharLimit = 5
	minMoistInput.Width = 15

	maxMoistInput := textinput.New()
	maxMoistInput.Placeholder = "Max Moisture %"
	maxMoistInput.CharLimit = 5
	maxMoistInput.Width = 15

	minTempInput := textinput.New()
	minTempInput.Placeholder = "Min Temp °C"
	minTempInput.CharLimit = 5
	minTempInput.Width = 15

	maxTempInput := textinput.New()
	maxTempInput.Placeholder = "Max Temp °C"
	maxTempInput.CharLimit = 5
	maxTempInput.Width = 15

	return model{
		mqttClient:   client,
		macInput:     macInput,
		plantIDInput: plantIDInput,
		minMoistInput: minMoistInput,
		maxMoistInput: maxMoistInput,
		minTempInput:  minTempInput,
		maxTempInput:  maxTempInput,
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit

		case "p":
			if !m.provisioningMode && len(m.state.UnprovisionedDevices) > 0 {
				m.provisioningMode = true
				m.focusedInput = 0
				m.macInput.Focus()
				return m, textinput.Blink
			}

		case "esc":
			if m.provisioningMode {
				m.provisioningMode = false
				m.macInput.Blur()
				m.plantIDInput.Blur()
				m.minMoistInput.Blur()
				m.maxMoistInput.Blur()
				m.minTempInput.Blur()
				m.maxTempInput.Blur()
			}

		case "tab", "shift+tab":
			if m.provisioningMode {
				if msg.String() == "tab" {
					m.focusedInput = (m.focusedInput + 1) % 6
				} else {
					m.focusedInput = (m.focusedInput - 1 + 6) % 6
				}
				m.updateInputFocus()
			}

		case "enter":
			if m.provisioningMode {
				return m, m.submitProvisioning()
			}
		}

	case stateUpdateMsg:
		m.state = MasterState(msg)
		return m, nil

	case mqttErrorMsg:
		m.err = msg
		return m, nil
	}

	// Update active text input
	if m.provisioningMode {
		var cmd tea.Cmd
		switch m.focusedInput {
		case 0:
			m.macInput, cmd = m.macInput.Update(msg)
		case 1:
			m.plantIDInput, cmd = m.plantIDInput.Update(msg)
		case 2:
			m.minMoistInput, cmd = m.minMoistInput.Update(msg)
		case 3:
			m.maxMoistInput, cmd = m.maxMoistInput.Update(msg)
		case 4:
			m.minTempInput, cmd = m.minTempInput.Update(msg)
		case 5:
			m.maxTempInput, cmd = m.maxTempInput.Update(msg)
		}
		return m, cmd
	}

	return m, nil
}

func (m *model) updateInputFocus() {
	inputs := []*textinput.Model{
		&m.macInput, &m.plantIDInput, &m.minMoistInput,
		&m.maxMoistInput, &m.minTempInput, &m.maxTempInput,
	}
	for i, input := range inputs {
		if i == m.focusedInput {
			input.Focus()
		} else {
			input.Blur()
		}
	}
}

func (m model) submitProvisioning() tea.Cmd {
	return func() tea.Msg {
		payload := map[string]interface{}{
			"action":       "new_plant",
			"mac":          m.macInput.Value(),
			"plant_id":     m.plantIDInput.Value(),
			"min_moisture": m.minMoistInput.Value(),
			"max_moisture": m.maxMoistInput.Value(),
			"min_temp":     m.minTempInput.Value(),
			"max_temp":     m.maxTempInput.Value(),
		}

		data, err := json.Marshal(payload)
		if err != nil {
			return mqttErrorMsg(err)
		}

		token := m.mqttClient.Publish("garden/admin", 0, false, data)
		token.Wait()

		if token.Error() != nil {
			return mqttErrorMsg(token.Error())
		}

		return nil
	}
}

func (m model) View() string {
	var b strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("10")).
		Background(lipgloss.Color("235")).
		Padding(0, 1)

	b.WriteString(headerStyle.Render("IoT Herb Garden Monitor - Dashboard"))
	b.WriteString("\n\n")

	// Error display
	if m.err != nil {
		errorStyle := lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n\n")
	}

	// Provisioning mode
	if m.provisioningMode {
		b.WriteString(m.renderProvisioningForm())
	} else {
		// Dashboard view
		if len(m.state.UnprovisionedDevices) > 0 {
			b.WriteString(m.renderUnprovisionedAlert())
			b.WriteString("\n\n")
		}

		if len(m.state.Plants) == 0 {
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Render("No plants configured yet. Press 'p' to provision a device."))
		} else {
			b.WriteString(m.renderPlantGrid())
		}

		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Render("Press 'p' to provision | 'q' to quit"))
	}

	return b.String()
}

func (m model) renderUnprovisionedAlert() string {
	alertStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("11")).
		Background(lipgloss.Color("58")).
		Bold(true).
		Padding(1, 2)

	msg := fmt.Sprintf("⚠ %d Unprovisioned Device(s): %s",
		len(m.state.UnprovisionedDevices),
		strings.Join(m.state.UnprovisionedDevices, ", "))

	return alertStyle.Render(msg)
}

func (m model) renderPlantGrid() string {
	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(1, 2).
		Width(35)

	okStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	alertStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)

	var cards []string
	for plantID, state := range m.state.Plants {
		var b strings.Builder

		// Plant name header
		nameStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("14"))
		b.WriteString(nameStyle.Render(plantID))
		b.WriteString("\n\n")

		// Moisture
		moistStyle := okStyle
		if state.MoistureAlert {
			moistStyle = alertStyle
		}
		b.WriteString(moistStyle.Render(fmt.Sprintf("Moisture: %.1f%%", state.Moisture)))
		b.WriteString("\n")

		// Temperature
		tempStyle := okStyle
		if state.TempAlert {
			tempStyle = alertStyle
		}
		b.WriteString(tempStyle.Render(fmt.Sprintf("Temp: %.1f°C", state.Temp)))
		b.WriteString("\n")

		// Alert message
		if state.AlertMsg != "" {
			b.WriteString("\n")
			b.WriteString(alertStyle.Render("⚠ " + state.AlertMsg))
		}

		cards = append(cards, cardStyle.Render(b.String()))
	}

	// Layout cards in rows of 2
	var rows []string
	for i := 0; i < len(cards); i += 2 {
		if i+1 < len(cards) {
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards[i], "  ", cards[i+1]))
		} else {
			rows = append(rows, cards[i])
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m model) renderProvisioningForm() string {
	formStyle := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("12")).
		Padding(1, 2).
		Width(60)

	var b strings.Builder

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("12"))

	b.WriteString(titleStyle.Render("Provision New Device"))
	b.WriteString("\n\n")

	b.WriteString("MAC Address:\n")
	b.WriteString(m.macInput.View())
	b.WriteString("\n\n")

	b.WriteString("Plant ID:\n")
	b.WriteString(m.plantIDInput.View())
	b.WriteString("\n\n")

	b.WriteString("Min Moisture (%):\n")
	b.WriteString(m.minMoistInput.View())
	b.WriteString("\n\n")

	b.WriteString("Max Moisture (%):\n")
	b.WriteString(m.maxMoistInput.View())
	b.WriteString("\n\n")

	b.WriteString("Min Temp (°C):\n")
	b.WriteString(m.minTempInput.View())
	b.WriteString("\n\n")

	b.WriteString("Max Temp (°C):\n")
	b.WriteString(m.maxTempInput.View())
	b.WriteString("\n\n")

	b.WriteString(lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Render("Tab: Next Field | Enter: Submit | Esc: Cancel"))

	return formStyle.Render(b.String())
}

func subscribeToState(client mqtt.Client, program *tea.Program) {
	handler := func(_ mqtt.Client, msg mqtt.Message) {
		var state MasterState
		if err := json.Unmarshal(msg.Payload(), &state); err != nil {
			program.Send(mqttErrorMsg(err))
			return
		}
		program.Send(stateUpdateMsg(state))
	}

	token := client.Subscribe("garden/state", 0, handler)
	token.Wait()
	if token.Error() != nil {
		log.Fatalf("Failed to subscribe to garden/state: %v", token.Error())
	}
}

func main() {
	// Setup MQTT client
	opts := mqtt.NewClientOptions()
	opts.AddBroker("tcp://localhost:1883")
	opts.SetClientID("garden_tui")
	opts.SetAutoReconnect(true)

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to connect to MQTT broker: %v", token.Error())
	}

	// Create Bubble Tea program
	p := tea.NewProgram(initialModel(client), tea.WithAltScreen())

	// Subscribe to state updates
	subscribeToState(client, p)

	// Run the program
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}
