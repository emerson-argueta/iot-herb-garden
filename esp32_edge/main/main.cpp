/*
 * ESP32 Edge Node Firmware
 * Hub-and-Spoke IoT Herb Garden Monitor
 */

#include <stdio.h>
#include <string.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/event_groups.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "esp_event.h"
#include "esp_log.h"
#include "nvs_flash.h"
#include "mqtt_client.h"
#include "cJSON.h"
#include "driver/adc.h"
#include "driver/temperature_sensor.h"

// Configuration
#define WIFI_SSID      "YOUR_WIFI_SSID"
#define WIFI_PASSWORD  "YOUR_WIFI_PASSWORD"
#define MQTT_BROKER    "mqtt://192.168.1.100:1883"

#define NVS_NAMESPACE  "plant_config"
#define NVS_PLANT_ID   "plant_id"

#define TELEMETRY_INTERVAL_MS  (15 * 60 * 1000)  // 15 minutes
#define MOISTURE_ADC_CHANNEL   ADC1_CHANNEL_0     // GPIO36

static const char *TAG = "EDGE_NODE";

// Global state
static char plant_id[64] = {0};
static bool provisioned = false;
static esp_mqtt_client_handle_t mqtt_client = NULL;
static EventGroupHandle_t wifi_event_group;
static const int WIFI_CONNECTED_BIT = BIT0;

// Function declarations
static void wifi_init_sta(void);
static void mqtt_init(void);
static void check_provisioning(void);
static void telemetry_task(void *pvParameters);
static float read_moisture(void);
static float read_temperature(void);

// NVS Operations
static bool load_plant_id_from_nvs(void) {
    nvs_handle_t nvs_handle;
    esp_err_t err = nvs_open(NVS_NAMESPACE, NVS_READONLY, &nvs_handle);
    if (err != ESP_OK) {
        ESP_LOGI(TAG, "NVS namespace not found, device is unprovisioned");
        return false;
    }

    size_t required_size = sizeof(plant_id);
    err = nvs_get_str(nvs_handle, NVS_PLANT_ID, plant_id, &required_size);
    nvs_close(nvs_handle);

    if (err == ESP_OK) {
        ESP_LOGI(TAG, "Loaded plant_id from NVS: %s", plant_id);
        return true;
    }

    ESP_LOGI(TAG, "No plant_id in NVS");
    return false;
}

static esp_err_t save_plant_id_to_nvs(const char *id) {
    nvs_handle_t nvs_handle;
    esp_err_t err = nvs_open(NVS_NAMESPACE, NVS_READWRITE, &nvs_handle);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Failed to open NVS: %s", esp_err_to_name(err));
        return err;
    }

    err = nvs_set_str(nvs_handle, NVS_PLANT_ID, id);
    if (err == ESP_OK) {
        err = nvs_commit(nvs_handle);
        ESP_LOGI(TAG, "Saved plant_id to NVS: %s", id);
    }

    nvs_close(nvs_handle);
    return err;
}

// WiFi Event Handler
static void wifi_event_handler(void* arg, esp_event_base_t event_base,
                               int32_t event_id, void* event_data) {
    if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_DISCONNECTED) {
        ESP_LOGI(TAG, "WiFi disconnected, reconnecting...");
        esp_wifi_connect();
        xEventGroupClearBits(wifi_event_group, WIFI_CONNECTED_BIT);
    } else if (event_base == IP_EVENT && event_id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t* event = (ip_event_got_ip_t*) event_data;
        ESP_LOGI(TAG, "Got IP: " IPSTR, IP2STR(&event->ip_info.ip));
        xEventGroupSetBits(wifi_event_group, WIFI_CONNECTED_BIT);
    }
}

