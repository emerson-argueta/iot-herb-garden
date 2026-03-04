/*
 * herb-display — M5Stack Paper S3 e-ink display node
 *
 * Subscribes to garden/state (retained, published every 5 s by the brain)
 * and renders a plant-status dashboard on the 960×540 e-ink screen.
 *
 * Display strategy
 *   Full refresh  : on boot and whenever the plant list changes (rare)
 *   Partial update: whenever sensor values change between ticks (common)
 *   No refresh    : when nothing has changed (preserves display lifetime)
 *
 * Configuration: edit the #defines below before flashing.
 */

#include <Arduino.h>
#include <M5Unified.h>
#include <WiFi.h>
#include <PubSubClient.h>
#include <ArduinoJson.h>
#include <map>
#include <vector>
#include <string>
#include <algorithm>
#include <ctime>

// ── User configuration ────────────────────────────────────────────────────────

#define WIFI_SSID       "your_ssid"
#define WIFI_PASSWORD   "your_password"
#define MQTT_BROKER     "192.168.1.100"
#define MQTT_PORT       1883
#define MQTT_CLIENT_ID  "herb-display"
#define TOPIC_STATE     "garden/state"

// ── Display dimensions (M5Stack Paper S3) ────────────────────────────────────

static constexpr int SCREEN_W = 960;
static constexpr int SCREEN_H = 540;

// ── JSON payload shapes (mirror brain's garden/state) ─────────────────────────

struct PlantState {
    String display_name;
    float  moisture      = 0;
    float  temp          = 0;
    bool   watering      = false;
    bool   in_cooldown   = false;
    bool   moisture_alert = false;
    bool   temp_alert    = false;
    String alert_msg;
    int64_t last_seen    = 0;   // unix timestamp
};

struct StatePayload {
    int64_t                   timestamp = 0;
    std::map<String, PlantState> plants;
    std::vector<String>       unprovisioned;
};

// ── Global state ──────────────────────────────────────────────────────────────

static WiFiClient   s_wifi_client;
static PubSubClient s_mqtt(s_wifi_client);
static StatePayload s_current;
static StatePayload s_last_drawn;
static bool         s_needs_draw   = false;
static bool         s_full_refresh = true;   // force full refresh on boot

// ── Connectivity helpers ──────────────────────────────────────────────────────

static void wifi_connect()
{
    Serial.printf("[wifi] connecting to %s\n", WIFI_SSID);
    WiFi.mode(WIFI_STA);
    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
    while (WiFi.status() != WL_CONNECTED) {
        delay(500);
        Serial.print(".");
    }
    Serial.printf("\n[wifi] connected, IP: %s\n",
                  WiFi.localIP().toString().c_str());
}

static void mqtt_subscribe()
{
    s_mqtt.subscribe(TOPIC_STATE, 1);
    Serial.printf("[mqtt] subscribed to %s\n", TOPIC_STATE);
}

static void mqtt_reconnect()
{
    while (!s_mqtt.connected()) {
        Serial.println("[mqtt] connecting…");
        if (s_mqtt.connect(MQTT_CLIENT_ID)) {
            Serial.println("[mqtt] connected");
            mqtt_subscribe();
        } else {
            Serial.printf("[mqtt] failed (rc=%d), retry in 5 s\n",
                          s_mqtt.state());
            delay(5000);
        }
    }
}

// ── JSON parsing ──────────────────────────────────────────────────────────────

static StatePayload parse_state(const uint8_t *payload, unsigned int len)
{
    StatePayload out;
    JsonDocument doc;
    if (deserializeJson(doc, payload, len) != DeserializationError::Ok)
        return out;

    out.timestamp = doc["timestamp"].as<int64_t>();

    JsonObject plants = doc["plants"].as<JsonObject>();
    for (JsonPair kv : plants) {
        PlantState ps;
        ps.display_name   = kv.value()["display_name"]   | "";
        ps.moisture       = kv.value()["moisture"]       | 0.0f;
        ps.temp           = kv.value()["temp"]           | 0.0f;
        ps.watering       = kv.value()["watering"]       | false;
        ps.in_cooldown    = kv.value()["in_cooldown"]    | false;
        ps.moisture_alert = kv.value()["moisture_alert"] | false;
        ps.temp_alert     = kv.value()["temp_alert"]     | false;
        ps.alert_msg      = kv.value()["alert_msg"]      | "";
        ps.last_seen      = kv.value()["last_seen"]      | (int64_t)0;
        out.plants[String(kv.key().c_str())] = ps;
    }

    JsonArray unprov = doc["unprovisioned_devices"].as<JsonArray>();
    for (JsonVariant v : unprov) {
        out.unprovisioned.push_back(v.as<String>());
    }
    return out;
}

