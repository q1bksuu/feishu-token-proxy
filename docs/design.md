# feishu-token-proxy 设计

## 意图

Windows 服务器上的本地进程要调飞书 OpenAPI，但它们自己处理不了 `tenant_access_token`（获取、缓存、刷新）。起一个本地常驻服务替它们做：调用方把飞书域名换成本地地址，其余照旧。

## 目标

- **透明转发**：`http://127.0.0.1:<port>/open-apis/...` → `https://open.feishu.cn/open-apis/...`，自动注入 `Authorization: Bearer <tat>`；method / path / query / body / 响应原样透传，含文件上传下载
- **`GET /token`**：返回当前有效 token，格式与飞书官方获取 token 接口一致（`{"code":0,"msg":"ok","tenant_access_token":"t-...","expire":<剩余秒数>}`），已有解析飞书响应的代码可直接复用
- **`GET /health`**：服务状态与 token 剩余有效期，不返回 token 本身
- **失效兜底**：飞书返回 token 无效时强制刷新并重放请求一次，调用方无感
- Windows 下直接运行的单文件 exe

## 非目标（现阶段）

- 开机自启 / Windows 服务：需要时用 WinSW 或 NSSM 包一层，程序本身不改
- 多个飞书应用、`user_access_token`、商店应用
- 调用方鉴权、局域网或外网访问
- 国际版 Lark：上游地址做成配置项，改配置即可切换，不专门适配

## 反目标

- 日志或错误信息里出现 `app_secret`、token 明文
- 调用方自带的 `Authorization` 混进转发请求
- token 异常时反复刷新，打爆飞书鉴权接口

## 约束

- Go 1.25，运行时只用标准库，测试使用 testify；交叉编译 `GOOS=windows GOARCH=amd64`
- 只允许监听回环地址（`127.0.0.1` / `::1` / `localhost`），配置成别的地址启动直接报错退出

## 方案

| 方案 | 取舍 | 结论 |
|---|---|---|
| A. Go exe 自带 Windows 服务注册 | 部署最干净，但多一段服务代码 | 现阶段不需要自启，不选 |
| B. Go exe 当普通控制台程序运行，要常驻时再用 WinSW/NSSM 包装 | 代码最少，以后加自启不改程序 | **选定** |
| Python + NSSM / NyxID | 要装运行时 / 为一个 token 搭整套平台 | 未纳入 |

## 设计

### 配置

exe 同目录 `config.json`，可用 `-config <路径>` 指定：

```json
{
  "app_id": "cli_xxx",
  "app_secret": "xxx",
  "listen": "127.0.0.1:18080",
  "upstream": "https://open.feishu.cn",
  "log_file": ""
}
```

`listen`、`upstream` 有默认值（如上）；`log_file` 为空时只输出到控制台，非空时同时写文件。

### token 管理

- 启动时先获取一次，失败则打印原因并退出，配置错误立刻暴露
- 缓存 token 和过期时刻；后台每分钟检查一次，剩余不足 25 分钟就重新获取（飞书只在剩余不足 30 分钟时才发新 token，否则返回原值；旧 token 在过期前仍然可用）
- 后台刷新失败只记日志，旧 token 未过期继续用；请求到达时 token 已过期则同步获取，失败返回 502 + JSON 错误说明
- 并发安全：同一时刻只有一个获取请求在飞行，其他请求等待复用结果
- 强制刷新：转发收到 `99991663` 或 `99991665` 时触发；距上次获取不足 60 秒则跳过刷新，直接把飞书的原始响应返回（`99991663` 也可能是接口本身不支持 tenant_access_token，此时刷新无用）

### 转发

- 只处理 `/open-apis/` 前缀，其他路径返回 404
- 请求体整体读入内存以便重放，超过上限返回 413
- 去掉调用方的 `Authorization` 和 hop-by-hop 头，注入当前 token
- 响应判断：HTTP 非 2xx 且 `Content-Type` 为 JSON 时读出 body 检查 `code`（已实测：token 无效时飞书返回 HTTP 400 + JSON `code:99991663`），命中则按上面规则刷新并重放一次；其余响应流式透传，大文件下载不进内存
- 每个请求一行日志：时间、方法、路径、状态码、耗时、是否重放

### 交付物

- `build.sh`：交叉编译出 `dist/feishu-token-proxy.exe`
- `config.example.json`、`README.md`（部署与调用示例）
- 单元测试：用 `httptest` 模拟飞书，覆盖 token 缓存与提前刷新、并发只获取一次、Authorization 剥离与注入、失效重放、强制刷新冷却、大响应流式透传

## 留给实现者

- 请求体大小上限（飞书单次上传接口一般不超过 30MB，取一个留余量的值）
- 上游超时设置（不能卡住大文件下载）
- 日志具体格式
