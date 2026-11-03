# ops-collector — new-api / sub2api 多站点运营数据采集器（Go 版）

无 Web UI 的命令行采集器，供运营 agent / 定时任务调用。**当前主实现为 Go**，
编译为单个静态 exe；`src/`（Python 版）保留作参考实现，功能一致。

## 采集内容
| 数据 | new-api | sub2api |
|---|---|---|
| 调用日志 | `/api/log/self`（原生分页） | `/api/v1/usage`（含逐条首字 `first_token_ms`） |
| 分组倍率 | `/api/pricing`（group_ratio + 模型级覆盖） | `/api/v1/groups/available`（rate_multiplier）+ `/groups/rates`（用户专属倍率覆盖） |
| 成功率 | `/api/perf-metrics/*`（**原生**，站点口径） | usage + usage/errors（**近似派生**） |
| 首字 TTFT | `/api/perf-metrics?model=` 按分组（**原生** avg_ttft_ms） | 逐条 `first_token_ms` 均值（**派生**） |
| tk/s | `/api/perf-metrics`（**原生** avg_tps） | `output_tokens/((duration-first_token)/1000)`（**派生**） |
| 模型价格（含缓存价） | `/api/pricing` 倍率 × **$2/1M 基准** × 分组倍率（**原生计算**，含 cache_ratio/create_cache_ratio） | usage 实付成本派生 `sum(cost)/sum(tokens)×1M`（**已含倍率**，derived） |
| 官方预设价目表 | `/api/ratio_config`（公开端点，同步存档） | 无用户面端点（UNSUPPORTED） |

所有指标带 `provenance`（native/derived/unsupported/missing）与口径说明。

### 日志 ↔ 上游渠道自动对齐
对自己的站，可把模型 ID 映射到指定上游渠道（多Key渠道可加 `key_label`
泳道标签精确归属），采集时自动标注每条请求日志的上游归属：

```json
"log_alignment": [
  { "models": ["gpt-4o", "gpt-4o-*"], "channel_id": 1, "key_label": "supplierA-main" },
  { "models": ["claude-*"], "channel_id": 2 }
]
```

- 对齐来源：`rule`（配置映射，优先）> `native`（new-api 日志自带渠道 id /
  sub2api usage 的 account_id）
- `admin: true` 时自动回查渠道名（`channel_name`）；未开启时只给 ID
- `channel_id` 与 `channels list` 同键域（稳定 ID，非网址）；`key_label`
  是自定义标签，**真实密钥写进配置会被直接拒绝**
- 未对齐的日志如实留空，不猜测

## 构建与测试（Go ≥1.22，模块代理用 goproxy.cn）
```powershell
cd go
$env:GOPROXY = "https://goproxy.cn,direct"
go build -ldflags="-s -w" -o ops-collector.exe .
go vet ./... ; go test ./...     # 全部离线（httptest mock），不访问真实站点
```

## 快速开始
```powershell
cd go
.\ops-collector.exe init
.\ops-collector.exe add-site --site-id siteA --kind new_api `
    --base-url https://站点A --user 用户名
.\ops-collector.exe secrets set siteA:password
.\ops-collector.exe login-test
.\ops-collector.exe collect
.\ops-collector.exe export var\runs\<run_id>
.\ops-collector.exe prices var\runs\<run_id>   # 模型价格表（含缓存价 + 官方预设）
```
> cmd 控制台看中文乱码时先 `chcp 65001`；JSON 字节本身恒为 UTF-8（agent 消费不受影响）。

## 凭据规则
- 密码/令牌只存 Windows 凭据管理器（keyring 服务 `ops-collector`）或环境变量
  `OPS_COLLECTOR_SECRET_<条目大写>`；不落盘明文、不进命令行。
- 站点开启 Turnstile/2FA 时密码登录自动化被阻断 → 在站点"安全设置"创建
  访问令牌，`add-site --mode api_key` 后 `secrets set <site_id>:pat`。

## 渠道管理（管理员面，给模型用的接口）
站点配置加 `"admin": true`（运营者自有的管理员账号）后启用：

```powershell
.\ops-collector.exe channels list   --site X            # 渠道列表（脱敏，无 key）
.\ops-collector.exe channels get    --site X --id 1
.\ops-collector.exe channels add    --site X --body-file ch.json    # 请求体走文件，密钥不进命令行
.\ops-collector.exe channels update --site X --body-file ch.json
.\ops-collector.exe channels delete --site X --id 1 --yes            # 删除必须 --yes
.\ops-collector.exe channels set-status --site X --id 1 --status disabled
.\ops-collector.exe channels test   --site X --id 1 [--model M]      # sub2api 无此端点
```

### 渠道状态自动更新 + 变化提醒
```powershell
# 一次性：与上次快照 diff（首次建立基线）；有变化退出码 2
.\ops-collector.exe channels check --site X --on status,add_remove,rt,balance `
    --rt-above-ms 5000 --balance-below-usd 10

# 常驻：每 300s 自动刷新状态并提醒（duration-s=0 直到 Ctrl+C）
.\ops-collector.exe channels watch --site X --interval-s 300 --duration-s 0
```
- 提醒项：`status`（启停）、`add_remove`（增删）、`models`（模型变化）、
  `rt`（响应时间越线）、`balance`（余额低于阈值）
- 变化写入 `var/logs/channel-watch/<site>.jsonl`，stdout 输出 changes 数组，
  check 退出码 2 → 定时任务/运营模型据此感知。

## 供运营 agent 调用
- CLI：stdout 恒为 JSON（`run_summary` / artifact 路径 / 扁平指标表 / 渠道与变化）。
- artifact 契约见 `schemas/result.schema.json`；上游接口契约见
  `docs/upstream-contracts.md`（§5 渠道管理）。
- MCP：Python 版（`python -m ops_collector.mcp_server`，需 `pip install -e .[mcp]`）
  已可用；Go 版 MCP（rmcp / modelcontextprotocol-go-sdk）待接。

## 增量与断点
`var/state/<site_id>.json` 记录每个站点最后一次成功的日志时间戳；
下次采集从该位点增量拉取，重复运行幂等。

## 目录
- `go/` **Go 主实现**（`internal/` 按包隔离：config/secrets/httpc/auth/adapters/metrics/run/cli）
- `src/` Python 参考实现（与 Go 版行为一致）
- `tests/` Python 测试；Go 测试在各包 `*_test.go`
- `var/` 运行产物（真实数据，不得提交）

## 许可与来源
- 本仓库代码 MIT；上游契约依据：QuantumNous/new-api（AGPL-3.0，仅参考其
  公开接口形态，未复制代码）、Wei-Shaw/sub2api（LGPL-3.0，同上）。
