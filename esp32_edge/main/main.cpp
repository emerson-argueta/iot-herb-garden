/*
 * herb-edge — ESP32 edge node firmware
 *
 * Hardware
 *   Moisture  : capacitive soil sensor → ADC1 (GPIO34 = ch6 by default)
 *   Temp      : DS18B20 one-wire       → GPIO4  (4.7 kΩ pull-up to 3.3 V)
 *   Pump      : relay module           → GPIO26 (active HIGH)
 *
 * MQTT flow (hub-and-spoke; brain is sole state manager)
 *   Publish  garden/setup          {"mac":"…","status":"awaiting_provision"}
 *   Subscribe garden/setup/{MAC}   {"assign_id":"plant_id"}   → provision
 *   Publish  garden/telemetry      {"plant_id":"…","moisture":…,"temp":…}
 *   Subscribe garden/command/{id}  {"action":"water_on"|"water_off"}
 *
 * Provisioning
 *   plant_id is persisted in NVS.  On first boot (empty NVS) the node enters
 *   provisioning mode, beaconing to garden/setup every 10 s until the brain
 *   (or TUI) replies with an assign_id.  The node resets on next boot if NVS
 *   is erased (idf.py erase-flash), returning to provisioning mode.
 */

#include <cstring>
#include <cstdio>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/event_groups.h"

#include "esp_system.h"
#include "esp_wifi.h"
#include "esp_netif.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "nvs_flash.h"
#include "nvs.h"
#include "mqtt_client.h"
#include "esp_adc/adc_oneshot.h"
#include "driver/gpio.h"
#include "cJSON.h"

#include "onewire_bus.h"
#include "ds18b20.h"

static const char *TAG = "herb-edge";

// ── Pin / channel configuration (from Kconfig) ────────────────────────────────

#define PUMP_GPIO         ((gpio_num_t)CONFIG_PUMP_GPIO)
#define MOISTURE_ADC_CH   ((adc_channel_t)CONFIG_MOISTURE_ADC_CHANNEL)
#define ONEWIRE_BUS_GPIO  CONFIG_ONEWIRE_GPIO

// ── Timing ────────────────────────────────────────────────────────────────────

#define TELEMETRY_MS          (30 * 1000)   // publish sensor data every 30 s
#define SETUP_BEACON_MS       (10 * 1000)   // re-announce until provisioned
#define PUMP_MAX_ON_US        ((uint64_t)CONFIG_PUMP_MAX_ON_SECONDS * 1000000ULL)

// ── NVS keys ──────────────────────────────────────────────────────────────────

#define NVS_NAMESPACE   "herb"
#define NVS_KEY_PLANT   "plant_id"

// ── Event bits ────────────────────────────────────────────────────────────────

#define EV_WIFI_UP    BIT0
#define EV_MQTT_UP    BIT1
#define EV_PROVISION  BIT2   // plant_id is known and stored

static EventGroupHandle_t s_ev;

// ── Shared state (written once then read-only) ────────────────────────────────

static char s_plant_id[64] = {};    // empty = not yet provisioned
static char s_mac_str[13]  = {};    // "AABBCCDDEEFF" (no colons)
static esp_mqtt_client_handle_t s_mqtt = nullptr;

// ── Actuator + safety timer ───────────────────────────────────────────────────

static esp_timer_handle_t s_pump_timer = nullptr;

// Called by esp_timer when the safety cut-off fires.
static void pump_cutoff_cb(void *)
{
    gpio_set_level(PUMP_GPIO, 0);
    ESP_LOGW(TAG, "pump safety cut-off fired after %d s — relay forced OFF",
             CONFIG_PUMP_MAX_ON_SECONDS);
}

static void pump_timer_init(void)
{
    const esp_timer_create_args_t args = {
        .callback        = pump_cutoff_cb,
        .arg             = nullptr,
        .dispatch_method = ESP_TIMER_TASK,
        .name            = "pump_cutoff",
        .skip_unhandled_events = true,
    };
    ESP_ERROR_CHECK(esp_timer_create(&args, &s_pump_timer));
}

static void pump_set(bool on)
{
    gpio_set_level(PUMP_GPIO, on ? 1 : 0);
    ESP_LOGI(TAG, "pump → %s", on ? "ON" : "OFF");

    if (on) {
        // (Re-)arm the one-shot safety timer every time the pump turns on.
        esp_timer_stop(s_pump_timer);   // no-op if not running
        esp_timer_start_once(s_pump_timer, PUMP_MAX_ON_US);
        ESP_LOGI(TAG, "pump safety timer armed (%d s)", CONFIG_PUMP_MAX_ON_SECONDS);
    } else {
        // Disarm if the brain sent an explicit water_off before the cut-off.
        esp_timer_stop(s_pump_timer);
    }
}

// ── NVS helpers ───────────────────────────────────────────────────────────────

