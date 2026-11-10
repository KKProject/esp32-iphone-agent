// Board settings in NVS and the Wi-Fi link, see netcfg.h.
#include "netcfg.h"
#include <WiFi.h>
#include <Preferences.h>
#include <esp_mac.h>
#include <esp_random.h>
#include <esp_coexist.h>
#include "wifi_video.h"
#include "logbuf.h"

#if STA_FROM_SECRETS
#if __has_include("secrets.h")
#include "secrets.h"  // WIFI_SSID, WIFI_PASS (git-ignored)
#define BOOT_STA 1
#else
#warning "STA_FROM_SECRETS set but secrets.h is missing: using the Wi-Fi stored on the board"
#endif
#endif

static Preferences prefs;
static String staSsid, staPass, mode;  // mode: "sta" | "off"
static String pending;
static bool networkOnly;
static uint8_t appKey[32];
static bool appKeyReady;
static SemaphoreHandle_t pendingLock = xSemaphoreCreateMutex();
// STA retries back off: a tight reconnect loop starves BLE, which shares the radio.
static uint32_t retryAt = 0, retryGap = 5000;
static volatile int staWhy;  // why the last try failed (wifi_err_reason_t), 0 = no failure; `why` in net_json()

String board_suffix() {
  uint8_t mac[6];
  esp_read_mac(mac, ESP_MAC_BT);
  char s[5];
  snprintf(s, sizeof s, "%02X%02X", mac[4], mac[5]);
  return s;
}

// Value of a string field in a flat JSON object ("" if absent). Handles \" and \\ escapes.
static String jsonStr(const String &j, const char *key) {
  int k = j.indexOf(String("\"") + key + "\"");
  if (k < 0) return "";
  int c = j.indexOf(':', k);
  int q = c < 0 ? -1 : j.indexOf('"', c);
  if (q < 0) return "";
  String v;
  for (int i = q + 1; i < (int)j.length() && j[i] != '"'; i++) v += j[i] == '\\' && i + 1 < (int)j.length() ? j[++i] : j[i];
  return v;
}

static String jsonEsc(const String &s) {
  String o;
  for (char ch : s) {
    if (ch == '"' || ch == '\\') o += '\\';
    o += ch;
  }
  return o;
}

static void startWifi() {
  WiFi.disconnect(true);
  WiFi.setHostname("iphone-agent");
  staWhy = 0;
  if (mode == "off") {  // diagnostics: BLE alone on the radio
    WiFi.mode(WIFI_OFF);
    return;
  }
  WiFi.mode(WIFI_STA);  // sockets opened at boot need the network stack
  // BLE provisioning requires modem sleep for coexistence. With Bluetooth disabled,
  // keep Wi-Fi awake so the computer's commands do not wait for power-save delivery.
  WiFi.setSleep(!networkOnly);
  // The phone powers the board: full TX power's current peaks made it drop its USB power (the board then came back
  // with reset reason "power-on"). The router is further away than the hotspot's phone was, so not the hotspot's 5 dBm.
  WiFi.setTxPower(WIFI_POWER_11dBm);
  if (staSsid.isEmpty()) return;  // waits for a `net` write over BLE
  WiFi.setAutoReconnect(false);
  retryGap = 5000;
  retryAt = millis() + retryGap;
  WiFi.setMinSecurity(staPass.isEmpty() ? WIFI_AUTH_OPEN : WIFI_AUTH_WPA2_PSK);  // the core rejects open networks by default
  WiFi.begin(staSsid.c_str(), staPass.c_str());
}

static void onWifi(arduino_event_id_t e, arduino_event_info_t info) {
  if (e == ARDUINO_EVENT_WIFI_STA_DISCONNECTED) staWhy = info.wifi_sta_disconnected.reason;
  if (e == ARDUINO_EVENT_WIFI_STA_GOT_IP) staWhy = 0;
#if DEBUG_SERIAL
  if (e == ARDUINO_EVENT_WIFI_STA_DISCONNECTED) board_log("wifi: disconnected, reason %d", info.wifi_sta_disconnected.reason);
  if (e == ARDUINO_EVENT_WIFI_STA_GOT_IP) board_log("wifi: got ip %s", WiFi.localIP().toString().c_str());
#endif
}

