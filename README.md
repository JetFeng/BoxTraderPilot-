# BoxPilot

面向个人的 **SPX Box Spread 融资利率只读扫描器**（Phase 1：Scanner）。

批量拉取 SPX 期权链，枚举 到期日 × 行权价 组合，计算每个 short box 的隐含借款年化利率（365 天口径），
与基准融资利率对比，按借款成本从低到高排序输出。

> **个人自用只读工具，不构成投资建议。**
> 本阶段为零下单的纯只读工具：代码库中不存在任何下单/改单/撤单相关的类型、函数或接口方法，
> 并由 CI grep + 单元测试双重守护（见「安全红线」）。
> Phase 2 才会考虑下单流程，且必须重新评审安全红线。

## 领域口径

Short box（K1 < K2，同一到期日）：卖 K1 Call、买 K2 Call、买 K1 Put、卖 K2 Put。
到期 payoff 恒为 W = K2 − K1；开仓净收入权利金 C，实际借款本金 = C，利息 = W − C。

```
r = (W − C) / C × 365 / D      D = 距到期自然日（calendar days），年化一律 365
```

对每个组合输出三种口径：

| 口径 | 计算 | 含义 |
| --- | --- | --- |
| `C_natural` / `r_natural` | 卖腿取 bid、买腿取 ask | 保守口径：立刻按对手价成交的利率（排序键，升序） |
| `C_mid` / `r_mid` | 四腿取 mid | 理论中枢利率 |
| `spread_bp` | `r_natural − benchmark`（基点） | 相对基准利差，负数 = Box 更便宜 |

**单位约定**：所有价格内部用整数表示（1 = 0.0001 点，4 位小数精度），禁止 float64 直接算钱；
利率用精确有理数比较（`r_natural ≥ r_mid` 恒成立，有单测守护），仅在输出时转 float。
显示层面金额单位为「点」，美元 = 点 × 100（SPX 乘数）。

## 快速开始

```bash
# 1. 离线验收（零网络请求，使用 testdata 中的假数据 fixture）
go run ./cmd/boxpilot -config config.example.yaml -fixture testdata/spx_chain_fixture.json

# 2. 运行测试（含红线守护、端到端 fixture 扫描）
go test ./...

# 3. 重新生成假数据 fixture（确定性输出）
go run ./cmd/fixturegen
```

### 接入 Schwab 实时行情

1. 在 <https://developer.schwab.com/> 注册应用，取得 App Key / App Secret，并完成一次 OAuth 授权拿到 Refresh Token。
2. 从环境变量提供凭证（**禁止写入任何文件**，提供 `.env.example` 占位）：

```bash
export SCHWAB_CLIENT_ID=...     # 你的 App Key
export SCHWAB_APP_SECRET=...    # 你的 App Secret
export SCHWAB_REFRESH_TOKEN=... # OAuth refresh token
```

3. `cp config.example.yaml config.yaml`，按需修改扫描范围后：

```bash
go run ./cmd/boxpilot -config config.yaml
```

Schwab 适配器实现于 `internal/broker/schwab/`，仅含两个只读市场数据端点（option chain 按到期日整链批量拉取、
underlying quote）与 OAuth token 刷新；429/5xx 只读重试（指数退避，最多 3 次，尊重 `Retry-After`）。
端点与字段名以官方文档为准：<https://developer.schwab.com/>（Market Data API）。

### Refresh token 轮换与持久化

Schwab **每次刷新都会轮换 refresh token 并使旧值失效**。因此 BoxPilot 会把轮换后的新值写入本地 token 存储
（默认 `~/.config/boxpilot/schwab_refresh_token`，权限 0600；可用 `SCHWAB_TOKEN_STORE` 覆盖路径），
启动时优先采用存储中的值，环境变量里的旧值仅作为「存储值被吊销」时的兜底。

这样 `SCHWAB_REFRESH_TOKEN` 只需在首次 OAuth 后有效一次；只要存储文件在，重启不会掉线。
若存储文件被删除或存储值被吊销，重新 OAuth 并更新环境变量即可（存储值优先级更高，需要先删掉存储文件）。
存储文件等同凭证，禁止提交（已在 `.gitignore` 中）。

## 配置

见 `config.example.yaml`：扫描范围（到期日 / 间距 / K1 区间 / 步长 / 最小 DTE）、基准利率、
过滤规则（单腿价差、最小卖腿 bid、缺报价处理）、输出（table|csv|json、CSV 导出路径、top N）。
启动时校验：width 为 strike_step 整数倍、日期合法且不在过去、benchmark ∈ (0, 50%] 等，不合法直接报错退出。

## Watch 模式（轮询报警）

在单次快照扫描外加了一层轮询循环：按 `watch.interval` 重跑扫描，对**全部有效组合**（不限 top N）
应用触发规则——`spread_bp ≤ threshold_bp` 视为机会，连续 `confirm_rounds` 轮确认后首次报警；
同一机会需再改善 `hysteresis_bp` 才会重复报警；任意两次报警间隔不小于 `cooldown`。
行情数据变陈旧（>15 分钟）时暂停机会报警并推送一次数据陈旧通知，数据恢复后自动继续。

`threshold_bp` / `confirm_rounds` / `hysteresis_bp` 三项**显式写 0 会被原样采用**（不写则取默认
`-50` / `1` / `25`）——因为 0 对三者都是有效取值：

| 配置 | 含义 |
| --- | --- |
| `threshold_bp: 0` | 只要 `spread_bp ≤ 0`（不比基准贵）即视为机会 |
| `confirm_rounds: 0` | 不做确认轮次，首轮满足条件立即报警 |
| `hysteresis_bp: 0` | 同一机会只要有任何改善就重报 |

```bash
go run ./cmd/boxpilot -config config.yaml -watch
```

