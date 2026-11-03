# 上游接口契约（锁定自源码，2026-11-02 拉取）

本文档是 `ops_collector` 两个 adapter 的实现依据。所有端点/字段均从上游源码确认，
不是猜测。源码获取方式：jsdelivr CDN（`https://cdn.jsdelivr.net/gh/<repo>@main/<path>`，
GitHub 直连在中国网络不可达，jsdelivr 实测可用）。

## 1. new-api（QuantumNous/new-api，AGPL-3.0-or-later）

来源文件（@main，2026-11-02）：
- `router/api-router.go` — 路由与认证中间件
- `controller/user.go` — 登录请求/响应结构
- `controller/log.go` — 日志查询参数
- `common/page_info.go` — 分页
- `middleware/auth.go` — Bearer 鉴权链
- `web/src/features/performance-metrics/{api,types}.ts` — 性能指标契约
- 本地参考快照：`C:\Users\Administrator\Desktop\角色卡\.kd-reference\new-api-pricing\`

### 认证
- `POST /api/user/login`，JSON `{username, password}`（服务端开启加密时改发
  `password_encrypted` + `encryption_key_id`）。
  成功响应：`{success:true, message:"", data:{access_token, token_type,
  access_expires_at, session, user:{id, username, ...}}}`（`controller/user.go:198-215`）。
- 加密密钥：`GET /api/user/login/encryption-key` → `{enabled, kid?, public_key?}`。
  enabled=true 时客户端需用 RSA 公钥加密密码。**上游前端具体 padding 未从源码确认**
  （jsdelivr 对 `web/src/helpers/auth.jsx` 持续 404），实现按 OAEP-SHA256 →
  PKCS1v15 顺序尝试，失败则报清晰错误并建议改用站点访问令牌（PAT）。
- `middleware.TurnstileCheck()` 挂在登录路由上：站点开启 Turnstile 人机校验时，
  自动化密码登录不可用 → 必须改用 PAT。
- 2FA：登录可能返回 challenge（`setupLogin`）→ 本工具不支持自动过 2FA，报错建议 PAT。
- 后续请求鉴权：`Authorization: Bearer <access_token>`（内部门票或用户在安全设置里
  创建的 PAT 均走此链路，`middleware/auth.go:205-235`）；另附带
  `New-Api-User: <user_id>` 头以兼容旧版网关。
- 会话续期：`POST /api/user/auth/refresh`（依赖 refresh cookie）。

### 日志（调用记录）
- `GET /api/log/self?p=<页>&page_size=<≤100>&start_timestamp=<秒>&end_timestamp=<秒>`
  （`UserAuth`；时间范围有服务端上限 `limitLogTimeRange`，超限报错）。
- 统计：`GET /api/log/self/stat?start_timestamp=&end_timestamp=&model_name=&group=`。
- 管理员另有 `/api/log/`（AdminAuth），普通运营账号用不到。

### 分组倍率
- `GET /api/pricing`（匿名可用，登录后信息更全）→
  `{success, data:[{model_name, quota_type(0=按倍率,1=按次), model_ratio,
  completion_ratio, model_price, enable_groups[], group_ratio?{组:倍率}, ...}],
  vendors:[], group_ratio:{默认:1.0,...}, usable_group:{}, supported_endpoint:{},
  auto_groups:[]}`。
- 用户可用分组：`GET /api/user/self/groups`。

### 性能指标（成功率/首字/吞吐）
- 汇总：`GET /api/perf-metrics/summary?hours=24&recent_model=m1&recent_model=m2`
  → `data:{summary?, window_start?, window_end?, models:[{model_name,
  avg_latency_ms, success_rate, avg_tps, request_count?, recent_success_series?}]}`。
- 单模型按分组：`GET /api/perf-metrics?model=<name>&hours=24` →
  `data:{summary?, series?, groups:[{group, avg_ttft_ms, avg_latency_ms,
  success_rate, avg_tps, series[]}], window_start?, window_end?}`。
- **口径**：`avg_ttft_ms` 即"首字"（TTFT）；`avg_tps` 即 tk/s；
  `success_rate` 上游定义为"排除业务拒绝、含当前不完整小时"（前端 UI 文案）。
  三个指标都是站点原生聚合（native），非本地推算。

## 2. sub2api（Wei-Shaw/sub2api，LGPL-3.0-or-later）

来源文件（@main，2026-11-02）：
- `backend/internal/server/routes/{auth,user}.go`
- `backend/internal/handler/{auth_handler,usage_handler,api_key_handler}.go`
- `backend/internal/handler/dto/types.go`
- `backend/internal/pkg/response/response.go`

### 通用
- 路由前缀 `/api/v1`；成功包络 `{code:0, message:"success", data:...}`；
  分页包络 `data:{items:[], total, page, page_size, pages}`；
  分页参数 `page` / `page_size`（`response.ParsePagination`）。
- 限流：登录 20/min、面板按用户限流、usage 重查询有更严的 Heavy 限流
  （adapter 默认 min_interval 节流 + 429 退避）。

### 认证
- `POST /api/v1/auth/login`，JSON `{email, password}`（可带 `turnstile_token` /
  `tencent_captcha_*`，站点开启人机校验时需提供；本工具支持通过配置传入
  turnstile token 的方式仅限环境变量注入，不做打码）。
  成功：`{access_token, refresh_token?, expires_in?, token_type, user:{...}}`。
- 2FA：`POST /api/v1/auth/login/2fa`（需验证码，本工具不支持自动过，报错）。
- 续期：`POST /api/v1/auth/refresh`（30/min）。
- 后续请求：`Authorization: Bearer <access_token>`（JWT）。

### 日志（usage）
- `GET /api/v1/usage?page=&page_size=&sort_by=created_at&sort_order=desc
  [&api_key_id=&group_id=&model=&start_date=YYYY-MM-DD&end_date=YYYY-MM-DD
  &request_type=&stream=]`（UsageLog 列表，`usage_handler.go:235-260`）。
- UsageLog 关键字段（`dto/types.go:597-678`）：`id, request_id, model, group_id,
  api_key_id, input_tokens, output_tokens, cache_read_tokens,
  cache_creation_tokens, total_cost, actual_cost, rate_multiplier,
  request_type, stream, duration_ms?, first_token_ms?, created_at`。
- 错误记录：`GET /api/v1/usage/errors`（单独端点，列表）；
  明细 `GET /api/v1/usage/errors/:id`。

### 分组倍率
- `GET /api/v1/groups/available` → `data:[{id, name, platform, rate_multiplier,
  status, ...}]`（`api_key_handler.go:322`）。
- `GET /api/v1/groups/rates` → 用户专属倍率，**`Record<group_id, rate_multiplier>`
  数字映射**（`api_key_handler.go:344` + `frontend/src/api/groups.ts:26`），
  覆盖分组默认 `rate_multiplier`（覆盖前值保留在记录 extra）。

### 性能指标
- 无站点级预聚合性能端点（用户侧）。首字/吞吐从 usage 日志**本地派生（derived）**：
  - `ttft_ms` = `first_token_ms`（原生字段，逐条）；
  - `output_tokens_per_second` = `output_tokens / ((duration_ms - first_token_ms)/1000)`
    （两项齐全时），否则 `output_tokens / (duration_ms/1000)`；公式与口径写入记录。
  - 成功率：usage 列表为成功记账记录，失败在 `/usage/errors` →
    `success_rate = usage_total / (usage_total + errors_total)`（近似口径，
    记录中标注 derived + 公式）。
- 聚合参考：`GET /api/v1/usage/dashboard/{stats,trend,models,snapshot-v2}`。

## 3. 模型价格契约（2026-11-03 增补）

### new-api：原生计价（native）
- 换算基准（源码 `common/constants.go:22`）：
  `QuotaPerUnit = 500*1000.0 // $0.002 / 1K tokens`
  → **1 倍率 = $0.002/1K = $2/1M tokens**。
