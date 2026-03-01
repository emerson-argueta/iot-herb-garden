# M5Stack Paper S3 E-Ink Display

## Overview
PlatformIO firmware for M5Stack Paper S3 e-ink display in the Hub-and-Spoke IoT Herb Garden Monitor.

## Hardware
- M5Stack Paper (ESP32-based e-ink display)
- 4.7" e-ink touchscreen (540x960)
- Built-in battery for portable operation

## Features
- **Read-Only Display**: Subscribes only to `garden/state`
- **Dashboard View**: Grid layout showing all plants with moisture/temperature
- **Alert Indicators**: Visual alerts for plants exceeding thresholds
- **Unprovisioned Device Banner**: Shows when devices need provisioning
- **Deep Sleep**: Wakes every 15 minutes, updates display, returns to sleep

## Configuration
Edit `src/main.cpp` and update:
```cpp
const char* WIFI_SSID = "YOUR_WIFI_SSID";
const char* WIFI_PASSWORD = "YOUR_WIFI_PASSWORD";
const char* MQTT_BROKER = "192.168.1.100";
```

## Build and Upload
```bash
# Install PlatformIO
pip install platformio

# Build
pio run

# Upload to M5Paper
pio run --target upload

# Monitor serial output
pio device monitor
```

## Operation Flow
1. Wake from deep sleep (or power on)
2. Initialize M5Paper e-ink display
3. Connect to WiFi
4. Connect to MQTT broker
5. Subscribe to `garden/state`
6. Wait for state message (30s timeout)
7. Parse JSON and render dashboard
8. Disconnect WiFi/MQTT
9. Enter deep sleep for 15 minutes
10. Repeat

## Display Layout
```
┌─────────────────────────────────┐
│ Herb Garden Monitor             │
│ Updated: timestamp              │
├─────────────────────────────────┤
│ ⚠ N Unprovisioned Device(s)     │ (if any)
├─────────────────────────────────┤
│ ┌──────────┐  ┌──────────┐     │
│ │ thyme_1  │  │lavender_1│     │
│ │ Moist:35%│  │ Moist:45%│     │
│ │ Temp:22°C│  │▮Temp:21°C│     │
│ │          │  │ Alert:...│     │
│ └──────────┘  └──────────┘     │
└─────────────────────────────────┘
```

## Power Optimization
- Deep sleep between updates conserves battery
- E-ink display retains image when powered off
- Typical battery life: several days with 15-min intervals

## Troubleshooting
- **Display not updating**: Check WiFi/MQTT connectivity
- **Garbled display**: Adjust `UPDATE_MODE` (GC16, GL16, A2)
- **Battery drain**: Increase `DEEP_SLEEP_SECONDS`
