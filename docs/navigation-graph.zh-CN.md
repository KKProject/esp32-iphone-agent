# App 导航图（SQLite）

电脑端 CLI / MCP 现在可以保存和检索 App 的页面状态图。它与已有 Intent 并存：Intent 仍是独立步骤列表；导航图让多条路线共享节点和有方向的边。

## 数据放在哪里

默认配置目录下的 `navigation/graph.sqlite3`，截图放在 `navigation/screenshots/`。macOS 的默认完整路径为：

```text
~/Library/Application Support/esp32agent/navigation/
  graph.sqlite3
  screenshots/<内容 SHA-256>.png 或 .jpeg
```

不需要在 ESP32 上运行数据库，也不需要修改手机 App。数据库使用 WAL、外键、事务和忙等待；成功/失败计数与观察记录在同一事务中更新。截图是单独的本地文件，数据库不存图片内容。引用历史步骤时复制参考图，因此历史 run 被清理不会删除图中的参考图。

| 表 | 主要内容 | 索引 |
| --- | --- | --- |
| `nodes` | App、版本、设备布局、状态 key、名称、条件 | 唯一 `(app, build, profile, state_key)` |
| `edges` | 起点、终点、动作、条件、是否启用、成功/失败次数、最近验证 | 起点 + 是否启用、终点 |
| `observations` | 实际到达节点、成功/失败、验证依据、可选截图 ID、时间 | 边 + 验证时间 |
| `screenshots` | 节点、相对文件路径、尺寸、时间 | 节点 + 时间 |

数据库版本由 `PRAGMA user_version` 管理。列表分页，默认 100 条，最多 500 条；截图工具返回可读取的绝对路径。运行数据不提交到 Git。需要手动备份时，先关闭使用数据库的 CLI/MCP 进程，再复制整个 `navigation` 目录（包括尚未收敛的 WAL 文件）。

## 怎样定义节点

节点表示**页面状态**，不是一个 App 名称或一个 URL。用固定 key 区分，例如：

```text
home.guest.no_modal
home.logged_in.no_modal
local_services.business.top
registration.list.top
registration.list.filter_open
registration.detail.merchant_123.services
```

弹窗、键盘、滚动位置、城市、账号角色等会影响点击目标时，必须区分状态。不同商家详情如果内容或按钮位置不稳定，也不要合并为同一节点。

每个节点必须提供 `app`、`build`、`profile`：

- `app`：优先使用真实 bundle ID。
- `build`：实际 App 版本/构建号；不清楚时使用独立的探索批次标识，不要把它当作已知版本复用。
- `profile`：设备、方向、显示缩放、字体设置等布局规格，由操作方保持一致。

同一组合下相同 key 会更新原节点并保留 ID。不同版本或布局不会自动合并，也不允许跨范围连边、寻路。App 更新后建立新的范围，重新验证再保存可用路线。

`conditions` 是字符串数组，表示路线使用期间保持成立的前置条件，例如 `account:guest`、`city:beijing`。规划和准备动作时，`facts` 必须包含路径上所有节点及边的条件；精确匹配，全部满足才可用。系统不会根据页面自动推断条件，也不会模拟条件变化。登录、键盘、弹窗等会随动作变化的状态应主要放在节点 key 中。

## MCP 工作流

所有图工具均在电脑本地运行，不要求板子在线。它们负责存储、规划及准备动作，实际点击仍使用已有的 `tap` 等工具。

### 1. 探索并保存节点

按原有流程逐步截图、操作、检查。确认两个不同页面状态后，分别调用 `graph_node_save`：

```json
{
  "app": "example.app",
  "build": "1.0.0-100",
  "profile": "iphone13pro-portrait-default",
  "key": "home.guest.no_modal",
  "name": "首页·游客·无弹窗",
  "conditions": ["account:guest", "city:beijing"]
}
```

返回 `id`。用相同范围和另一个 key 保存目标页，得到另一个节点 ID。`graph_node_find` 根据范围 + key 精确查找，`graph_node_list` 分页浏览，`graph_node_show` 读取节点及最近 20 张参考图。

### 2. 保存有方向的动作边

用 `history` 的 show 查找准确的动作编号，调用 `graph_edge_save`：

```json
{
  "from": "<首页节点 ID>",
  "to": "<本地服务节点 ID>",
  "label": "点击本地服务",
  "from_run": "<history run ID>",
  "step": 3,
  "conditions": ["account:guest", "city:beijing"]
}
```

`step` 是 history show 返回的 **1 起始动作编号**，不是 `steps.jsonl` 的行号或 `n`。一次只引用一个动作，不会从整段历史自动猜测页面边界。失败命令不能导入。

也可以直接提供 `action`，与 `from_run` / `step` 二选一：

```json
{
  "from": "<首页节点 ID>",
  "to": "<本地服务节点 ID>",
  "label": "点击本地服务",
  "action": {"action": "tap", "x": 880, "y": 210}
}
```

