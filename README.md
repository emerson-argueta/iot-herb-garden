# Hub-and-Spoke IoT Herb Garden Monitor

Local, air-gapped MQTT-based IoT system for monitoring herb garden sensors with central state management.

## Architecture

```
┌─────────────┐
│ ESP32 Edge  │──┐
│   Nodes     │  │
└─────────────┘  │
                 │    ┌──────────────┐      ┌─────────────┐
┌─────────────┐  ├────│  Mosquitto   │──────│  Go Brain   │
│ ESP32 Edge  │──┤    │  MQTT Broker │      │   Daemon    │
│   Nodes     │  │    └──────────────┘      └─────────────┘
└─────────────┘  │           │                      │
                 │           │                      │
┌─────────────┐  │           │                      │
│ ESP32 Edge  │──┘           │                      │
│   Nodes     │              │                      │
└─────────────┘              │                      │
                             │                      │
                    ┌────────┴──────────┬───────────┘
                    │                   │
              ┌─────▼──────┐     ┌──────▼──────┐
              │   Go TUI   │     │   M5Paper   │
              │   Client   │     │   E-Ink     │
              └────────────┘     └─────────────┘
```

## Components

### Component A: ESP32 Edge Nodes (ESP-IDF/C++)
- **Path**: `esp32_edge/`
- Reads moisture and temperature sensors
- Publishes telemetry every 15 minutes
- NVS-based provisioning with auto-assignment
- Deep sleep capable for battery operation

### Component B: Go Brain Daemon
- **Path**: `brain/`
- Central state manager and config keeper
- Listens to telemetry, tracks unprovisioned devices
- Computes alert flags from thresholds
- Publishes master state every 5 seconds
- Handles provisioning workflow

### Component C: Go TUI Client
- **Path**: `tui/`
- Terminal dashboard with Bubble Tea
- Plant cards with color-coded alerts
- Interactive provisioning form
- Zero calculation logic (pure view)

### Component D: M5Paper E-Ink Display
- **Path**: `m5paper_display/`
- E-ink dashboard for wall mounting
- Deep sleep wake/update/sleep cycle
- Battery powered portable display
- Read-only state subscriber

## MQTT Topics

| Topic | Direction | Purpose |
|-------|-----------|---------|
| `garden/telemetry` | Edge → Brain | Sensor readings |
| `garden/setup` | Edge ↔ Brain | Provisioning broadcasts |
| `garden/setup/{MAC}` | Brain → Edge | Device assignments |
| `garden/admin` | TUI → Brain | Provisioning commands |
| `garden/state` | Brain → All | Master state (5s ticker) |

## Quick Start

### 1. Start MQTT Broker
```bash
docker-compose up -d
```

### 2. Run Brain Daemon
```bash
cd brain
go mod download
go run main.go
```

### 3. Run TUI Client
```bash
cd tui
go mod download
go run main.go
```

### 4. Flash ESP32 Edge Node
```bash
cd esp32_edge
# Edit main.cpp with WiFi credentials and MQTT broker IP
idf.py build
idf.py -p /dev/ttyUSB0 flash monitor
```

### 5. Flash M5Paper Display
```bash
cd m5paper_display
# Edit src/main.cpp with WiFi credentials and MQTT broker IP
pio run --target upload
pio device monitor
```

## Provisioning Workflow

1. Flash ESP32 with blank NVS
2. ESP32 broadcasts MAC to `garden/setup`
3. Brain adds to unprovisioned list
4. TUI shows alert banner
5. User presses `p` and fills form:
   - MAC address
   - Plant ID (e.g., `thyme_1`)
   - Moisture thresholds (min/max %)
   - Temperature thresholds (min/max °C)
6. Brain updates `config.yaml`
7. Brain publishes assignment to `garden/setup/{MAC}`
8. ESP32 saves Plant ID to NVS and restarts
9. ESP32 enters telemetry mode

## Configuration

### config.yaml
```yaml
broker:
  address: "tcp://localhost:1883"
  client_id: "garden_brain"

plants:
  thyme_1:
    min_moisture: 20
    max_moisture: 40
    min_temp: 18.0
    max_temp: 25.0
```

## Constraints

- ✅ Local network only (air-gapped)
- ✅ MQTT strictly (no HTTP REST)
- ✅ ESP-IDF for ESP32 (no Arduino)
- ✅ Flat YAML config (no database)
- ✅ Hub-and-spoke architecture
- ✅ Central state management in Brain

## State Message Schema

```json
{
  "timestamp": 1700000000,
  "plants": {
    "thyme_1": {
      "moisture": 35.0,
      "temp": 22.5,
      "moisture_alert": false,
      "temp_alert": false
    },
    "lavender_1": {
      "moisture": 45.0,
      "temp": 21.0,
      "moisture_alert": true,
      "alert_msg": "Moisture 45.0% exceeds 40.0% max threshold"
    }
  },
  "unprovisioned_devices": ["A1B2C3"]
}
```

## Tech Stack

- **Broker**: Eclipse Mosquitto (Docker)
- **Edge**: ESP-IDF (C++/FreeRTOS)
- **Brain**: Go + Paho MQTT
- **TUI**: Go + Bubble Tea + Lipgloss
- **Display**: PlatformIO (Arduino) + M5EPD

## License

MIT
