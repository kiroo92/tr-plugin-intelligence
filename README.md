# TokenRouter 智商测试

独立插件，把 kiroo92 分支的智商测试拆出来。按分组定时检测站点模型，记录「满血 / 降智」结论。

只做**站点定时检测与手动检测**。没有用户自测，用户页只读。

## 范围

- 管理员按分组配置模型、测试类型（糖果 / 画图）、网关地址、提示词、协议、推理强度、服务档位和检测间隔。
- 宿主每分钟触发一次 tick，把到期的配置入队；插件进程内的两个工作者负责实际执行，因此宿主任务超时不会打断正在进行的模型请求。
- 结果分「执行状态」与「判定」两层：请求失败、超时、取消、额度不足都记为执行异常，**不会**显示成降智。
- 每个配置保留最近 60 次明确结论、60 条终态诊断和 10 份完整 HTML。

## 已知限制

这三条来自宿主 SDK 的能力边界，不是实现取舍：

1. **API Key 只能手填。** 插件拿不到站内 Key 列表接口，无法校验 Key 归属、额度和分组。填错只会得到上游 401/403。
2. **作品不能内嵌预览。** 插件 HTTP 响应不能返回 `text/html`（宿主会按 502 拒绝），宿主 CSP 也挡掉 `srcdoc` 里的内联脚本。页面提供源码复制与下载，请在隔离环境中打开。
3. **没有 PNG 截图。** 宿主的 Chromium 截图能力在宿主进程内，插件用不到。

## 兼容与安装

推荐添加自建插件来源，启用后刷新发现页并安装「智商测试」。

- 名称：智商测试 · kiroo92
- 插件 ID：`kiroo92.intelligence`
- 目标宿主：支持 DataDir 的 kiroo92 分发 TokenRouter v0.7.3 及以上

Go 依赖沿用现有插件的方式：require 公开 SDK `v0.2.1`，通过版本化 replace 使用
`github.com/kiroo92/TokenRouter-plugin-data/plugin-sdk/go v0.3.0`。Web SDK 固定 `0.2.0`。

## 配置

在宿主「插件配置」填写一个 secret 字段：

| 字段 | 说明 |
| --- | --- |
| `credential_key` | 凭据主密钥，至少 32 个字符。用于 AES-256-GCM 加密各配置的 API Key |

主密钥缺失时，保存 API Key 与运行检测都会被拒绝——插件不会退回到明文存储。更换主密钥前需停止任务并重填凭据；已有密文不会自动迁移。

## 检测流程

**糖果**：管理员入队 → 向 Manxue 发送真实网关地址、Key 和模型 → Manxue 反过来请求本站 → 轮询结论。
**画图**：管理员入队 → 插件用自己的 Key 请求网关生成 HTML → 保存作品 → 以 `html` benchmark 提交 Manxue 评价 → 轮询结论。

评价请求会整份重建成固定字段（`model` 固定 `gpt-6-astra`，地址与 Key 用占位值，不带 `protocol` / `reasoning_effort` / `service_tier`），生成用的 Key 不会离开插件。

**糖果测试会把真实地址与 Key 发给 Manxue。** 请使用专用、限额、可撤销的 Key。两种测试的模型调用都走该 Key 的普通计费规则，定时检测会持续消耗余额。

重试边界：生成失败最多 3 次（间隔 15s / 30s），评价失败最多 5 次并复用同一幂等键，整体观察期 30 分钟。次数在每次请求前持久化，重启不会重置。付费请求前先保存 `submitting`；重启后既无远端编号又无已保存作品的中断请求标为未知，需人工核对使用记录，不自动重放。

## 数据与备份

- 业务数据只在 `filepath.Join(env.DataDir, "plugin.sqlite")`，WAL 模式，运行中可能有 `-wal` / `-shm`。
- `Init` 要求 DataDir 为非空绝对路径且可写，不可用即启动失败，不回退到当前目录或 StateDir。
- StateDir 不保存业务数据。
- 备份请使用 SQLite 在线备份或停机备份，不要只复制主文件。

## 构建

```powershell
./build.ps1     # 测试、前端构建、五平台纯 Go 编译、SDK 包校验
```

或分步：

```bash
make test       # go test ./... 、go vet ./...
make frontend   # pnpm --dir web install && build
make build      # 五平台交叉编译到 dist/
make pack       # 打包并校验 release/*.tar.gz
```

`trplugin pack` 的归档输入位于临时目录，Windows 上若 `TMP` 与输出不在同一盘符会报
`archive output must be outside its input directory`；`build.ps1` 已经把 `TMP`/`TEMP`
指向 `dist/tmp` 规避该问题。

## 开发预览

```bash
pnpm --dir web dev
```

开发服务器用内存假数据渲染两种页面，可切换浅色 / 深色和页面视角，不连接宿主、不发真实请求。