以上坐标仅为结构示例，使用时必须从实际画面确认。坐标单位是 **0–1000**，与 Intent/history 一致。支持 `tap`（可选 `count` 1–3）、`longpress`（可选 `ms`，1–10000）、`swipe`、`type`、`key`、`home`。动作格式沿用历史记录：

```json
{"action":"swipe","x1":500,"y1":800,"x2":500,"y2":300}
{"action":"longpress","x":500,"y":500,"ms":1500}
{"action":"type","text":"hello"}
{"action":"key","combo":"enter"}
{"action":"home"}
```

同一来源、目标、动作和条件会复用边 ID，不会清空验证记录。返回的 `edge` 含 ID、动作及统计；历史中有源页参考图时，还返回 `reference`。导入仅保存动作和源页参考图，初始成功次数为零，**不会把“命令执行成功”当作“到达目标页”**。若参考图文件已丢失，错误信息会明确指出边已保存但图片未复制。

### 3. 记录实际到达结果

检查操作后的画面/可靠状态信号后调用 `graph_observe`：

```json
{
  "edge": "<边 ID>",
  "actual_node": "<本地服务节点 ID>",
  "success": true,
  "evidence": "截图确认标题为本地服务，分类侧栏出现"
}
```

成功必须提供与边目标一致的 `actual_node`，并说明验证依据。可先用 `graph_screenshot_save` 的 `{ "node":"<实际节点 ID>", "path":"/absolute/screenshot.png" }` 保存实际结果图，再把返回的截图 ID 作为 `screenshot` 附在观察记录中。参考图需要属于实际节点，源页截图不能充当目标页证据。

失败用 `success:false`，说明实际情况；已重新定位时提供实际节点，不知道时省略。最近一次观察失败的边会被寻路和动作准备排除，直到成功重验。可通过原有截图/点击工具手动重验，不必强制准备已知失败的动作。`graph_edge_enable` 可以停用边；重新启用不会清除失败记录。`graph_observations` 分页查看验证依据。

### 4. 寻路并复用

先用截图或可靠信号确认当前节点，再调用 `graph_path`：

```json
{
  "from": "<已确认的当前节点 ID>",
  "to": "<目标节点 ID>",
  "facts": ["account:guest", "city:beijing"]
}
```

默认只使用有成功验证且最近未失败的启用边。探索新边时可显式加 `include_unverified:true`；这仍不会放行已知失败或停用的边。

返回顺序排列的 `edges`、总成本和访问节点数。算法使用 Dijkstra，每次按索引查询当前节点的出口，不读取整张图。每条已验证边的成本是：

```text
1 + (失败次数 + 1) / (成功次数 + 1) + min(距上次成功天数, 30) / 30
```

未验证边成本从 10 起。这样会综合步数、历史可靠性和近期验证情况。`max_visited` 默认 10000，最多 100000，达到限制会明确报错，不返回未完成的路线。

对选中的第一条边调用 `graph_prepare`：

```json
{
  "edge": "<边 ID>",
  "current_node": "<已确认的当前节点 ID>",
  "width": 720,
  "height": 1558,
  "facts": ["account:guest", "city:beijing"]
}
```

它重新检查来源、条件和边状态，按当前截图尺寸换算坐标，返回可调用的标准 MCP `tool` + `arguments`。它**不发出动作、不推进当前节点、不记录成功**。执行返回的工具后检查结果，再记录 observation；匹配后才能从下一节点继续。

`graph_edge_list` 查看出口或以 `incoming:true` 查看入口，`graph_edge_show` 查看完整动作和统计。所有列表的 `limit` / `offset` 都是可选参数。

## 命令行

`esp32agent graph <操作> --input FILE|-` 使用与 MCP 完全相同的 JSON 参数。操作名去掉 `graph_`，下划线改连字符，例如 `node-save`、`edge-save`、`path`、`prepare`；`observe` 和 `observations` 保持原名。结果在标准响应的 `data.result` 中。

```sh
esp32agent graph node-save --input node.json
esp32agent graph path --input route.json
esp32agent graph prepare --input move.json
```

标准输入也可使用：

```sh
esp32agent graph node-list --input - <<'JSON'
{"app":"example.app","build":"1.0.0-100","profile":"iphone13pro-portrait-default"}
JSON
```

## 当前边界

这层存储不自动识别页面、不后台遍历整款 App，也没有连续盲点的批量执行器。历史操作无法可靠地自动命名页面，需要探索时标注状态和验证结果；保存图中的动作也不代表授权执行其中的付款、发布、协议等操作。

已知路线可复用坐标和动作，减少重复分析。当前工具流程仍要求逐步截图核对；若后续要只在关键节点检查，需要接入可靠的状态信号、异常检测和执行器，再明确哪些边能省略检查。出现弹窗、登录过期、网络错误或布局变化时，立即停止旧路线并重新定位，不根据预期继续推进。
