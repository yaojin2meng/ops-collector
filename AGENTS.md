# 运营agent · 数据采集器 — 项目约束

## 用途
本仓库是**无 Web UI 的多站点运营数据采集器**（new-api / sub2api），
供运营 agent 通过 CLI 获取：调用日志、分组倍率、分组×模型成功率、
首字（TTFT）、吞吐（tk/s），用于运营成本与运营方式调整。

## 主实现：Go（`go/`）
- Go ≥1.22；依赖仅 `zalando/go-keyring` + `golang.org/x/term`，模块代理 goproxy.cn
- 构建与测试：
  ```powershell
  cd go
  $env:GOPROXY = "https://goproxy.cn,direct"
  go build -ldflags="-s -w" -o ops-collector.exe .
  go vet ./... ; go test ./...
  ```
- `src/`（Python 版）是参考实现，行为契约与 Go 版一致；两边改接口必须同步

## 硬性约束
1. **只采集/管理用户已配置且有权访问的站点**：
   - 用户面（self）接口：默认能力（日志、倍率、价格、性能）。
   - 管理员面接口（渠道增删/启停/测试）：**仅当站点配置 `"admin": true`** 才启用
     （运营者自有的管理员账号）；未配置时一律报 `ADMIN_REQUIRED`，不降级不试探。
   - 渠道删除必须显式 `--yes`（CONFIRM_REQUIRED 门控）；渠道密钥类字段
     （key/channel_info/setting(s)/param_override/header_override）永不进入
     记录、日志与输出（records.CleanChannelRaw + 黑名单双保险）。
2. **凭据永不落盘明文**：密码/令牌只进 Windows 凭据管理器（keyring）
   或环境变量；不写入配置、日志、artifact、命令行参数、URL
   （渠道 add/update 的请求体从 --body-file 读入）。
3. **stdout 只输出机器可解析 JSON（UTF-8）**；诊断一律走 stderr 或 var/logs/*.jsonl。
   （cmd 里看中文乱码是代码页问题，用 chcp 65001，不要改输出编码。）
4. **不做主动探测请求**（不发测试调用）；性能数据只来自站点已有日志/聚合接口。
   例外：管理员面的 `channels test` 会触发站点对指定渠道发起真实调用（显式消耗
   额度），仅在运营者明确指令下使用。
5. **字段白名单清洗**：提示词、响应正文、Cookie、令牌不得进入记录
   （Go: adapters/*_allowlist；Python: records.py *_RAW_ALLOWLIST）。
6. **指标必须带 provenance**（native/derived/unsupported/missing）与口径说明，
   禁止把不同口径的数据静默混合。
7. **上游契约以 docs/upstream-contracts.md 为准**；改动 adapter 前先核对源码，
   未知版本/形态的响应不得静默猜测。
8. 新增依赖前先评估必要性（Go 版目标：标准库 + 最小第三方）。

## 常用命令（Go）
```powershell
cd go
.\ops-collector.exe init
.\ops-collector.exe add-site --site-id X --kind new_api --base-url https://... --user 用户名
.\ops-collector.exe secrets set X:password
.\ops-collector.exe login-test
.\ops-collector.exe collect [--site X] [--no-incremental]
.\ops-collector.exe export var\runs\<run_id>
```

## 目录
- `go/` 主实现；`internal/adapters/` 按站点类型隔离
- `tests/` + 各包 `*_test.go` 全部离线（httptest mock），不访问真实站点
- `var/runs|logs|state` 运行产物，**不得提交**（`go/ops-collector.exe` 也不提交）
- `docs/upstream-contracts.md` 上游接口契约（锁定自源码）

## 网络环境注意（中国大陆）
- Go 模块拉取设 `GOPROXY=https://goproxy.cn,direct`（实测 173ms，最快）
- GitHub 直连不可达；拉上游源码用 jsdelivr CDN（见契约文档）