// WiFi Initialization
static void wifi_init_sta(void) {
    wifi_event_group = xEventGroupCreate();

    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();

    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));

    esp_event_handler_instance_t instance_any_id;
    esp_event_handler_instance_t instance_got_ip;
    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT,
                                                        ESP_EVENT_ANY_ID,
                                                        &wifi_event_handler,
                                                        NULL,
                                                        &instance_any_id));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT,
                                                        IP_EVENT_STA_GOT_IP,
                                                        &wifi_event_handler,
                                                        NULL,
                                                        &instance_got_ip));

    wifi_config_t wifi_config = {};
    strcpy((char*)wifi_config.sta.ssid, WIFI_SSID);
    strcpy((char*)wifi_config.sta.password, WIFI_PASSWORD);
    wifi_config.sta.threshold.authmode = WIFI_AUTH_WPA2_PSK;

    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wifi_config));
    ESP_ERROR_CHECK(esp_wifi_start());

    ESP_LOGI(TAG, "WiFi initialization complete");
}

// MQTT Event Handler
static void mqtt_event_handler(void *handler_args, esp_event_base_t base,
                               int32_t event_id, void *event_data) {
    esp_mqtt_event_handle_t event = (esp_mqtt_event_handle_t)event_data;

    switch ((esp_mqtt_event_id_t)event_id) {
        case MQTT_EVENT_CONNECTED:
            ESP_LOGI(TAG, "MQTT connected");

            if (!provisioned) {
                // Subscribe to setup topic with our MAC
                uint8_t mac[6];
                esp_efuse_mac_get_default(mac);
                char setup_topic[64];
                snprintf(setup_topic, sizeof(setup_topic),
                        "garden/setup/%02X%02X%02X", mac[3], mac[4], mac[5]);
                esp_mqtt_client_subscribe(mqtt_client, setup_topic, 0);
                ESP_LOGI(TAG, "Subscribed to %s", setup_topic);

                // Broadcast unprovisioned status
                cJSON *json = cJSON_CreateObject();
                char mac_str[18];
                snprintf(mac_str, sizeof(mac_str), "%02X%02X%02X",
                        mac[3], mac[4], mac[5]);
                cJSON_AddStringToObject(json, "mac", mac_str);
                cJSON_AddStringToObject(json, "status", "awaiting_provision");
                char *payload = cJSON_PrintUnformatted(json);
                esp_mqtt_client_publish(mqtt_client, "garden/setup", payload, 0, 0, 0);
                ESP_LOGI(TAG, "Broadcast: awaiting provision (MAC: %s)", mac_str);
                free(payload);
                cJSON_Delete(json);
            }
            break;

        case MQTT_EVENT_DATA:
            ESP_LOGI(TAG, "MQTT data received on topic: %.*s", event->topic_len, event->topic);

            if (!provisioned && strstr(event->topic, "garden/setup/") != NULL) {
                // Parse assignment message
                cJSON *json = cJSON_ParseWithLength(event->data, event->data_len);
                if (json != NULL) {
                    cJSON *assign_id = cJSON_GetObjectItem(json, "assign_id");
                    if (assign_id != NULL && cJSON_IsString(assign_id)) {
                        const char *id = assign_id->valuestring;
                        ESP_LOGI(TAG, "Received plant assignment: %s", id);

                        if (save_plant_id_to_nvs(id) == ESP_OK) {
                            ESP_LOGI(TAG, "Provisioning complete, restarting...");
                            vTaskDelay(pdMS_TO_TICKS(1000));
                            esp_restart();
                        }
                    }
                    cJSON_Delete(json);
                }
            }
            break;

        case MQTT_EVENT_ERROR:
            ESP_LOGE(TAG, "MQTT error");
            break;

        case MQTT_EVENT_DISCONNECTED:
            ESP_LOGI(TAG, "MQTT disconnected");
            break;

        default:
            break;
    }
}

// MQTT Initialization
static void mqtt_init(void) {
    esp_mqtt_client_config_t mqtt_cfg = {};
    mqtt_cfg.broker.address.uri = MQTT_BROKER;

    uint8_t mac[6];
    esp_efuse_mac_get_default(mac);
    char client_id[32];
    snprintf(client_id, sizeof(client_id), "edge_%02X%02X%02X",
            mac[3], mac[4], mac[5]);
    mqtt_cfg.credentials.client_id = client_id;

    mqtt_client = esp_mqtt_client_init(&mqtt_cfg);
    esp_mqtt_client_register_event(mqtt_client, (esp_mqtt_event_id_t)ESP_EVENT_ANY_ID,
                                   mqtt_event_handler, NULL);
    esp_mqtt_client_start(mqtt_client);

    ESP_LOGI(TAG, "MQTT client started (ID: %s)", client_id);
}