- `GET /api/pricing` 模型项含 `model_ratio, completion_ratio, model_price,
  quota_type(0=按倍率,1=按次), cache_ratio, create_cache_ratio, enable_groups`
  （字段集见本仓库 `角色卡/.kd-reference/new-api-pricing/types.ts` 快照）。
- 计算公式（写入每条 ModelPrice 的 definition）：
  - 按倍率：`input_$M = model_ratio × 2 × 分组倍率`；
    `output_$M = model_ratio × completion_ratio × 2 × 分组倍率`；
    `cache_read_$M = model_ratio × cache_ratio × 2 × 分组倍率`；
    `cache_create_$M = model_ratio × create_cache_ratio × 2 × 分组倍率`。
  - 按次（quota_type=1）：`per_call_$ = model_price × 分组倍率`。
  - 分组倍率取站点 `group_ratio`；模型项自带 `group_ratio` 覆盖时优先。
- 官方预设同步：`GET /api/ratio_config`（公开、免登录，路由
  `router/api-router.go:62`）→ `{model_ratio, completion_ratio, model_price}`
  原样存入 artifact `preset_ratio_config`（站点当前生效的预设价目表）。

### sub2api：实付派生（derived）
- 用户面**无模型定价端点**（价格是管理员侧渠道配置，
  `frontend/src/api/` 用户 API 仅 groups/usage/keys 等）。
- 从 usage 日志派生实付单价：
  `input_$M = sum(input_cost)/sum(input_tokens)×1e6`（output 同理）。
  usage 逐条含 `input_cost, output_cost, total_cost`（`dto/types.go:626-630`），
  金额天然**已含分组 rate_multiplier 与用户专属倍率**。
- 价格取近 7 天窗口（避免旧价漂移），样本数写入 extra。

## 4. 授权与安全边界

- 用户面（self）接口默认启用；**管理员面接口仅当站点配置 `"admin": true`**
  （运营者自有的管理员账号），否则报 `ADMIN_REQUIRED`，不降级不试探。
- 渠道删除必须显式 `--yes`；渠道密钥类字段永不进入记录/日志/输出。
- 凭据不落盘明文：密码仅在登录时从 Windows 凭据管理器/环境变量读取；
  会话 token 存 keyring；日志与 artifact 均做字段白名单清洗。

