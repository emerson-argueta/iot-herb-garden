idf_component_register(
    SRCS "main.cpp"
    INCLUDE_DIRS "."
    REQUIRES
        nvs_flash
        esp_wifi
        esp_netif
        esp_event
        esp_timer
        mqtt
        esp_adc
        driver
        cJSON
        onewire_bus
        ds18b20
)
