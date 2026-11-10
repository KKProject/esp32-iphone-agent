// Authenticated, encrypted App command/status channel. BLE supplies its persistent key at setup.
#pragma once
#include <Arduino.h>
bool app_tcp_begin();
bool app_tcp_connected();
bool app_tcp_active();
bool app_tcp_send(char kind, const String &text);
