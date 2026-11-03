---
name: ops-collector
description: 采集运营站点（new-api / sub2api）的调用日志、分组倍率、各分组模型成功率、首字延迟（TTFT）与吞吐（tk/s），供运营 agent 做成本与运营调整。当需要获取这些站点的运营数据、执行采集、读取采集结果或配置新站点时使用本技能。
---

# ops-collector 使用指南（Go 版）

## 何时用
- 需要拉取"我的调用日志 / 分组倍率 / 分组×模型成功率 / 首字 tk/s"
- 运营 agent 定期采集并分析成本、质量趋势

## 前提
- 已构建：`cd go && go build -o ops-collector.exe .`（GOPROXY=goproxy.cn）
- 站点已在 `config/collector.json` 配置（未配置则先 `add-site`）
- 凭据已写入凭据管理器（`secrets set <site_id>:password`）

## 步骤

### 1. 确认站点与凭据
```powershell
cd go
.\ops-collector.exe validate-config
.\ops-collector.exe login-test            # 逐站点测试登录
```
- `LOGIN_BLOCKED`：站点开了 Turnstile/2FA → 让运营者在站点"安全设置"创建
  访问令牌，重新 `add-site --mode api_key`，`secrets set <site_id>:pat`。
- `AUTH_FAILED`：核对用户名/密码；`secrets set` 覆盖。
- `SECRET_NOT_FOUND`：凭据未写入。

### 2. 采集
```powershell
.\ops-collector.exe collect               # 全部启用站点，增量
.\ops-collector.exe collect --site siteA  # 指定站点
.\ops-collector.exe collect --no-incremental  # 忽略断点全量重跑
```
输出 run_summary JSON：`status`（success/partial/failed）、每站点 counts 与
artifact 路径。`partial` 属正常（个别站点失败不影响其他），失败原因看
`error_code`（AUTH_FAILED / TRANSIENT / HTTP_xxx / SECRET_NOT_FOUND ...）。

### 3. 读取结果
- artifact：`var/runs/<run_id>/site-<site_id>.json`
  （结构见 `schemas/result.schema.json`；含 `model_prices` 与
  new-api 的 `preset_ratio_config` 官方预设价目表）
- 扁平指标表：`.\ops-collector.exe export var\runs\<run_id>`
- 模型价格表：`.\ops-collector.exe prices var\runs\<run_id>`
  （列：input/output/cache_read/cache_create USD per 1M、per_call USD、
  group_ratio、provenance；new-api 基准 1 倍率=$2/1M）

### 4. 指标解读（关键）
- 每个指标有 `provenance`：
  - `native`：站点原生聚合，口径以站点为准（new-api 成功率"排除业务拒绝、
    含当前不完整小时"）
  - `derived`：本地按记录内公式推算（sub2api 的 TTFT/tps/成功率），
    `definition` 字段写明公式
  - `unsupported` / `missing`：上游没有该数据或本次未取到——**不要当作 0**
- 对比不同站点时注意口径差异：new-api 成功率是站点侧聚合口径，
  sub2api 是 usage/(usage+errors) 近似口径，不可直接混算。
- 价格口径：new-api 价格=倍率×2×分组倍率（native，含缓存价）；
  sub2api 价格=近7天实付 sum(cost)/sum(tokens)×1M（derived，天然含倍率）。
  跨站比价时 new-api 看 `input_per_m_usd`，sub2api 看同列即可，但注意
  sub2api 是"你实付的价"，new-api 是"目录价×你的分组倍率"。

## 约束
- 不主动发起模型调用探测；凭据不进命令行/日志/文件
- `var/` 下是真实运营数据，不得提交仓库
- stdout 恒为 UTF-8 JSON；cmd 人看中文先 `chcp 65001`

## 渠道管理（管理员面）
站点配置 `"admin": true`（运营者自有管理员账号）后可用，否则报 `ADMIN_REQUIRED`：

```powershell
.\ops-collector.exe channels list --site X          # 列表（无密钥字段）
.\ops-collector.exe channels add --site X --body-file ch.json   # 请求体走文件
.\ops-collector.exe channels delete --site X --id N --yes       # 必须显式 --yes
.\ops-collector.exe channels set-status --site X --id N --status enabled|disabled
.\ops-collector.exe channels check --site X         # 与上次快照 diff；有变化退出码 2
.\ops-collector.exe channels watch --site X --interval-s 300    # 周期自动更新+提醒
```
- 提醒项 `--on status,add_remove,models,rt,balance`（rt/balance 有阈值参数，
  越线即报、回落再越线才再报，不重复骚扰）
- 变化落 `var/logs/channel-watch/<site>.jsonl`；`check` 输出 changes 数组
  供模型直接消费。
- sub2api 无单渠道 test 端点（其渠道连通性走站点 channel-monitors 体系）。