// Sensor Reading Functions
static float read_moisture(void) {
    // Configure ADC for moisture sensor
    adc1_config_width(ADC_WIDTH_BIT_12);
    adc1_config_channel_atten(MOISTURE_ADC_CHANNEL, ADC_ATTEN_DB_11);

    // Read raw value
    int raw = adc1_get_raw(MOISTURE_ADC_CHANNEL);

    // Convert to percentage (calibration needed for real sensors)
    // Assuming 0-4095 maps to 0-100% (inverted: wet=high voltage, dry=low)
    float moisture = ((4095 - raw) / 4095.0f) * 100.0f;

    ESP_LOGI(TAG, "Moisture raw: %d, percentage: %.1f%%", raw, moisture);
    return moisture;
}

static float read_temperature(void) {
    // Use ESP32's internal temperature sensor
    temperature_sensor_handle_t temp_sensor = NULL;
    temperature_sensor_config_t temp_config = TEMPERATURE_SENSOR_CONFIG_DEFAULT(-10, 80);

    esp_err_t err = temperature_sensor_install(&temp_config, &temp_sensor);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Temperature sensor install failed");
        return 0.0f;
    }

    temperature_sensor_enable(temp_sensor);

    float temp_celsius;
    temperature_sensor_get_celsius(temp_sensor, &temp_celsius);

    temperature_sensor_disable(temp_sensor);
    temperature_sensor_uninstall(temp_sensor);

    ESP_LOGI(TAG, "Temperature: %.1f°C", temp_celsius);
    return temp_celsius;
}

// Telemetry Task (FreeRTOS)
static void telemetry_task(void *pvParameters) {
    ESP_LOGI(TAG, "Telemetry task started for plant: %s", plant_id);

    while (1) {
        // Wait for WiFi and MQTT connection
        xEventGroupWaitBits(wifi_event_group, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);
        vTaskDelay(pdMS_TO_TICKS(5000)); // Give MQTT time to connect

        // Read sensors
        float moisture = read_moisture();
        float temp = read_temperature();

        // Build JSON payload
        cJSON *json = cJSON_CreateObject();
        cJSON_AddStringToObject(json, "plant_id", plant_id);
        cJSON_AddNumberToObject(json, "moisture", moisture);
        cJSON_AddNumberToObject(json, "temp", temp);

        char *payload = cJSON_PrintUnformatted(json);

        // Publish telemetry
        int msg_id = esp_mqtt_client_publish(mqtt_client, "garden/telemetry", payload, 0, 0, 0);
        if (msg_id >= 0) {
            ESP_LOGI(TAG, "Published telemetry: moisture=%.1f%%, temp=%.1f°C", moisture, temp);
        } else {
            ESP_LOGE(TAG, "Failed to publish telemetry");
        }

        free(payload);
        cJSON_Delete(json);

        // Sleep for 15 minutes
        vTaskDelay(pdMS_TO_TICKS(TELEMETRY_INTERVAL_MS));
    }
}

// Main Application
extern "C" void app_main(void) {
    ESP_LOGI(TAG, "ESP32 Edge Node starting...");

    // Initialize NVS
    esp_err_t ret = nvs_flash_init();
    if (ret == ESP_ERR_NVS_NO_FREE_PAGES || ret == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ret = nvs_flash_init();
    }
    ESP_ERROR_CHECK(ret);

    // Check provisioning status
    provisioned = load_plant_id_from_nvs();

    if (provisioned) {
        ESP_LOGI(TAG, "Device is provisioned as: %s", plant_id);
    } else {
        ESP_LOGI(TAG, "Device is NOT provisioned, entering setup mode");
    }

    // Initialize WiFi
    wifi_init_sta();

    // Wait for WiFi connection
    xEventGroupWaitBits(wifi_event_group, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);

    // Initialize MQTT
    mqtt_init();

    // If provisioned, start telemetry task
    if (provisioned) {
        xTaskCreate(telemetry_task, "telemetry", 4096, NULL, 5, NULL);
    } else {
        ESP_LOGI(TAG, "Waiting for provisioning...");
    }
}