## 5. 渠道管理契约（2026-11-03 增补，管理员面）

### new-api（源码：router/channel-router.go + controller/channel.go + model/channel.go）
- 路由组 `/api/channel`（AdminAuth；权限：读=ChannelRead，写=ChannelWrite，
  敏感写（创建/删除）=ChannelSensitiveWrite，操作=ChannelOperate）：
  - `GET  /api/channel/?p=&page_size=[&status=1|0]` → `data:{items:[Channel],
    total, page, page_size, type_counts}`（列表 Omit key）
  - `GET  /api/channel/:id`；`POST /api/channel/`（body
    `{mode:"single", channel:{...Channel}}`）；`PUT /api/channel/`（body 含 id）；
    `DELETE /api/channel/:id`
  - `POST /api/channel/:id/status`（body `{status}`：1=启用 2=手动禁用
    3=自动禁用；isManageableChannelStatus 仅接受 1/2）
  - `GET  /api/channel/test/:id?model=`（注意 test 在前，非 :id/test）
- Channel 结构关键字段：`id,type,status,name,models,group,priority,weight,
  response_time(毫秒),test_time,used_quota,balance(USD),base_url,tag,
  created_time,balance_updated_time`。

### sub2api（源码：routes/admin.go + handler/admin/channel_handler.go）
- 路由组 `/api/v1/admin/channels`：
  - `GET /`（分页）、`GET /:id`、`POST /`、`PUT /:id`（部分更新）、`DELETE /:id`
- 创建体：`{name(必填), description, group_ids, model_pricing:[{platform,
  models(1-100), billing_mode(token|per_request|image), input_price,
  output_price, cache_write_price, cache_read_price, per_request_price,
  intervals, time_pricing}], model_mapping, billing_model_source,
  restrict_models, features, features_config}`
- 状态：`"active"`（enabled）/ `"disabled"`（manual_disabled）。
- 模型清单在各 `model_pricing[].models`；无单渠道连通性测试端点
  （连通性验证走独立的 `/api/v1/admin/channel-monitors` 体系）→ Test 报
  UNSUPPORTED。

### 多Key（密钥聚合）渠道兼容
- new-api `Channel.ChannelInfo` 含 multi_key 形态（model/channel.go:64-72）。
  采集器只提取安全摘要进 Channel 记录：`multi_key / multi_key_size /
  multi_key_enabled（状态=1 的 key 计数）/ multi_key_mode`，
  key 明细与状态列表永不落盘。
- key 级管理走 `POST /api/channel/multi_key/manage`（action:
  disable_key/enable_key/delete_key/delete_disabled_keys/get_key_status），
  本采集器 v1 未封装（避免误操作密钥），需要时用站点管理后台。

### api_key（PAT/访问令牌）模式兼容
- new-api PAT 鉴权链部分版本强制 `New-Api-User` 头 → 首次使用 PAT 时调
  `/api/user/self` 解析 user_id 并缓存，后续请求附带（解析失败降级为不带，
  新版本可从令牌自解析）。
- 管理面（渠道 CRUD）在 PAT 模式下同样可用（AdminAuth 接受 PAT；
  细粒度权限由 authz scope 决定——PAT 需含 ChannelRead/Write/SensitiveWrite）。

### 客户端重试语义（agent 可编程依赖）
- POST 永不自动重试（非幂等，防重复建渠道）；GET/PUT/DELETE 对 429/5xx
  有限重试，退避封顶 30s；Retry-After 仅采纳 [1,120] 秒。
- 所有等待可被 Ctrl+C（ctx）打断。

### 日志 ↔ 上游渠道自动对齐（collector 本地逻辑，2026-11-04 增补）
- 目标：把每条请求日志归属到具体上游渠道（多Key渠道精确到 key 泳道）。
- 原生来源：new-api `/api/log/self` 每条自带 `channel`（渠道 id）；
  sub2api usage 每条自带 `account_id`（上游账号 id）。两者都以稳定 ID
  写入 `CallRecord.ChannelID`（非网址）。
- 配置规则：`sites[].log_alignment[] = {models[], channel_id, key_label?}`，
  models 支持精确与 `*` 通配（path.Match 语义）；**规则命中覆盖 native**
  （运营者显式指定优先），`key_label` 为自定义泳道标签。
- 安全：key_label 疑似真实密钥（sk- 前缀/超长）时配置校验直接拒绝；
  多Key渠道的 key 明细永不落盘。
- 渠道名回查：`admin: true` 时经 channels list 建 id→name 映射；
  查不到留空（诚实缺失）。

### 状态自动检查（collector 本地逻辑，非上游端点）
- 快照存 `var/state/channel-watch/<site>.json`（原子写）；
  通知追加 `var/logs/channel-watch/<site>.jsonl`。
- 变化类型：`status_changed` / `channel_added` / `channel_removed` /
  `models_changed` / `response_time_spike`（越线即报，回落再越线才再报）/
  `balance_below`（同越线语义）。
- 首次运行只建立基线（`baseline: true`，零变化）；
  `channels check` 有变化时退出码 2。
