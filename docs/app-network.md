# App: BLE provisioning, network operation

This local variant uses Bluetooth only to configure the board. The matching App and firmware are required together.

## Setup and reconnection

1. The App pairs with the board through the existing bonded BLE service and writes the Wi-Fi settings.
2. The encrypted `net` characteristic supplies the board IP, per-boot video token (`vtok`) and persistent App network key (`atok`, 32 bytes as 64 hex characters). The ordinary CLI `net` response excludes both tokens.
3. The App opens TCP port **7203**, verifies the board's identity and proves its own session key. It saves the endpoint in the iOS Keychain before requesting `netonly on`.
4. The board acknowledges, persists the network-only setting and restarts once. It does not initialize the Bluetooth controller on subsequent boots. The App cancels its BLE connection and stops BLE scanning/reconnection.
5. The App reconnects directly over TCP, refreshes `vtok` over its authenticated connection and resumes the existing video channel on TCP **7201**. Status and control commands use 7203. The computer continues using its independent encrypted channel on **7202**.

The App receives a complete status object every second. Two-second encrypted pings keep its session alive. The board drops an inactive socket after six seconds; the App reconnects after socket failure or six seconds without an authenticated message. Returning to the foreground immediately checks for a stale connection. A network failure does not turn BLE back on.

The existing App/CLI arbitration applies to both App transports: active App commands own control, while an App that only watches status or pings can coexist with an idle computer connection.

## Recovery

Settings → Board shows the transport, live board state and saved IP. If DHCP changes the address, update it there; reserving the address on the network avoids this step.

“重新配置 Wi-Fi” sends `netonly off`, clears the App's saved endpoint and returns to BLE provisioning. If the network is unreachable, connect the board by USB, send `0 setup` plus a newline at 115200 baud, and forget/reconfigure the board in the App. Existing Wi-Fi credentials and pairing keys are retained.

When migrating a board already in computer-only network mode, stop the computer daemon (`esp32agent daemon stop`) before sending USB `0 setup`, then open the updated App to complete provisioning. Restart the computer daemon after the App completes the handoff. This prevents the computer from disabling BLE during the App's initial setup.

Forgetting only in the App does not remotely revoke a credential or re-enable the board's radio. Use the reconfiguration action first when possible.

## App protocol on TCP 7203

Every record is `[u16 little-endian length][frame]`, with lengths 1…1100. Partial and coalesced TCP records are supported.

- Client hello: `02 || client_nonce[16]`.
- Board welcome: `82 || board_nonce[16] || HMAC-SHA256(master, "app-welcome-v1" || client_nonce || board_nonce)[0:16]`.
- HKDF-SHA256 derives 32 bytes with the master as input, the two nonces as salt and `esp32agent-session-v1` as info. The first 16 bytes encrypt App → board; the remaining 16 bytes encrypt board → App.
- Encrypted frame: `20 || u32_LE(counter) || ciphertext || tag[16]`. AES-128-GCM authenticates the five-byte header. The nonce is `[stream][seven zero bytes][u32_LE(counter)]`, where stream is `C` for App → board and `B` for board → App.
- Counters begin at 1, increase exactly by one, and must not wrap. Invalid tags, replayed/reordered frames and invalid records close the connection.
- App plaintext: `c<request-id> <command>`; board plaintext: `r<request-id> ok|err …` or `s<status JSON>`.
- The first valid encrypted command authenticates the App. Unauthenticated sockets receive no status or command replies.
- `appnet` refreshes configuration and video credentials over the encrypted App connection. `netonly on|off` requires an authenticated network session. App credentials are not available through the CLI channel.

Shared CryptoKit/mbedtls vectors cover directional encryption, full-length tags, board proof, tampering and replay rejection. An on-device loopback test verifies split/coalesced records and reconnection with a fresh authenticated session.

## iOS background behavior

Removing BLE removes the old Bluetooth background wakeups. Ordinary network sockets do not grant unlimited iOS background execution. The existing finite background-task handling remains; after iOS suspends the App, live status resumes on foreground/reconnection. No unrelated audio/location background mode is added to keep the process alive. AirPlay receiving on the board and computer control remain independent of the App's foreground state.
