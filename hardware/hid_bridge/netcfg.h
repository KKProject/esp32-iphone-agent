// Board settings in NVS and the Wi-Fi link: the board joins the local network the app wrote over BLE (no
// network yet: BLE only until it does). A build with -DSTA_FROM_SECRETS=1 joins WIFI_SSID / WIFI_PASS from the
// git-ignored secrets.h instead (for testing from a Mac).
#pragma once
#include <Arduino.h>

void net_begin();
void net_poll();                         // call from loop(): applies a pending `net` write
String net_json(bool vtok = true);       // value of the `net` characteristic (the CLI gets it without vtok)
void net_request(const String &json);    // `net` write; applied by net_poll()
const char *net_mode();                  // "sta" (on the network) | "none"
String net_ip();
int net_peers();                         // 1 when on the network, else 0
bool net_up();                           // has an IP address
bool net_coex(const String &pref);       // Wi-Fi/BLE radio share: "bt" | "balance" (boot default) | "wifi"
const char *net_coex_mode();

// AirPlay settings (take effect on the next boot of the receiver)
uint16_t cfg_width();
uint16_t cfg_height();
uint8_t cfg_fps();
void cfg_set_res(uint16_t w, uint16_t h);
void cfg_set_fps(uint8_t fps);
void cfg_ed25519_seed(uint8_t seed[32]);  // created once, stable across boots
String board_suffix();                    // "1A2B": last two BT MAC bytes

// Network-only operation survives reboot; USB `0 setup` restores BLE provisioning.
bool net_only();
bool net_set_only(bool enabled);

// Persistent app network key, disclosed only through the encrypted provisioning characteristic.
bool net_app_key(uint8_t out[32]);
