# Gemini Web2API Cookie Sync

浏览器扩展：定时把浏览器里 gemini.google.com 的登录 cookie 推到你自己部署的
gemini-web2api-go（`POST /admin/api/cookies/sync`），省掉手动重新导出。

浏览器开着时会自己轮转登录 cookie，扩展每隔几分钟读一次当前值推上去，另外
`__Secure-1PSIDTS` 等关键项一变化，30 秒内就会再推一次。

## 安装

- **Firefox**（推荐）：打开 `about:debugging#/runtime/this-firefox` →「临时载入附加组件」→ 选
  `extension/manifest.json`。临时载入的扩展重启浏览器后会消失；想长期使用，需要 Firefox
  Developer Edition / ESR 并关掉签名校验，或者自己签名。
- **Chrome / Edge**：打开 `chrome://extensions` → 打开「开发者模式」→「加载已解压的扩展程序」→ 选
  `extension` 目录。

## 设置

点扩展图标 →「设置」：

- **服务器地址**：例如 `https://your-server:8080`。保存时浏览器会请求访问这个地址的权限。
  不是 HTTPS 的地址会提示风险，cookie 会明文传输，只建议在本机或内网这样用。
- **管理 token**：服务端的 `ADMIN_TOKEN`，没设可以留空。
- **账号**：同一个浏览器登了多个 Google 号时，每个号填一行，序号就是浏览器里的登录顺序
  （`/u/0/`、`/u/1/`）。

服务端按 `SAPISID + __Secure-1PSID` 加序号找到对应账号更新；找不到就新建一条。
已经因为失效被自动停用的账号，同步到新 cookie 后会重新启用。

## 权限

| 权限 | 用途 |
|---|---|
| `cookies` + `https://*.google.com/*` | 读取 gemini.google.com 会带上的 cookie |
| `alarms` | 定时同步 |
| `storage` | 保存设置和上次同步结果（只存在本地） |
| 你填的服务器地址 | 推送 cookie。只在保存设置时按需申请这一个地址 |

扩展只连你填的服务器，不连其它任何地址。

## 注意

- 扩展读到的是完整的 Google 登录 cookie，服务器地址和 token 不要填错。
- 浏览器关着时不会同步。Firefox 导出的号服务端自己也能续命；Chrome 的号要靠浏览器开着、扩展持续推送。
