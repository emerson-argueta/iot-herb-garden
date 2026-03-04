# IoT Herb Garden Monitor

Local-only, MQTT-based herb garden monitor. No cloud, no REST API.

```
ESP32 nodes ──→ Mosquitto broker ──→ brain (Go daemon)
                                          │
                                    ┌─────┴──────┐
                               TUI (Go)    M5Paper display
```

## Components

| Directory | Language | Role |
|-----------|----------|------|
| `brain/` | Go | Headless daemon — sole state manager, controller, alert emailer |
| `tui/` | Go (bubbletea) | Terminal dashboard + provisioning UI |
| `esp32_edge/` | C++ (ESP-IDF) | Sensor node: moisture, temperature, pump relay |
| `m5paper_display/` | C++ (Arduino/PlatformIO) | E-ink status display |
| `mosquitto/` | — | Eclipse Mosquitto broker config |

## MQTT topics

| Topic | Direction | Purpose |
|-------|-----------|---------|
| `garden/telemetry` | Edge → Brain | Sensor readings |
| `garden/setup` | Edge → Brain | Provisioning beacon |
| `garden/setup/{MAC}` | Brain → Edge | Assign plant ID |
| `garden/admin` | TUI → Brain | Provision new plant |
| `garden/state` | Brain → All | Master state (retained, every 5 s) |
| `garden/command/{id}` | Brain → Edge | `water_on` / `water_off` |

## Prerequisites

- Docker (for Mosquitto)
- Go ≥ 1.22 (brain + TUI)
- ESP-IDF ≥ 5.1 (ESP32 firmware)
- PlatformIO (M5Paper firmware)

## Running

### 1. Start the broker

```bash
docker compose up -d
```

### 2. Start the brain

```bash
cd brain
go run . -config config.yaml
```

Edit `brain/config.yaml` to set the broker address, plant thresholds, and
optional email notifications. Plants can also be added at runtime via the TUI.

### 3. Start the TUI

```bash
cd tui
go run . --broker tcp://localhost:1883 --id herb-tui
```

| Flag | Default | Description |
|------|---------|-------------|
| `--broker` | `tcp://localhost:1883` | MQTT broker address |
| `--id` | `herb-tui` | MQTT client ID (change if running multiple TUI instances) |
| `--debug` | off | Enable verbose MQTT logging to stdout/stderr |

### 4. Flash an ESP32 node

```bash
cd esp32_edge
idf.py menuconfig          # set WiFi SSID/password, broker IP, pin assignments
idf.py build flash monitor
```

On first boot the node beacons every 10 s to `garden/setup`. Use the TUI to
assign it a plant ID and thresholds. The assignment is persisted to NVS so it
survives reboots.

### 5. Flash the M5Stack Paper S3

Edit the `#define` lines at the top of `m5paper_display/src/main.cpp`:

```cpp
#define WIFI_SSID      "your_ssid"
#define WIFI_PASSWORD  "your_password"
#define MQTT_BROKER    "192.168.1.100"
```

Then build and upload with PlatformIO:

```bash
cd m5paper_display
pio run --target upload
```

## Using the TUI

### Main view

```
Herb Garden Monitor  connected  updated 14:32:01
────────────────────────────────────────────────────────────────────────

PLANTS

  Basil (herb-basil)                              32s ago
  Moisture  47.3% [███████░░░░░░░]  Temp 21.4°C  Pump OFF

  Mint (herb-mint)  ⚠ MOISTURE                   1m ago
  Moisture  12.1% [██░░░░░░░░░░░░]  Temp 22.0°C  Pump ON (cooldown)

UNPROVISIONED DEVICES

  ▶ AA:BB:CC:DD:EE:FF

────────────────────────────────────────────────────────────────────────
[q] quit  [↑↓/jk] select  [p] provision
```

The header shows broker connection status and the time of the last state
update received from the brain. Each plant card shows:

| Field | Description |
|-------|-------------|
| Name (id) | Display name and plant ID |
| `⚠ MOISTURE` / `⚠ TEMP` | Active alert badge |
| Last seen | Time since last telemetry (`Xs ago`, `Xm ago`, or `offline`) |
| Moisture % + bar | Current soil moisture with 14-cell visual bar |
| Temp | Temperature in °C |
| Pump | `Pump OFF`, `Pump ON`, or `Pump ON (cooldown)` |

