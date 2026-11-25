# 英文界面与聊天播报（i18n）

Status: in-progress

## Problem Statement

使用英文客户端（`-region EN`，登录 `locale=en-US`）的玩家，游戏界面本身是英文，
但服务端自行拼出的聊天播报（中鱼、耐力/超载、上鱼、放生、跑鱼、轮子损坏、
多钩汇总、升级礼物、福袋、水域奖杯、资格奖杯）全部是硬编码中文，鱼名也取
`loc_wiki_fish.fish_zh`。上鱼播报的 KV 模板把中文鱼名放进键1，客户端原样显示，
所以聊天里鱼名是中文。launcher 控制中心与管理后台也只有中文。

## Solution

服务端增加一个界面语言设置：英文服时聊天播报与鱼名用英文，中文服保持现状。
默认值跟随 `gateway.region`（`CN` → 中文，其他 → 英文），已有中文部署无需改配置、
行为不变。launcher 面板与管理后台后续增加 EN / 中文切换（独立阶段，见下）。

## User Stories

1. 作为英文客户端玩家，我希望中鱼、上鱼、跑鱼等聊天播报和鱼名是英文，以便看懂。
2. 作为中文服运维，我不改任何配置时，播报文案与现在逐字一致。
3. 作为运维，我可以用 `game.language`（`zh` / `en`）显式覆盖自动推断。
4. 边界：鱼种没有英文名时回退中文名，再回退 fish_key，不发空串。
5. 边界：物品名只在静态目录有英文名时显示英文；静态库约 4.7k 个物品只有中文名，
   本阶段原样显示（不改静态库）。

## Implementation Decisions

阶段划分（每阶段一个 commit，可独立回滚）：

1. **聊天播报与鱼名（本阶段）**
   - `internal/config`：`Game.Language`；`Load` 在环境变量覆盖后推断空值
     （`Gateway.Region` 为 `CN` 或空 → `zh`，其他 → `en`），非法值报错。
     环境变量 `RF4_GAME_LANGUAGE`。
   - `internal/gameserver`：新增语言选择辅助（按 `cfg.Language` 二选一文案）与
     英文鱼名表（fish_key → 官方英文名，来源 `tools/rf4records_fish_map.py`，
     250 条一一对应，取反后生成 Go 表，不写库）。
   - 改造的播报点：中鱼、耐力/超载、上鱼（文本与 KV 键1）、放生、跑鱼（含语气词池）、
     轮子/摩擦片损坏、多钩汇总、升级礼物、福袋、水域奖杯、资格奖杯。
     中文文案保持原字面量不动，英文作为并列分支。
   - 不改协议形状：仍走既有 24/27 回显与 401 KV，只换字符串内容。
2. **launcher 控制中心**（已实现）：`cmd/launcher/panel_i18n.js` 为中→英字典与 DOM
   翻译层，launcher 服务 `/` 时注入到 `</body>` 前（`panelPage`），**不改 panel.html**，
   上游面板改动可直接合入；新增未翻译文案保持中文。默认语言 = `game.language`，
   右下角 EN / 中文 按钮可覆盖（localStorage）。`#logs` 内的原始日志不翻译。
3. **管理后台**（已实现）：翻译引擎抽为共享的 `internal/admin/web/i18n/engine.js`
   （控制面板经 `admin.I18nEngineJS()` 内联同一份），后台字典 `admin_dict.js`；
   admin 服务 `index.html` 时注入（带内容哈希 `?v=`，升级后不吃旧缓存），SPA 源文件不改。
   默认语言经 `Server.SetLanguage(game.language)`；「关于」页英文时读 `README.en.md`
   （发布包同时带上）；渲染出的文档（`.md`）不做逐词替换。单字（件/条/级…）只在
   数字或空白旁替换，避免误伤中文名称；属性只在值变化时回写（防 MutationObserver 死循环）。
   数据库里的鱼名、物品名不在此阶段（见阶段 4/5）。
4. **数据库名称**（已实现，管理后台侧）：`tools/extract_client_names.py` 从客户端
   `resources.assets` 的 `en_US`/`zh_CN` 本地化导出 `ref/client_names.json`（官方客户端
   内容不入库）；`internal/admin/names.go` 按 item_id / fish_key 把库内中文名映射为英文，
   以 `i18n/names_en.js` 下发给翻译引擎。数据库一行不改；缺文件时名称保持中文。
   规则说明类自由文本（如鱼饵前置条件）不做翻译。游戏服聊天里的物品名留待后续。
5. **游戏内查鱼**（`fish_query_*`）：回复文案与英文鱼名输入解析，单独评估。

数据与迁移：无表结构变更，不写静态库。

兼容：客户端基线 4.0.25034；回滚 = revert 对应阶段 commit，配置项缺省即旧行为。

## Testing Decisions

- 单元层（`internal/config`）：空 `language` 按 region 推断；显式值优先；非法值报错。
- 单元层（`internal/gameserver`）：`language=en` 时各播报文本的已知良好英文字面量；
  `zh` 时沿用既有中文断言（`fishing_hooked_chat_test.go` 等不改期望值）。
- 英文鱼名表：抽查若干已知映射（`perch`→Perch、`vjun`→Loach、`e.pikeperch`→Zander），
  期望值取自 `tools/rf4records_fish_map.py` 的字面量。
- 回滚后：英文断言变红，中文断言保持绿。
- 行为层：实机英文客户端中鱼一次，确认聊天显示英文鱼名（未验证前不标 done）。

## Out of Scope

- 静态库物品中文名的翻译（无数据源）。
- 客户端自身本地化文本（由客户端负责）。
- 管理员手写的邮件/广播内容。
- 调试探针文案（如成就推送探针）。

## Further Notes

- `[事实]` 客户端区域枚举 0=GL…3=EN、4=CN：`internal/wiregen/tables.go`。
- `[事实]` 英文客户端登录请求携带 `region=EN&locale=en-US`（本机 gateway 日志，2026-11-24）。
- `[事实]` 上鱼 KV 键1 为显示用鱼名：`internal/gameserver/fishing.go` 的 `fishLandedKV` 注释。
- `[推断]` 英文鱼名放进键1 后客户端同样原样显示；实机未验证。
