/*
 * M5Stack Paper S3 E-Ink Display
 * Hub-and-Spoke IoT Herb Garden Monitor
 */

#include <M5EPD.h>
#include <WiFi.h>
#include <PubSubClient.h>
#include <ArduinoJson.h>

// Configuration
const char* WIFI_SSID = "YOUR_WIFI_SSID";
const char* WIFI_PASSWORD = "YOUR_WIFI_PASSWORD";
const char* MQTT_BROKER = "192.168.1.100";
const int MQTT_PORT = 1883;

// Sleep configuration (15 minutes)
#define DEEP_SLEEP_SECONDS 900
#define uS_TO_S_FACTOR 1000000ULL

// Display dimensions
#define SCREEN_WIDTH 540
#define SCREEN_HEIGHT 960

// Global objects
M5EPD_Canvas canvas(&M5.EPD);
WiFiClient wifiClient;
PubSubClient mqttClient(wifiClient);

// State tracking
bool stateReceived = false;
StaticJsonDocument<4096> stateDoc;

// Function declarations
void connectWiFi();
void connectMQTT();
void mqttCallback(char* topic, byte* payload, unsigned int length);
void drawDashboard();
void drawPlantCard(int x, int y, int w, int h, const char* plantId, JsonObject state);
void drawUnprovisionedAlert(JsonArray devices);
void enterDeepSleep();

void setup() {
    Serial.begin(115200);
    Serial.println("M5Paper Display starting...");

    // Initialize M5Paper
    M5.begin();
    M5.EPD.SetRotation(0);
    M5.EPD.Clear(true);
    M5.RTC.begin();

    // Create canvas
    canvas.createCanvas(SCREEN_WIDTH, SCREEN_HEIGHT);
    canvas.setTextSize(3);

    // Show loading screen
    canvas.fillCanvas(0);
    canvas.setTextColor(15);
    canvas.drawString("IoT Herb Garden", 20, 20);
    canvas.drawString("Connecting...", 20, 60);
    canvas.pushCanvas(0, 0, UPDATE_MODE_GC16);

    // Connect to WiFi
    connectWiFi();

    // Connect to MQTT
    mqttClient.setServer(MQTT_BROKER, MQTT_PORT);
    mqttClient.setCallback(mqttCallback);
    mqttClient.setBufferSize(4096);
    connectMQTT();

    // Subscribe to garden/state
    mqttClient.subscribe("garden/state");
    Serial.println("Subscribed to garden/state");

    // Wait for state update (timeout after 30 seconds)
    unsigned long startTime = millis();
    while (!stateReceived && (millis() - startTime < 30000)) {
        mqttClient.loop();
        delay(100);
    }

    if (stateReceived) {
        Serial.println("State received, drawing dashboard");
        drawDashboard();
    } else {
        Serial.println("Timeout waiting for state");
        canvas.fillCanvas(0);
        canvas.setTextColor(15);
        canvas.drawString("No data received", 20, 20);
        canvas.drawString("Check MQTT broker", 20, 60);
        canvas.pushCanvas(0, 0, UPDATE_MODE_GC16);
    }

    // Disconnect and sleep
    mqttClient.disconnect();
    WiFi.disconnect(true);
    WiFi.mode(WIFI_OFF);

    enterDeepSleep();
}

void loop() {
    // Never reached due to deep sleep
}

void connectWiFi() {
    Serial.print("Connecting to WiFi: ");
    Serial.println(WIFI_SSID);

    WiFi.mode(WIFI_STA);
    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);

    int attempts = 0;
    while (WiFi.status() != WL_CONNECTED && attempts < 20) {
        delay(500);
        Serial.print(".");
        attempts++;
    }

    if (WiFi.status() == WL_CONNECTED) {
        Serial.println("\nWiFi connected");
        Serial.print("IP: ");
        Serial.println(WiFi.localIP());
    } else {
        Serial.println("\nWiFi connection failed!");
    }
}

void connectMQTT() {
    Serial.print("Connecting to MQTT broker...");

    String clientId = "m5paper_" + String((uint32_t)ESP.getEfuseMac(), HEX);

    int attempts = 0;
    while (!mqttClient.connected() && attempts < 10) {
        if (mqttClient.connect(clientId.c_str())) {
            Serial.println(" connected!");
        } else {
            Serial.print(".");
            delay(1000);
            attempts++;
        }
    }

    if (!mqttClient.connected()) {
        Serial.println("\nMQTT connection failed!");
    }
}

