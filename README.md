# feishu-token-proxy

> 个人开源项目，非飞书（Feishu / Lark）官方出品，与飞书无隶属或背书关系。

在本机代理飞书 OpenAPI，自动获取、缓存、刷新 `tenant_access_token` 并注入到请求里。调用方把飞书域名换成本地地址即可，不用自己处理 token。

## Windows 部署

1. 在开发机执行 `./build.sh`，得到 `dist/feishu-token-proxy.exe` 和 `dist/config.example.json`
2. 把 `dist/` 里的两个文件拷到服务器同一个目录，例如 `C:\feishu-token-proxy\`
3. 把 `config.example.json` 改名为 `config.json`，填上 `app_id`、`app_secret`
4. 双击或在命令行运行 `feishu-token-proxy.exe`。启动时会先取一次 token，凭据有误会打印原因并退出
5. 验证：`curl http://127.0.0.1:18080/health`

默认读取 exe 同目录的 `config.json`，也可以用 `-config <路径>` 指定。

## 配置项

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `app_id` | 是 | | 飞书自建应用的 App ID |
| `app_secret` | 是 | | 飞书自建应用的 App Secret |
| `listen` | 否 | `127.0.0.1:18080` | 监听地址，只能是回环地址（`127.0.0.1`、`[::1]`、`localhost`），否则启动报错 |
| `upstream` | 否 | `https://open.feishu.cn` | 飞书地址，国际版 Lark 填 `https://open.larksuite.com` |
| `log_file` | 否 | 空 | 为空时只输出到控制台；非空时同时追加写入该文件，相对路径按配置文件所在目录解析 |

配置文件里出现未知字段（如拼错的字段名）会直接报错。

## 调用

### 透明转发

把 `https://open.feishu.cn` 换成 `http://127.0.0.1:18080`，不用带 `Authorization`：

```bash
curl -X POST 'http://127.0.0.1:18080/open-apis/im/v1/messages?receive_id_type=chat_id' \
  -H 'Content-Type: application/json; charset=utf-8' \
  -d '{"receive_id":"oc_xxx","msg_type":"text","content":"{\"text\":\"hello\"}"}'
```

```python
import json
import requests

BASE = "http://127.0.0.1:18080"
resp = requests.post(
    f"{BASE}/open-apis/im/v1/messages",
    params={"receive_id_type": "chat_id"},
    json={"receive_id": "oc_xxx", "msg_type": "text", "content": json.dumps({"text": "hello"})},
)
print(resp.json())
```

文件上传、下载同样直接转发。

### 只取 token

调用方需要自己拼请求时，可以只取 token：

```bash
curl http://127.0.0.1:18080/token
# {"code":0,"msg":"ok","tenant_access_token":"t-xxx","expire":5400}
```

返回格式与飞书获取 `tenant_access_token` 的接口一致，`expire` 是剩余有效秒数。已有解析飞书响应的代码可以直接复用。

### 健康检查

```bash
curl http://127.0.0.1:18080/health
# {"status":"ok","token_expires_in":5400,"last_refresh":"2026-11-16T10:00:00+08:00","last_error":""}
```

token 可用时返回 200，否则返回 503，`status` 为 `no_token`。这个接口只读状态，不会触发获取 token，也不返回 token 本身。

## 行为说明

- **提前刷新**：后台每分钟检查一次，token 剩余有效期不足 25 分钟时重新获取。后台获取失败只记日志，旧 token 在过期前继续使用
- **失效重放**：飞书返回 HTTP 非 2xx 的 JSON 响应且 `code` 为 `99991663` 或 `99991665`（token 无效）时，强制刷新 token 并把请求重放一次。距上次获取不足 60 秒时直接把飞书的原始响应返回给调用方，避免反复刷新（`99991663` 也可能是该接口不支持 tenant_access_token）
- **请求头**：调用方自带的 `Authorization` 会被替换成当前 token；响应以未压缩形式返回给调用方
- **只接受本机程序直接发起的请求**：`Host` 必须是 `127.0.0.1`、`localhost` 或 `[::1]`；带 `Origin` 头或跨站 `Sec-Fetch-Site` 头的浏览器请求返回 403。用来防止本机浏览器里的网页读取 token
- **请求体上限**：64MB，超过返回 413
- **代理自身的错误**：统一为 `{"code":-1,"msg":"feishu-token-proxy: <原因>"}`，例如拿不到 token 时返回 502。飞书返回的错误原样透传
- **日志**：每个请求一行，包含方法、路径（不含 query）、状态码、耗时、是否重放。日志里不会出现 `app_secret` 和 token

## 出口代理

服务器访问外网需要走代理时，启动前设置环境变量：

```bat
set HTTPS_PROXY=http://proxy.example.com:8080
feishu-token-proxy.exe
```

调用方所在环境如果设置了 `HTTP_PROXY`，记得把 `127.0.0.1` 加进 `NO_PROXY`，否则调用方会把发给本服务的请求也发给代理。

## 开机自启（可选）

需要常驻时用 [WinSW](https://github.com/winsw/winsw) 包装，程序本身不用改。把 `WinSW-x64.exe` 改名为 `feishu-token-proxy-service.exe`，和下面的 `feishu-token-proxy-service.xml` 一起放到 exe 同目录：

```xml
<service>
  <id>feishu-token-proxy</id>
  <name>feishu-token-proxy</name>
  <description>飞书 tenant_access_token 本地代理</description>
  <executable>%BASE%\feishu-token-proxy.exe</executable>
  <arguments>-config "%BASE%\config.json"</arguments>
  <log mode="roll-by-size" />
  <onfailure action="restart" delay="10 sec" />
</service>
```

然后以管理员身份执行 `feishu-token-proxy-service.exe install`、`feishu-token-proxy-service.exe start`。

## 安全提示

- `config.json` 含 `app_secret`，用 NTFS 权限限制只有运行服务的账号和管理员能读
- 服务只监听本机回环地址，没有调用方鉴权，本机任何进程都能通过它以应用身份调用飞书接口；浏览器发起的跨站请求会被拒绝

## 许可证

[MIT](LICENSE)