// ── Plant list changed? ───────────────────────────────────────────────────────

static bool plant_list_changed(const StatePayload &a, const StatePayload &b)
{
    if (a.plants.size() != b.plants.size()) return true;
    for (auto &kv : a.plants) {
        if (b.plants.find(kv.first) == b.plants.end()) return true;
    }
    return false;
}

// ── MQTT callback ─────────────────────────────────────────────────────────────

static void on_message(const char *topic, uint8_t *payload, unsigned int len)
{
    if (strcmp(topic, TOPIC_STATE) != 0) return;

    StatePayload incoming = parse_state(payload, len);
    if (plant_list_changed(s_current, incoming)) {
        s_full_refresh = true;
    }
    s_current   = incoming;
    s_needs_draw = true;
}

// ── Rendering ─────────────────────────────────────────────────────────────────

static String ago_str(int64_t unix_ts)
{
    if (unix_ts == 0) return "never";
    int64_t now = (int64_t)time(nullptr);
    int64_t d   = now - unix_ts;
    if (d < 0)   d = 0;
    if (d < 60)  return String(d) + "s ago";
    if (d < 3600) return String(d / 60) + "m ago";
    return "offline";
}

/*
 * Draw a filled rectangle moisture bar.
 * x, y = top-left; w = total width; h = height; pct = 0..100
 */
static void draw_moisture_bar(M5Canvas &cv, int x, int y, int w, int h,
                              float pct, bool alert)
{
    cv.drawRect(x, y, w, h, TFT_BLACK);
    int filled = (int)(pct / 100.0f * (float)(w - 2));
    if (filled > 0) {
        uint16_t col = alert ? TFT_BLACK : TFT_DARKGRAY;
        cv.fillRect(x + 1, y + 1, filled, h - 2, col);
    }
}

