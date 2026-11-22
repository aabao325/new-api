# 流式响应透传 Msh 缓存写入 Token 用量头

上游在 SSE 响应头中返回：

- `Msh-Usage-Cache-Write-Tokens-1h`
- `Msh-Usage-Cache-Write-Tokens-5m`

流式代理如果只转发响应体，这两个头会丢失。按下面三步适配：

1. 收到上游响应后、向客户端写出第一个字节（`WriteHeader` / `flushHeaders`）之前读取这两个头。头名大小写不敏感。
2. 有值才写入客户端响应；同一头可能有多条值，逐条追加，不要覆盖。**值为 `0` 也必须透传**（只跳过空字符串）。
3. 只写这两个头，不要整包复制上游响应头，避免 `Content-Encoding`、`Set-Cookie`、hop-by-hop 头泄漏。

## Go（net/http）

```go
var usageHeaders = []string{
	"Msh-Usage-Cache-Write-Tokens-1h",
	"Msh-Usage-Cache-Write-Tokens-5m",
}

// 在 WriteHeader / Flush 之前调用
func copyUsageHeaders(dst, upstream http.Header) {
	for _, name := range usageHeaders {
		for _, value := range upstream.Values(name) {
			if value != "" {
				dst.Add(name, value)
			}
		}
	}
}
```

## Node（Express + fetch 代理）

```js
const USAGE_HEADERS = [
  'msh-usage-cache-write-tokens-1h',
  'msh-usage-cache-write-tokens-5m',
]

// 在 res.writeHead / res.flushHeaders 之前调用
for (const name of USAGE_HEADERS) {
  const value = upstream.headers.get(name)
  if (value) res.setHeader(name, value)
}
```

## 验证

```bash
curl -i -N https://your-gateway/v1/chat/completions | grep -i '^msh-usage'
```

流式响应必须能看到两行；值为 `0` 也要出现。
