# 智商测试正式插件

- 独立仓库为 https://github.com/kiroo92/tr-plugin-intelligence，主分支 main。只在本目录提交，不包含工作区其它插件。
- 最新公开版本以 GitHub Releases 为准。每个版本包含五平台 tar.gz、plugin.json、checksums.txt、icon.svg、index.json 及独立 index.json.sig；签名的公钥 `/xRos0dV8zLsy9dHOxnpWeNXstJ988e/8v79AFqh8As=` 与来源 URL 写在 README。签名私钥在服务器用户配置目录 `/root/.config/TokenRouter/plugin-signing/tr-plugin-intelligence.key`，禁止进入项目或发行包，需由维护者备份。
- 功能只含站点定时检测与手动检测。**禁止恢复用户自测**：没有用户 Key 选择、没有 `source=user` 共享结果行、没有用户提交限流。用户页只读。用户已于 2026-11-15 明确取消该功能。
- 设计来源是 `kiroo92/TokenRouter` 的 `backend/internal/intelligence`（`service.go`、`drawing.go`、`provider/`、`remote/result.go`）与 `frontend/src/views/{admin/IntelligenceTestsView,user/IntelligenceView}.vue`、`components/intelligence/`。布局照原站：分组卡片 + 固定模型列 + 观测行 + 60 格历史条 + 图例。原型见 `../intelligence-prototype/index.html`，本插件已是正式实现。
- 判定口径不可改：`passed` → 满血，`incorrect` → 降智，画图 `normal` → 满血、`degraded`/`suspicious` → 降智。执行失败、取消、超时、额度不足与无法判定都不得显示为降智，也不进入历史条与通过率。
- 手填 Key 是既定前提。表单必须写明插件无法校验 Key 归属与额度；糖果测试额外提示真实地址与 Key 会发送给 Manxue。
- 凭据：管理员长期 Key 用 AES-256-GCM 加密存 `plugin.sqlite`，主密钥取宿主 secret `credential_key`，附加认证数据为记录自身的 `key_id`，防止跨记录密文替换。主密钥缺失或解密失败时拒绝执行，**不得回退明文**。Key、密文、主密钥、远端任务编号不得出现在日志、响应或前端。
- Manxue 协议：固定 `https://manxue.ai/api/v1/tests`，POST 创建、GET 轮询。画图评价正文必须整份重建为 `{benchmark:"html", base_url, api_key, model, html}`，`model` 固定 `gpt-6-astra`，不带 `protocol`/`reasoning_effort`/`service_tier`；html 创建接受任意 2xx。糖果创建才带真实地址、Key、模型且协议只能 `responses`。
- 重试与恢复：生成最多 3 次、评价最多 5 次并复用稳定幂等键、观察期 30 分钟；次数请求前持久化，重启不重置；`submitting` 无远端编号且无作品的中断请求标未知，不自动重放。重定向与请求体重放一律禁用。
- 网络：Base URL 仅公网 HTTPS，拒绝账号密码、查询、片段、本机与私网；保存与执行前各校验一次，执行前解析 DNS 并逐个检查地址。
- 数据：只在 `DataDir/plugin.sqlite`（WAL）。`Init` 校验目录可写，StateDir 不放业务数据。活动任务唯一由 `runs` 的部分唯一索引保证。保留 60 条明确结论、60 条终态诊断、10 份 HTML。
- 配置表单的分组必须是下拉，来自 `GET /admin/groups`，每项显示「名称 · 倍率×（#编号）」；倍率取宿主返回的当前账号实际值。模型标识在模型广场有公开条目时以按钮列出并保留手填，切换分组要清空不匹配的模型。
- 权限只需 `catalog.read`。身份取 `sdk.CallerFrom`，不信任正文里的 user/role；用户读结果前按 `Catalog.GroupsForUser` 复核分组授权。
- 重型命令串行并设置 `GOMAXPROCS=2`、`GOFLAGS=-p=1`、`GOMEMLIMIT=1GiB`。Windows 用 `./build.ps1`；`trplugin pack` 需要 `TMP`/`TEMP` 与输出同盘符，脚本已指向 `dist/tmp`。
- 发布照福利/社群插件：GitHub Releases 的 `index.json` 加独立 Ed25519 签名、公钥与固定版本下载地址写进 README，同版本资产不替换。私钥放用户配置目录，禁止进入项目或发行包。
- 自检：`make test`（或 `go test ./...`）+ `pnpm --dir web build`（含 `vue-tsc`）。视觉变更后同步 `preview-*.jpg`。