static void render(bool full)
{
    // M5Unified canvas for flicker-free drawing.
    M5Canvas cv(&M5.Display);
    cv.setColorDepth(1);   // 1-bit for e-ink
    cv.createSprite(SCREEN_W, SCREEN_H);
    cv.fillSprite(TFT_WHITE);

    // ── Header ────────────────────────────────────────────────────────────────
    cv.setTextSize(2);
    cv.setTextColor(TFT_BLACK);
    cv.drawString("Herb Garden", 20, 14);

    // Timestamp
    char ts_buf[32];
    struct tm tm_info;
    time_t now = time(nullptr);
    localtime_r(&now, &tm_info);
    strftime(ts_buf, sizeof(ts_buf), "%H:%M:%S", &tm_info);
    cv.setTextSize(1);
    cv.drawString(ts_buf, SCREEN_W - 90, 20);

    cv.drawLine(0, 44, SCREEN_W, 44, TFT_BLACK);

    // ── Plant cards ───────────────────────────────────────────────────────────
    // Collect sorted plant IDs.
    std::vector<String> ids;
    for (auto &kv : s_current.plants) ids.push_back(kv.first);
    std::sort(ids.begin(), ids.end());

    constexpr int CARD_H  = 110;
    constexpr int CARD_W  = SCREEN_W - 40;
    constexpr int CARD_X  = 20;
    constexpr int CARD_Y0 = 54;
    constexpr int CARD_PAD = 8;
    constexpr int BAR_W   = 200;
    constexpr int BAR_H   = 16;

    if (ids.empty()) {
        cv.setTextSize(1);
        cv.drawString("Waiting for data from brain...", CARD_X, CARD_Y0 + 20);
    }

    for (int i = 0; i < (int)ids.size(); i++) {
        const String    &id = ids[i];
        const PlantState &ps = s_current.plants.at(id);
        int y = CARD_Y0 + i * (CARD_H + 6);

        // Card border
        cv.drawRect(CARD_X, y, CARD_W, CARD_H, TFT_BLACK);

        // Plant name: "Display Name (plant_id)" or just "plant_id"
        cv.setTextSize(2);
        String heading = ps.display_name.length() > 0
            ? ps.display_name + " (" + id + ")"
            : id;
        cv.drawString(heading, CARD_X + CARD_PAD, y + CARD_PAD);

        // Last-seen
        cv.setTextSize(1);
        String ago = ago_str(ps.last_seen);
        cv.drawString(ago, CARD_W - 80, y + CARD_PAD + 4);

        // Moisture label + bar + value
        int row1 = y + CARD_PAD + 28;
        cv.drawString("Moisture:", CARD_X + CARD_PAD, row1);
        draw_moisture_bar(cv, CARD_X + 90, row1, BAR_W, BAR_H,
                          ps.moisture, ps.moisture_alert);
        char mbuf[16];
        snprintf(mbuf, sizeof(mbuf), "%.1f%%", ps.moisture);
        cv.drawString(mbuf, CARD_X + 90 + BAR_W + 6, row1);
        if (ps.moisture_alert) {
            cv.drawString("!", CARD_X + 90 + BAR_W + 50, row1);
        }

        // Temperature
        int row2 = row1 + 22;
        char tbuf[24];
        snprintf(tbuf, sizeof(tbuf), "Temp: %.1f C", ps.temp);
        cv.drawString(tbuf, CARD_X + CARD_PAD, row2);
        if (ps.temp_alert) cv.drawString("!", CARD_X + 120, row2);

        // Pump status
        int row3 = row2 + 22;
        if (ps.watering) {
            String pump_str = ps.in_cooldown ? "Pump ON (cooldown)" : "Pump ON";
            cv.drawString(pump_str, CARD_X + CARD_PAD, row3);
        } else {
            cv.drawString("Pump OFF", CARD_X + CARD_PAD, row3);
        }

        // Alert message (if any)
        if (ps.alert_msg.length() > 0) {
            cv.drawString(ps.alert_msg, CARD_X + CARD_W / 2, row3);
        }
    }

    // ── Unprovisioned devices ─────────────────────────────────────────────────
    if (!s_current.unprovisioned.empty()) {
        int uy = CARD_Y0 + (int)ids.size() * (CARD_H + 6);
        cv.setTextSize(1);
        cv.drawString("Unprovisioned:", CARD_X, uy);
        uy += 16;
        for (auto &mac : s_current.unprovisioned) {
            cv.drawString(mac, CARD_X + 10, uy);
            uy += 14;
        }
    }

    // Push sprite to display.
    // Full refresh (epd_quality) clears to white then redraws — use on boot or
    // when layout changes.  Fast/partial (epd_fast) updates only changed pixels
    // with no white flash — use for routine value updates.
    M5.Display.setEpdMode(full ? epd_mode_t::epd_quality : epd_mode_t::epd_fast);
    M5.Display.waitDisplay();
    cv.pushSprite(0, 0);

    cv.deleteSprite();
}

// ── Arduino entry points ──────────────────────────────────────────────────────

void setup()
{
    auto cfg = M5.config();
    M5.begin(cfg);

    Serial.begin(115200);
    Serial.println("[herb-display] booting");

    // E-ink orientation: landscape.  EPD mode is set per-render, not globally.
    M5.Display.setRotation(1);

    // Splash while connecting
    M5.Display.setTextSize(2);
    M5.Display.drawString("Herb Garden", 20, 20);
    M5.Display.setTextSize(1);
    M5.Display.drawString("Connecting...", 20, 60);

    wifi_connect();

    // Sync clock via SNTP so timestamps make sense.
    configTime(0, 0, "pool.ntp.org");

    s_mqtt.setServer(MQTT_BROKER, MQTT_PORT);
    s_mqtt.setCallback(on_message);
    s_mqtt.setBufferSize(4096);   // garden/state can be large with many plants
    mqtt_reconnect();

    Serial.println("[herb-display] ready");
}

void loop()
{
    if (WiFi.status() != WL_CONNECTED) wifi_connect();
    if (!s_mqtt.connected()) mqtt_reconnect();
    s_mqtt.loop();

    if (s_needs_draw) {
        s_needs_draw = false;
        render(s_full_refresh);
        s_full_refresh = false;
        s_last_drawn   = s_current;
    }
}