报警通道（`watch.channels` 可多选）：

- `log`：写 stderr，零依赖（默认）
- `whatsapp`：通过 whatsmeow 用**个人 WhatsApp 账号**发消息。需要带 tag 构建：

  ```bash
  go run -tags whatsapp ./cmd/boxpilot -config config.yaml -watch
  ```

  首次启动会打印二维码，用手机 WhatsApp（已链接的设备）扫码配对；会话密钥保存在
  `watch.whatsapp.db_path`，**等同凭证、禁止提交**（已加入 .gitignore）。
  接收人默认是配对账号自己，也可用 `watch.whatsapp.recipient` 或环境变量 `WHATSAPP_RECIPIENT`
  指定其他号码（E.164）。

  > **风险提示**：whatsmeow 走的是个人号多设备协议，属于违反 WhatsApp 服务条款的用法，
  > 账号存在被限制/封禁的可能。请自行评估风险、保持低频率，并优先考虑用备用号码承担此用途。


## 输出

**stdout 只承载所选格式的数据，诊断信息一律走 stderr。** 这样 `csv` / `json` 输出可以被下游程序直接消费：

- `format: csv` → stdout 是纯 CSV（表头 + 数据行），摘要在 stderr
- `format: json` → stdout 是可直接 `json.Unmarshal` 的数组，摘要在 stderr
- `format: table` → stdout 是面向人类的表格 + 摘要（表格不是机器可读格式，摘要留在 stdout）

表格列：`expiry, D, K1, K2, width, C_mid, C_natural, r_mid%, r_natural%, spread_bp, legs_ok`
（终端下负数 spread 绿色高亮）。运行结束打印统计摘要：扫描/有效/各原因剔除数、
最优组合完整四腿报价明细、行情时间戳；本地时间与行情时间差超过 15 分钟时打印显著警告。

```bash
# 例：只取 JSON 数据，摘要仍在终端可见
go run ./cmd/boxpilot -config config.yaml -fixture testdata/spx_chain_fixture.json \
  | jq '.[0]'
```

## 项目结构

```
cmd/boxpilot/            # CLI 入口（快照扫描 / -watch 轮询报警）
cmd/fixturegen/          # 生成确定性假数据 fixture 的开发工具
internal/box/            # 领域计算：整数价格、四腿定价、365 口径精确利率
internal/scan/           # 组合枚举、过滤、排序、统计
internal/watch/          # 轮询引擎：触发规则（阈值/确认/迟滞/冷却）、陈旧数据暂停
internal/alert/          # 报警通道：Alerter 接口、log、WhatsApp（whatsmeow，-tags whatsapp）
internal/config/         # YAML 配置加载与校验
internal/broker/         # 只读 Broker 接口
internal/broker/fixture/ # 离线 fixture 适配器（零网络）
internal/broker/schwab/  # Schwab 只读适配器（全部网络调用集中于此）
internal/report/         # table/csv/json 输出与摘要
internal/logx/           # 脱敏日志
internal/redline/        # 红线守护测试
testdata/                # 假期权链 JSON fixture（非真实行情）
```

## 安全红线（Phase 1，后续需求不得覆盖）

1. **零下单代码**：Broker 接口只有 `GetOptionChain` / `GetQuote` 两个只读方法；
   仓库 grep 不得出现交易类标识符，由 `internal/redline` 单测 + CI grep 守护：

   ```bash
   grep -ri "placeorder\|replaceorder\|cancelorder\|submit" --include="*.go" .
   ```

2. **凭证零硬编码**：Client ID / App Secret / Refresh Token 一律从环境变量读取；
   `.env` 已加入 .gitignore（参照 `.env.example`）。轮换后的 refresh token 只落在本地
   token 存储文件（0600，等同凭证，已 gitignore），不写入代码、不打印到日志。
3. **日志脱敏**：完整 Token/Secret 一律遮蔽（`internal/logx`，有单测构造假 Token 断言被遮蔽）。
4. **只读重试**：行情请求失败指数退避最多 3 次；429 按 `Retry-After` 退避；仅限只读接口。
5. **数据时效标注**：所有输出带行情时间戳；时差 > 15 分钟打印显著警告。

## 测试

- 利率公式手算用例（含 365 口径、C→W、C→0 边界）与 `r_natural ≥ r_mid` 不变量（万组随机报价）
- 组合枚举（无重复、宽度一致、K1 区间、闰年 DTE）
- 每条过滤规则正反用例；排序严格升序（精确有理数比较）
- 脱敏、红线守护、Schwab 适配器（token 缓存/刷新、429/5xx 重试、字段解析、refresh token 轮换落盘与重启自愈）
- watch 引擎（确认轮数、迟滞重报、episode 重置、全局冷却、陈旧数据暂停、状态 GC）
- 端到端：testdata 假 fixture 跑完整 CLI，断言表格/CSV/JSON 排序与数值、JSON/CSV 输出可被真正解析、时效警告、watch 配置校验

注意：默认构建（无 tag）不含 whatsmeow；`go build -tags whatsapp ./...` 与
`go vet -tags whatsapp ./...` 应在 CI 中各跑一次，保证两种构建都可编译。
`go test -tags whatsapp ./...` 也可安全运行：whatsapp 相关用例按 build tag 分文件守卫
（`!whatsapp` 断言「需要用 `-tags whatsapp` 重新构建」，`whatsapp` 断言通道真实存在），
不会有任何用例去调用 `Connect()`（它会阻塞等待终端扫码）。

## Phase 2 预留（未实现）

最优组合一键下单（让价循环与状态机）、融资台账与 roll 提醒、历史结果入库、多基准自动对比
（SOFR / 3M 国库券 / SGV）。**下单功能引入前必须重新评审上述安全红线。**
