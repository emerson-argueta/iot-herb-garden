# ESP32 Edge Node Firmware

## Overview
ESP-IDF firmware for ESP32 edge nodes in the Hub-and-Spoke IoT Herb Garden Monitor.

## Prerequisites
- ESP-IDF v5.0 or later
- ESP32 development board
- Moisture sensor connected to GPIO36 (ADC1_CHANNEL_0)

## Configuration
Edit `main/main.cpp` and update:
```cpp
#define WIFI_SSID      "YOUR_WIFI_SSID"
#define WIFI_PASSWORD  "YOUR_WIFI_PASSWORD"
#define MQTT_BROKER    "mqtt://192.168.1.100:1883"
```

## Build and Flash
```bash
# Set up ESP-IDF environment
. $HOME/esp/esp-idf/export.sh

# Configure project (optional)
idf.py menuconfig

# Build
idf.py build

# Flash to ESP32
idf.py -p /dev/ttyUSB0 flash

# Monitor serial output
idf.py -p /dev/ttyUSB0 monitor
```

## Boot Logic
1. **First Boot (Unprovisioned)**:
   - Checks NVS for `PLANT_ID`
   - If none found: Broadcasts MAC to `garden/setup`
   - Subscribes to `garden/setup/{MAC}`
   - Waits for `assign_id` message
   - Saves to NVS and restarts

2. **Subsequent Boots (Provisioned)**:
   - Loads `PLANT_ID` from NVS
   - Enters telemetry mode
   - Wakes every 15 minutes
   - Reads moisture and temperature sensors
   - Publishes to `garden/telemetry`

## Sensor Connections
- **Moisture Sensor**: Analog output to GPIO36 (ADC1_CH0)
- **Temperature**: Uses ESP32 internal sensor

## MQTT Topics
- **Publish**: `garden/telemetry` - Sensor readings
- **Publish**: `garden/setup` - Provisioning broadcast
- **Subscribe**: `garden/setup/{MAC}` - Provisioning assignment