static void nvs_load_plant_id(void)
{
    nvs_handle_t h;
    if (nvs_open(NVS_NAMESPACE, NVS_READONLY, &h) != ESP_OK) return;
    size_t len = sizeof(s_plant_id);
    if (nvs_get_str(h, NVS_KEY_PLANT, s_plant_id, &len) == ESP_OK && s_plant_id[0]) {
        ESP_LOGI(TAG, "plant_id from NVS: %s", s_plant_id);
        xEventGroupSetBits(s_ev, EV_PROVISION);
    }
    nvs_close(h);
}

static void nvs_save_plant_id(const char *id)
{
    nvs_handle_t h;
    if (nvs_open(NVS_NAMESPACE, NVS_READWRITE, &h) != ESP_OK) return;
    nvs_set_str(h, NVS_KEY_PLANT, id);
    nvs_commit(h);
    nvs_close(h);
    ESP_LOGI(TAG, "plant_id saved to NVS: %s", id);
}

// ── Sensor initialisation ─────────────────────────────────────────────────────

static adc_oneshot_unit_handle_t s_adc  = nullptr;
static ds18b20_device_handle_t   s_ds   = nullptr;

static void sensors_init(void)
{
    // ── ADC for capacitive soil moisture ──────────────────────────────────────
    const adc_oneshot_unit_init_cfg_t adc_init = { .unit_id = ADC_UNIT_1 };
    ESP_ERROR_CHECK(adc_oneshot_new_unit(&adc_init, &s_adc));

    const adc_oneshot_chan_cfg_t chan_cfg = {
        .atten    = ADC_ATTEN_DB_12,
        .bitwidth = ADC_BITWIDTH_DEFAULT,
    };
    ESP_ERROR_CHECK(adc_oneshot_config_channel(s_adc, MOISTURE_ADC_CH, &chan_cfg));

    // ── DS18B20 on one-wire bus ───────────────────────────────────────────────
    const onewire_bus_config_t     bus_cfg = { .bus_gpio_num = ONEWIRE_BUS_GPIO };
    const onewire_bus_rmt_config_t rmt_cfg = { .max_rx_bytes = 10 };
    onewire_bus_handle_t ow_bus;
    ESP_ERROR_CHECK(onewire_new_bus_rmt(&bus_cfg, &rmt_cfg, &ow_bus));

    onewire_device_iter_handle_t it;
    ESP_ERROR_CHECK(onewire_new_device_iter(ow_bus, &it));
    onewire_device_t dev;
    while (onewire_device_iter_get_next(it, &dev) == ESP_OK) {
        const ds18b20_config_t ds_cfg = {};
        if (ds18b20_new_device(&dev, &ds_cfg, &s_ds) == ESP_OK) {
            ESP_LOGI(TAG, "DS18B20 found on GPIO%d", ONEWIRE_BUS_GPIO);
            break;
        }
    }
    onewire_del_device_iter(it);
    if (!s_ds) ESP_LOGW(TAG, "no DS18B20 found — temperature will read 0.0");

    // ── Pump relay GPIO ───────────────────────────────────────────────────────
    const gpio_config_t pump_io = {
        .pin_bit_mask  = 1ULL << CONFIG_PUMP_GPIO,
        .mode          = GPIO_MODE_OUTPUT,
        .pull_up_en    = GPIO_PULLUP_DISABLE,
        .pull_down_en  = GPIO_PULLDOWN_DISABLE,
        .intr_type     = GPIO_INTR_DISABLE,
    };
    ESP_ERROR_CHECK(gpio_config(&pump_io));
    pump_timer_init();
    pump_set(false);
}

// ── Sensor reading ────────────────────────────────────────────────────────────

/*
 * Moisture percentage from a capacitive sensor.
 * Calibrate DRY / WET to your specific sensor and supply voltage.
 * Typical 3.3 V 12-bit readings: ~3100 (air) → ~1300 (submerged).
 */
static float read_moisture(void)
{
    constexpr int DRY = 3100, WET = 1300;
    int raw = 0;
    adc_oneshot_read(s_adc, MOISTURE_ADC_CH, &raw);
    float pct = 100.0f * (float)(DRY - raw) / (float)(DRY - WET);
    if (pct < 0.0f)   pct = 0.0f;
    if (pct > 100.0f) pct = 100.0f;
    return pct;
}

/*
 * DS18B20 temperature read.  Triggers a 12-bit conversion (≤750 ms) then
 * reads the result.  Returns 0.0 if no sensor was detected at boot.
 */
static float read_temperature(void)
{
    if (!s_ds) return 0.0f;
    float t = 0.0f;
    ds18b20_trigger_temperature_conversion(s_ds);
    vTaskDelay(pdMS_TO_TICKS(750));   // 12-bit conversion time
    ds18b20_get_temperature(s_ds, &t);
    return t;
}