void net_begin() {
  WiFi.onEvent(onWifi);
  prefs.begin("agent", false);
  networkOnly = prefs.getBool("net_only", false);
  appKeyReady = prefs.getBytes("app_key",appKey,sizeof appKey) == sizeof appKey;
  if (!appKeyReady) {
    esp_fill_random(appKey,sizeof appKey);
    appKeyReady = prefs.putBytes("app_key",appKey,sizeof appKey) == sizeof appKey;
  }
  mode = prefs.getString("mode", "sta");
  if (mode != "off") mode = "sta";  // boards from before this firmware stored "ap"
  staSsid = prefs.getString("sta_ssid", "");
  staPass = prefs.getString("sta_pass", "");
#ifdef BOOT_STA
  mode = "sta";  // build-time test network, not stored
  staSsid = WIFI_SSID;
  staPass = WIFI_PASS;
#endif
  startWifi();
  net_coex("balance");
}

static const char *coexNames[] = {"wifi", "bt", "balance"};  // esp_coex_prefer_t order
static int coexPref = ESP_COEX_PREFER_BALANCE;

bool net_coex(const String &pref) {
  for (int i = 0; i < 3; i++)
    if (pref == coexNames[i] && esp_coex_preference_set((esp_coex_prefer_t)i) == ESP_OK) {
      coexPref = i;
      return true;
    }
  return false;
}

const char *net_coex_mode() { return coexNames[coexPref]; }

void net_request(const String &json) {
  xSemaphoreTake(pendingLock, portMAX_DELAY);
  pending = json;
  xSemaphoreGive(pendingLock);
}

static void retrySta() {
  if (mode != "sta" || staSsid.isEmpty()) return;
  if (WiFi.status() == WL_CONNECTED) {
    retryGap = 5000;
    return;
  }
  if ((int32_t)(millis() - retryAt) < 0) return;
  WiFi.disconnect();
  WiFi.begin(staSsid.c_str(), staPass.c_str());
  retryGap = min<uint32_t>(retryGap * 2, 60000);
  retryAt = millis() + retryGap;
}

void net_poll() {
  retrySta();
  xSemaphoreTake(pendingLock, portMAX_DELAY);
  String j = pending;
  pending = "";
  xSemaphoreGive(pendingLock);
  if (j.isEmpty()) return;
  String m = jsonStr(j, "mode");
  if (m == "off") {
    mode = m;
  } else if (m == "sta" && jsonStr(j, "ssid").length()) {
    mode = "sta";
    staSsid = jsonStr(j, "ssid");
    staPass = jsonStr(j, "pass");
    prefs.putString("sta_ssid", staSsid);
    prefs.putString("sta_pass", staPass);
  } else {
    return;  // malformed write: keep the current link
  }
  prefs.putString("mode", mode);
  startWifi();
}

String net_json(bool vtok) {
  // The password only goes in: nothing reads it back, also not USB serial's `netinfo`.
  String j = String("{\"mode\":\"") + mode + "\",\"ssid\":\"" + jsonEsc(staSsid) + "\",\"ip\":\"" + net_ip() +
             "\",\"why\":" + String((int)staWhy);
  j += String(",\"network_only\":") + (networkOnly ? "true" : "false");
  if (vtok && appKeyReady) {
    char hex[65];
    for (int i=0;i<32;i++) snprintf(hex+2*i,3,"%02x",appKey[i]);
    j += String(",\"atok\":\"") + hex + "\"";
  }
  return j + (vtok ? ",\"vtok\":\"" + wv_token() + "\"}" : "}");
}

const char *net_mode() {
  return net_up() ? "sta" : "none";
}

String net_ip() { return WiFi.localIP().toString(); }
int net_peers() { return net_up(); }
bool net_up() { return mode == "sta" && WiFi.status() == WL_CONNECTED; }

uint16_t cfg_width() { return prefs.getUShort("w", 480); }
uint16_t cfg_height() { return prefs.getUShort("h", 1040); }
uint8_t cfg_fps() { return prefs.getUChar("fps", 5); }

void cfg_set_res(uint16_t w, uint16_t h) {
  prefs.putUShort("w", w);
  prefs.putUShort("h", h);
}

void cfg_set_fps(uint8_t fps) { prefs.putUChar("fps", fps); }

void cfg_ed25519_seed(uint8_t seed[32]) {
  if (prefs.getBytes("ed_seed", seed, 32) == 32) return;
  esp_fill_random(seed, 32);
  prefs.putBytes("ed_seed", seed, 32);
}

bool net_only() { return networkOnly; }
bool net_set_only(bool enabled) {
  if (prefs.putBool("net_only", enabled) != 1) return false;
  networkOnly = enabled;
  return true;
}

bool net_app_key(uint8_t out[32]) {
  if (!appKeyReady) return false;
  memcpy(out,appKey,32);
  return true;
}
