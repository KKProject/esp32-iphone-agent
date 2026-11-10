---
name: esp32agent
description: See and drive a real iPhone through an ESP32 Agent board (USB mouse/keyboard + AirPlay screen mirroring, reached over Bluetooth). Use when asked to look at, tap, type on, or operate the iPhone, or to test an iOS app on the device.
---

# esp32agent

`esp32agent` talks to an ESP32 Agent board plugged into the iPhone. The board is the phone's USB mouse and
keyboard and receives its AirPlay screen mirroring; this computer reaches it over Bluetooth. Every command prints
JSON: `{"status":"ok","data":...}` or `{"status":"error","error":"..."}`. The MCP server (`esp32agent mcp`)
offers the same as tools: screenshot, tap, longpress, swipe, type_text, press_key, press_button, device_info,
plus history, intent_list, intent_show, intent_save and the graph_* navigation tools.

## Setup (once)
- `esp32agent pair` within 3 minutes of plugging the board into the iPhone.
- Screen Mirroring on the iPhone: Control Center > Screen Mirroring > ESP32 Agent.
- `esp32agent calibrate` (also runs by itself on the first tap). It needs the mouse pointer visible on screen.

## The loop
1. `esp32agent screenshot` (writes a PNG in the current directory and prints its `filePath`; read the image).
   It waits until the screen has settled after your last action, so it never shows the screen from before it.
2. Decide one action from what you see.
3. Act, with coordinates in pixels of that screenshot (full size: don't pass `--scale`/`--max-size` if you
   take coordinates from it):
   - `esp32agent io tap X,Y` (add `--aim` after a missed tap: finds the pointer and corrects first; `--count 2` double-taps, e.g. to zoom a map or select a word)
   - `esp32agent io longpress X,Y --duration 1500`
   - `esp32agent io swipe X1,Y1,X2,Y2` (scroll a list: swipe up from the lower to the upper middle)
   - `esp32agent io text 'hello'`, `esp32agent io keys enter`, `esp32agent io keys cmd+a`
   - `esp32agent io button HOME` (Home Screen)
4. Screenshot again and check the result before the next step. Never chain several taps blind.

When the task depends on what the phone plays (a video, a call, a voice message, a spoken code),
`esp32agent listen` gives its words from the last 30 s, or says why the phone can't be heard (listening off,
the app not sharing, no Wi-Fi). The words can be partial or misheard; check the screen when it matters.

## Typing
- The board types printable ASCII only, one key per character.
- Chinese is typed as pinyin: `io text '拿铁'` types `natie` (the reply says `typed_as`). With the iPhone's Chinese (pinyin)
  keyboard active, commit the first candidate with `io keys space`, or screenshot and tap the right candidate.
  Many search boxes accept the plain pinyin as well.
- The board is a hardware keyboard, so iOS may hide the on-screen keyboard; the text field still takes the keys.
- Tap the text field first so it has focus; `io keys cmd+a` then `io keys backspace` clears it.

## Save what worked
After a task succeeded, save it so the next run is faster:
1. `esp32agent history show latest` (MCP: `history` with action show): the run's steps in 0-1000 screen units,
   numbered, each with the screenshot it was done on. Every action and screenshot is recorded by itself; a new run
   starts after 10 minutes idle, or with `esp32agent history new` before a new task.
2. Pick the steps that did the job (skip detours, misses and retries).
3. `esp32agent intent save --name 'Alarm 7:00' --prompt 'Set an alarm for 7:00 AM every day' --hint 'Type the time
   instead of scrolling the wheel' --from-run RUN --steps 2,3,5` (MCP: `intent_save` with name, prompt, hints,
   from_run, steps). One short, clear instruction as the prompt and at most 6 concrete hints. Only replayable moves
   can be steps: tap, swipe, type, key, home. The same name replaces the old intent.

## Run a saved intent
1. `esp32agent intent list`, then `esp32agent intent show NAME` (MCP: `intent_list`, `intent_show`): the prompt,
   hints and recorded steps, each with its reference screen (a JPEG path).
2. Follow the steps as references, not blindly: coordinates are 0-1000 units of the screen, so convert them to
   pixels of your screenshot (x * width / 1000, y * height / 1000).
3. Take a screenshot after each step and check it looks like the next step's reference screen.
4. On a mismatch (another screen, a popup, a changed layout) drop the recorded steps and go on with the normal loop,
   using the prompt and hints.

## Build and reuse an app navigation graph

For reusable navigation across several routes, prefer the SQLite graph to duplicating whole intents.
The graph tools run locally without a board; they neither detect the current page nor send input.

1. Confirm page states through the normal screenshot/action/check loop. Save each distinct state with
   `graph_node_save`: app identifier, exact build, layout profile, stable state key, readable name and required
   environment conditions. Distinguish login, popup, keyboard, city and scroll states. Find existing states with
   `graph_node_find` (scope + key) or `graph_node_list` (paginated). Never assume a build or reuse another layout.
2. After an observed transition, `graph_edge_save` stores its source/target IDs, label and one 0-1000 action.
   Prefer `from_run` + `step` from history: this copies the action and source reference image into graph storage.
   Only the command was delivered; a newly saved edge is still unverified. Do not infer arrival from tap done.
3. Verify the actual destination, then `graph_observe` with edge ID, actual_node, explicit success boolean and
   evidence describing what was checked. Optionally attach an image saved with `graph_screenshot_save`.
   If a popup or error occurs, record failure; re-localize rather than advancing the expected node.
4. For a new navigation task, confirm the current node, then `graph_path` from it to the goal with known facts.
   All node/edge conditions must be present in facts. Default paths exclude unverified, disabled and latest-failed
   edges. Use include_unverified only for exploration; manually retry failed edges and verify before reuse.
5. `graph_prepare` converts ONE edge to a normal MCP tool and pixel arguments using the current screenshot
   width/height and confirmed current_node; it rechecks conditions/status but does not execute anything.
   Run that action, screenshot and check, then record the result before continuing. Preserve the normal
   permission requirements of the action even if a saved path contains it. Never batch blind actions.

CLI equivalents use `esp32agent graph node-save|node-find|node-list|node-show|edge-save|edge-list|edge-show|edge-enable|observe|observations|screenshot-save|screenshot-list|path|prepare --input FILE|-`
with the same JSON arguments as MCP. The database and managed screenshots are under the config directory's
`navigation/` folder; existing intents are unchanged. See `docs/navigation-graph.zh-CN.md` in the repository
for field definitions and examples. No automatic page recognition or full-app exploration is provided.

## Errors to act on
- `busy: the app is using the phone`: the iOS app is driving right now. Wait and retry later; don't fight it.
- `screen mirroring is off`: ask the user to start Screen Mirroring to ESP32 Agent.
- `video frozen`: ask the user to stop and restart Screen Mirroring.
- `not paired`: ask the user to replug the board and run `esp32agent pair` within 3 minutes.
- `phone usb asleep`: the phone is locked; ask the user to unlock it.
- `not supported on ESP32 Agent`: there is no app list, UI tree, file access or webview; work from screenshots.

## Limits
- Video comes over Bluetooth: a screenshot can be up to a second behind, animations look choppy.
- Only HOME as a button; no volume, power, or app install/launch (open apps from the Home Screen or Spotlight:
  `io button HOME`, swipe down in the middle, `io text 'Settings'`, `io keys enter`).