// ── WiFi ──────────────────────────────────────────────────────────────────────

static void wifi_event_handler(void *, esp_event_base_t base,
                               int32_t id, void *)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        ESP_LOGW(TAG, "WiFi disconnected — reconnecting");
        xEventGroupClearBits(s_ev, EV_WIFI_UP);
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        xEventGroupSetBits(s_ev, EV_WIFI_UP);
        ESP_LOGI(TAG, "WiFi connected");
    }
}

static void wifi_start(void)
{
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();

    wifi_init_config_t init = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&init));

    ESP_ERROR_CHECK(esp_event_handler_register(
        WIFI_EVENT, ESP_EVENT_ANY_ID, wifi_event_handler, nullptr));
    ESP_ERROR_CHECK(esp_event_handler_register(
        IP_EVENT, IP_EVENT_STA_GOT_IP, wifi_event_handler, nullptr));

    wifi_config_t cfg = {};
    strncpy((char *)cfg.sta.ssid,     CONFIG_WIFI_SSID,     sizeof(cfg.sta.ssid) - 1);
    strncpy((char *)cfg.sta.password, CONFIG_WIFI_PASSWORD, sizeof(cfg.sta.password) - 1);

    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &cfg));
    ESP_ERROR_CHECK(esp_wifi_start());
    ESP_ERROR_CHECK(esp_wifi_connect());
}