void mqttCallback(char* topic, byte* payload, unsigned int length) {
    Serial.print("Message received on topic: ");
    Serial.println(topic);

    // Parse JSON
    DeserializationError error = deserializeJson(stateDoc, payload, length);

    if (error) {
        Serial.print("JSON parsing failed: ");
        Serial.println(error.c_str());
        return;
    }

    stateReceived = true;
    Serial.println("State parsed successfully");
}

void drawDashboard() {
    canvas.fillCanvas(0);
    canvas.setTextColor(15);

    // Header
    canvas.setTextSize(4);
    canvas.drawString("Herb Garden Monitor", 20, 20);

    // Draw timestamp
    long timestamp = stateDoc["timestamp"];
    canvas.setTextSize(2);
    char timeStr[64];
    snprintf(timeStr, sizeof(timeStr), "Updated: %ld", timestamp);
    canvas.drawString(timeStr, 20, 70);

    // Check for unprovisioned devices
    JsonArray unprovisionedDevices = stateDoc["unprovisioned_devices"];
    if (unprovisionedDevices.size() > 0) {
        drawUnprovisionedAlert(unprovisionedDevices);
    }

    // Draw plant cards
    JsonObject plants = stateDoc["plants"];
    int plantCount = plants.size();

    if (plantCount == 0) {
        canvas.setTextSize(3);
        canvas.drawString("No plants configured", 20, 200);
    } else {
        int cardWidth = 250;
        int cardHeight = 180;
        int padding = 20;
        int startY = 140;

        int row = 0;
        int col = 0;

        for (JsonPair kv : plants) {
            const char* plantId = kv.key().c_str();
            JsonObject plantState = kv.value();

            int x = padding + col * (cardWidth + padding);
            int y = startY + row * (cardHeight + padding);

            drawPlantCard(x, y, cardWidth, cardHeight, plantId, plantState);

            col++;
            if (col >= 2) {
                col = 0;
                row++;
            }
        }
    }

    // Push to screen
    canvas.pushCanvas(0, 0, UPDATE_MODE_GC16);
    Serial.println("Dashboard drawn");
}

void drawPlantCard(int x, int y, int w, int h, const char* plantId, JsonObject state) {
    // Draw border
    canvas.drawRect(x, y, w, h, 15);

    // Plant name
    canvas.setTextSize(3);
    canvas.setTextColor(15);
    canvas.drawString(plantId, x + 10, y + 10);

    // Moisture
    float moisture = state["moisture"];
    bool moistureAlert = state["moisture_alert"];

    canvas.setTextSize(2);
    char moistureStr[32];
    snprintf(moistureStr, sizeof(moistureStr), "Moisture: %.1f%%", moisture);

    if (moistureAlert) {
        // Draw alert indicator (filled rect)
        canvas.fillRect(x + 5, y + 50, 5, 20, 15);
    }
    canvas.drawString(moistureStr, x + 15, y + 50);

    // Temperature
    float temp = state["temp"];
    bool tempAlert = state["temp_alert"];

    char tempStr[32];
    snprintf(tempStr, sizeof(tempStr), "Temp: %.1f C", temp);

    if (tempAlert) {
        // Draw alert indicator
        canvas.fillRect(x + 5, y + 80, 5, 20, 15);
    }
    canvas.drawString(tempStr, x + 15, y + 80);

    // Alert message
    if (state.containsKey("alert_msg")) {
        const char* alertMsg = state["alert_msg"];
        canvas.setTextSize(1);
        canvas.drawString("Alert:", x + 10, y + 115);

        // Word wrap alert message
        String msg = String(alertMsg);
        int maxWidth = w - 20;
        int lineHeight = 18;
        int currentY = y + 135;

        // Simple word wrap (truncate if too long)
        if (msg.length() > 40) {
            msg = msg.substring(0, 37) + "...";
        }
        canvas.drawString(msg.c_str(), x + 10, currentY);
    }
}

void drawUnprovisionedAlert(JsonArray devices) {
    // Draw alert banner
    int bannerY = 100;
    int bannerHeight = 30;

    canvas.fillRect(0, bannerY, SCREEN_WIDTH, bannerHeight, 10);
    canvas.setTextSize(2);
    canvas.setTextColor(0);

    char alertText[128];
    snprintf(alertText, sizeof(alertText), "! %d Unprovisioned Device(s)",
             devices.size());
    canvas.drawString(alertText, 20, bannerY + 5);

    canvas.setTextColor(15);
}

void enterDeepSleep() {
    Serial.println("Entering deep sleep for 15 minutes...");

    // Configure wake up source
    esp_sleep_enable_timer_wakeup(DEEP_SLEEP_SECONDS * uS_TO_S_FACTOR);

    // Enter deep sleep
    M5.shutdown();
}