### Main view keys

| Key | Action |
|-----|--------|
| `q` / `Ctrl+C` | Quit |
| `↑` / `k` | Move cursor up in unprovisioned device list |
| `↓` / `j` | Move cursor down in unprovisioned device list |
| `p` | Open provisioning form for the selected device |

The unprovisioned section and navigation keys only appear when there are
devices that have beaconed but not yet been assigned a plant ID.

### Provisioning a new device

Press `p` with an unprovisioned device selected to open the provisioning
form. The MAC address of the device is shown at the top and is filled in
automatically.

| Field | Default | Notes |
|-------|---------|-------|
| Plant ID | — | Required. Letters, digits, `-` and `_` only. Used as the MQTT topic segment and config key. |
| Display Name | — | Required. Human-readable label shown in TUI and on the e-ink display. |
| Min Moisture % | `20` | Brain waters the plant when moisture drops below this value. |
| Max Moisture % | `60` | Brain stops watering when moisture rises above this value. |
| Min Temp °C | `15` | Temperature alert threshold (lower bound). |
| Max Temp °C | `30` | Temperature alert threshold (upper bound). |
| Cooldown (sec) | `1200` | Minimum time (20 min) between watering cycles to allow soil absorption. |

**Provisioning form keys**

| Key | Action |
|-----|--------|
| `Tab` / `↓` | Move to next field |
| `Shift+Tab` / `↑` | Move to previous field |
| `Enter` | Advance to next field; submit on the last field |
| `Esc` | Cancel and return to main view |
| `Ctrl+C` | Quit |

On submit the TUI publishes to `garden/admin`. The brain stores the new
plant in `brain/config.yaml` and the ESP32 node receives its plant ID via
`garden/setup/{MAC}`, persisting it to NVS.

### Testing without hardware

You can exercise the full TUI + brain flow on a single machine with no
ESP32 connected. Start the broker and brain, then follow these steps:

**Step 1 — simulate an unprovisioned device beacon** (this is what an ESP32
sends before it has been assigned a plant ID):

```bash
mosquitto_pub -h localhost -t garden/setup \
  -m '{"mac":"AA:BB:CC:DD:EE:FF","status":"awaiting_provision"}'
```

The brain adds the MAC to its unprovisioned list and the TUI will show it
under **UNPROVISIONED DEVICES** within the next state tick (≤ 5 s). Use
`[↓/j]` to select it and `[p]` to open the provisioning form.

**Step 2 — after provisioning, simulate telemetry** so the plant card
appears in the **PLANTS** section:

```bash
mosquitto_pub -h localhost -t garden/telemetry \
  -m '{"plant_id":"herb-test","moisture":35.0,"temp":22.5}'
```

Replace `herb-test` with the plant ID you entered in the provisioning form.

## Calibrating the moisture sensor

The ESP32 firmware uses raw ADC counts to compute moisture percentage. The
defaults (`DRY=3100`, `WET=1300`) suit a typical capacitive sensor at 3.3 V with
12-bit resolution, but you should calibrate for your specific sensor:

1. Run `idf.py monitor` and read the raw ADC values printed at boot.
2. Record the value when the sensor is in dry air (`DRY`).
3. Record the value when the sensor is submerged in water (`WET`).
4. Update the constants in `esp32_edge/main/main.cpp` and reflash.

## Email alerts

Set `notifications.enabled: true` in `brain/config.yaml` and fill in your
SMTP credentials. The brain sends de-duplicated alerts for:

- Moisture or temperature out of range (CRITICAL / RESOLVED)
- Pump activated (INFO, one-shot)
- Edge node offline — no telemetry for `watchdog_minutes` (CRITICAL / RESOLVED)

## Hardware wiring (defaults, configurable via `idf.py menuconfig`)

| Signal | GPIO |
|--------|------|
| Capacitive moisture sensor (ADC1 ch 6) | GPIO 34 |
| DS18B20 one-wire data (4.7 kΩ pull-up) | GPIO 4 |
| Pump relay (active HIGH) | GPIO 26 |