static void build_mac_string(void)
{
    uint8_t mac[6];
    esp_wifi_get_mac(WIFI_IF_STA, mac);
    snprintf(s_mac_str, sizeof(s_mac_str), "%02X%02X%02X%02X%02X%02X",
             mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    ESP_LOGI(TAG, "MAC: %s", s_mac_str);
}

// ── MQTT message dispatch ─────────────────────────────────────────────────────

static void handle_setup_response(const char *data, int data_len)
{
    cJSON *root = cJSON_ParseWithLength(data, data_len);
    if (!root) return;

    const cJSON *id = cJSON_GetObjectItemCaseSensitive(root, "assign_id");
    if (cJSON_IsString(id) && id->valuestring[0]) {
        strncpy(s_plant_id, id->valuestring, sizeof(s_plant_id) - 1);
        nvs_save_plant_id(s_plant_id);

        // Subscribe to the command topic now that we have an identity.
        char cmd_topic[80];
        snprintf(cmd_topic, sizeof(cmd_topic), "garden/command/%s", s_plant_id);
        esp_mqtt_client_subscribe(s_mqtt, cmd_topic, 1);
        ESP_LOGI(TAG, "subscribed to %s", cmd_topic);

        xEventGroupSetBits(s_ev, EV_PROVISION);
        ESP_LOGI(TAG, "provisioned as %s", s_plant_id);
    }
    cJSON_Delete(root);
}

static void handle_command(const char *data, int data_len)
{
    cJSON *root = cJSON_ParseWithLength(data, data_len);
    if (!root) return;

    const cJSON *act = cJSON_GetObjectItemCaseSensitive(root, "action");
    if (cJSON_IsString(act)) {
        if      (strcmp(act->valuestring, "water_on")  == 0) pump_set(true);
        else if (strcmp(act->valuestring, "water_off") == 0) pump_set(false);
        else ESP_LOGW(TAG, "unknown action: %s", act->valuestring);
    }
    cJSON_Delete(root);
}

static void on_mqtt_data(const char *topic, int topic_len,
                         const char *data,  int data_len)
{
    // garden/setup/{MAC}
    char setup_topic[80];
    int setup_len = snprintf(setup_topic, sizeof(setup_topic),
                             "garden/setup/%s", s_mac_str);
    if (topic_len == setup_len && strncmp(topic, setup_topic, topic_len) == 0) {
        handle_setup_response(data, data_len);
        return;
    }

    // garden/command/{plant_id}
    if (s_plant_id[0]) {
        char cmd_topic[80];
        int cmd_len = snprintf(cmd_topic, sizeof(cmd_topic),
                               "garden/command/%s", s_plant_id);
        if (topic_len == cmd_len && strncmp(topic, cmd_topic, topic_len) == 0) {
            handle_command(data, data_len);
        }
    }
}

// ── MQTT event handler ────────────────────────────────────────────────────────

static void mqtt_event_handler(void *, esp_event_base_t,
                               int32_t id, void *event_data)
{
    auto *ev = static_cast<esp_mqtt_event_handle_t>(event_data);
    switch (id) {
    case MQTT_EVENT_CONNECTED:
        ESP_LOGI(TAG, "MQTT connected");
        xEventGroupSetBits(s_ev, EV_MQTT_UP);

        // Always (re-)subscribe to our setup topic so reprovisioning works.
        {
            char setup_topic[80];
            snprintf(setup_topic, sizeof(setup_topic), "garden/setup/%s", s_mac_str);
            esp_mqtt_client_subscribe(s_mqtt, setup_topic, 1);
        }
        // If already provisioned, re-subscribe to the command topic.
        if (s_plant_id[0]) {
            char cmd_topic[80];
            snprintf(cmd_topic, sizeof(cmd_topic), "garden/command/%s", s_plant_id);
            esp_mqtt_client_subscribe(s_mqtt, cmd_topic, 1);
        }
        break;

    case MQTT_EVENT_DISCONNECTED:
        ESP_LOGW(TAG, "MQTT disconnected");
        xEventGroupClearBits(s_ev, EV_MQTT_UP);
        break;

    case MQTT_EVENT_DATA:
        on_mqtt_data(ev->topic, ev->topic_len, ev->data, ev->data_len);
        break;

    default:
        break;
    }
}

static void mqtt_start(void)
{
    const esp_mqtt_client_config_t cfg = {
        .broker = { .address = { .uri = CONFIG_MQTT_BROKER_URI } },
    };
    s_mqtt = esp_mqtt_client_init(&cfg);
    ESP_ERROR_CHECK(esp_mqtt_client_register_event(
        s_mqtt, (esp_mqtt_event_id_t)ESP_EVENT_ANY_ID,
        mqtt_event_handler, nullptr));
    ESP_ERROR_CHECK(esp_mqtt_client_start(s_mqtt));
}

// ── Telemetry task ────────────────────────────────────────────────────────────

static void telemetry_task(void *)
{
    TickType_t last_wake = xTaskGetTickCount();
    for (;;) {
        // Block until the node is both provisioned and MQTT is up.
        xEventGroupWaitBits(s_ev, EV_PROVISION | EV_MQTT_UP,
                            pdFALSE, pdTRUE, portMAX_DELAY);

        const float moisture = read_moisture();
        const float temp     = read_temperature();

        char buf[128];
        snprintf(buf, sizeof(buf),
                 "{\"plant_id\":\"%s\",\"moisture\":%.1f,\"temp\":%.1f}",
                 s_plant_id, moisture, temp);
        esp_mqtt_client_publish(s_mqtt, "garden/telemetry", buf, 0, 1, 0);
        ESP_LOGI(TAG, "telemetry: %s", buf);

        vTaskDelayUntil(&last_wake, pdMS_TO_TICKS(TELEMETRY_MS));
    }
}

// ── Setup beacon task (active only before provisioning) ───────────────────────

static void setup_beacon_task(void *)
{
    for (;;) {
        // Self-terminate once provisioned.
        if (xEventGroupGetBits(s_ev) & EV_PROVISION) {
            ESP_LOGI(TAG, "provisioned — beacon task done");
            vTaskDelete(nullptr);
            return;
        }

        // Wait for MQTT before sending.
        xEventGroupWaitBits(s_ev, EV_MQTT_UP, pdFALSE, pdTRUE, portMAX_DELAY);

        char buf[96];
        snprintf(buf, sizeof(buf),
                 "{\"mac\":\"%s\",\"status\":\"awaiting_provision\"}", s_mac_str);
        esp_mqtt_client_publish(s_mqtt, "garden/setup", buf, 0, 1, 0);
        ESP_LOGI(TAG, "setup beacon sent");

        vTaskDelay(pdMS_TO_TICKS(SETUP_BEACON_MS));
    }
}

// ── Entry point ───────────────────────────────────────────────────────────────

extern "C" void app_main(void)
{
    ESP_LOGI(TAG, "herb-edge starting");

    // Initialise NVS (erase if partition is full or schema changed).
    esp_err_t ret = nvs_flash_init();
    if (ret == ESP_ERR_NVS_NO_FREE_PAGES ||
        ret == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_LOGW(TAG, "NVS partition wiped");
        ESP_ERROR_CHECK(nvs_flash_erase());
        ESP_ERROR_CHECK(nvs_flash_init());
    }

    s_ev = xEventGroupCreate();

    nvs_load_plant_id();   // sets EV_PROVISION if plant_id exists
    sensors_init();
    wifi_start();

    // Need WiFi up before querying MAC and starting MQTT.
    xEventGroupWaitBits(s_ev, EV_WIFI_UP, pdFALSE, pdTRUE, portMAX_DELAY);
    build_mac_string();
    mqtt_start();

    // Telemetry runs always (blocks internally until provisioned + MQTT up).
    xTaskCreate(telemetry_task, "telemetry", 4096, nullptr, 5, nullptr);

    // Beacon only needed if not yet provisioned.
    if (!(xEventGroupGetBits(s_ev) & EV_PROVISION)) {
        xTaskCreate(setup_beacon_task, "beacon", 2048, nullptr, 4, nullptr);
    }

    ESP_LOGI(TAG, "startup complete");
}
