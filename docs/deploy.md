# 部署指南

BoxPilot 的 Web 服务（`cmd/boxpilotd`）把只读扫描器包装成一个本地网页应用。
本文覆盖从本机跑到公网服务器的完整路径。

> **先把安全边界说清楚**：这个服务持有你的 Schwab 券商 API 凭证，**自身不做认证**。
> 代码是纯只读的（零下单，红线由 CI grep 与单元测试双重守护），但「谁能触发扫描」
> 仍然是暴露面 —— 任何能访问它的人都能消耗你的 Schwab 调用配额、看到你的扫描策略。
> 下面的部署方案默认「只监听回环地址 + 反向代理加认证」，请不要裸奔公网。

---

## 0. 先在本机跑通

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml：broker: schwab、scan.expiries、benchmark.rate 等

go build -o boxpilotd ./cmd/boxpilotd
./boxpilotd -config config.yaml
# → listening on http://127.0.0.1:8080
```

浏览器打开 <http://127.0.0.1:8080>。不接 Schwab 也能先看效果：

```bash
go run ./cmd/boxpilotd -config config.example.yaml \
  -fixture testdata/spx_chain_fixture.json
```

---

## 1. 凭证与 token 持久化

| 变量 | 作用 | 备注 |
| --- | --- | --- |
| `SCHWAB_CLIENT_ID` | App Key | 必需 |
| `SCHWAB_APP_SECRET` | App Secret | 必需 |
| `SCHWAB_REFRESH_TOKEN` | OAuth refresh token | 首次 OAuth 后有效一次即可 |
| `SCHWAB_TOKEN_STORE` | 轮换后 token 的落盘位置 | 默认 `~/.config/boxpilot/schwab_refresh_token`（0600） |

**关键机制**：Schwab 每次刷新都会轮换 refresh token 并使旧值失效。BoxPilot 会把轮换后的
新值写入 token 存储，启动时优先采用它，环境变量里的旧值仅作「存储值被吊销」时的兜底。

所以：**只要 token 存储文件还在，重启就不会掉线**，不需要重新 OAuth。
容器里务必挂一个 volume 到该路径（Dockerfile 已默认指向 `/data/schwab_refresh_token`）。

`.env` 文件权限建议 `chmod 600`，且已在 `.gitignore` 中。

---

## 2. Docker 部署（推荐）

### 2.1 构建镜像

```bash
docker build -t boxpilot .
```

### 2.2 准备配置与凭证

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml                 # 扫描范围、基准利率、阈值等

cp .env.example .env
$EDITOR .env                        # 填入三个凭证
chmod 600 .env
```

### 2.3 运行

```bash
docker run -d --name boxpilot \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  --env-file .env \
  -v boxpilot-data:/data \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  boxpilot
```

- `-p 127.0.0.1:8080:8080` —— **发布到回环地址**。容器内进程必须绑 `0.0.0.0`
  端口发布才生效，真正的边界在宿主机的发布绑定 + 反向代理。
- `-v boxpilot-data:/data` —— 持久化轮换后的 refresh token，**不要省**。
- `--env-file .env` —— 凭证注入，不落进镜像。

查看状态：

```bash
docker logs -f boxpilot
curl -s http://127.0.0.1:8080/healthz
```

### 2.4 docker compose

仓库根目录可放一份 `docker-compose.yml`：

```yaml
services:
  boxpilot:
    build: .
    image: boxpilot
    container_name: boxpilot
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    env_file:
      - .env
    volumes:
      - boxpilot-data:/data
      - ./config.yaml:/app/config.yaml:ro

volumes:
  boxpilot-data:
```

```bash
docker compose up -d && docker compose logs -f
```

---

## 3. 不用 Docker：systemd

```bash
sudo install -m 0755 boxpilotd /usr/local/bin/boxpilotd
sudo install -d -m 0700 -o boxpilot -g boxpilot /etc/boxpilot
sudo install -m 0600 config.yaml /etc/boxpilot/config.yaml
sudo install -m 0600 .env /etc/boxpilot/env
sudo useradd --system --home /var/lib/boxpilot --shell /usr/sbin/nologin boxpilot
```

`/etc/boxpilot/env`：

```
SCHWAB_CLIENT_ID=...
SCHWAB_APP_SECRET=...
SCHWAB_REFRESH_TOKEN=...
SCHWAB_TOKEN_STORE=/var/lib/boxpilot/schwab_refresh_token
```

`/etc/systemd/system/boxpilot.service`：

```ini
[Unit]
Description=BoxPilot — SPX box spread scanner (read-only)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=boxpilot
Group=boxpilot
EnvironmentFile=/etc/boxpilot/env
ExecStart=/usr/local/bin/boxpilotd -config /etc/boxpilot/config.yaml
Restart=always
RestartSec=5

# 加固
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/boxpilot
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=false

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now boxpilot
sudo systemctl status boxpilot
```

---

## 4. HTTPS 与认证（公网访问必须做）

服务本身没有认证层。用 **Caddy** 做反向代理 + Basic Auth + 自动 HTTPS：

`/etc/caddy/Caddyfile`：

```
box.example.com {
    # 只有你知道这个口令
    basicauth {
        admin $2a$14$...   # caddy hash-password 生成
    }

    # 本机直连，绕开公网
    reverse_proxy 127.0.0.1:8080

    # 别把服务端指纹泄露出去
    header {
        -Server
    }
}
```

```bash
caddy hash-password --plaintext '你的口令'
sudo systemctl reload caddy
```

**更安全的替代方案**（推荐顺序）：

1. **Tailscale / WireGuard** —— 只在私网可达，完全不暴露公网端口
2. **Cloudflare Tunnel** —— 不开入站端口，走 Cloudflare 的出站隧道
3. **SSH 隧道** —— 最简单，不需要任何额外软件：
   ```bash
   ssh -N -L 8080:127.0.0.1:8080 user@your-server
   # 本地浏览器打开 http://127.0.0.1:8080
   ```
4. **Caddy + Basic Auth** —— 唯一「真公网」的方案

无论哪种，都请再叠加：

- **限流** —— Caddy 可以配 `rate_limit`，或用 `fail2ban` 盯 429/403
- **不要记录 Authorization 头** —— 反代日志里别带凭证

---

## 5. WhatsApp 报警在服务器上的配对

WhatsApp 通道（`-tags whatsapp`）首次运行需要终端扫码。服务器上没有显示器时：

**方案 A（推荐）**：本地配对后把会话库拷过去

```bash
# 本地
go run -tags whatsapp ./cmd/boxpilot -config config.yaml -watch
# 手机扫码完成，会话保存在 whatsapp_session.db
```

```bash
# 传到服务器（这个文件等同凭证）
scp whatsapp_session.db user@your-server:/var/lib/boxpilot/
```

服务器上把 `watch.whatsapp.db_path` 指到该路径即可。

**方案 B**：进容器手工扫一次

```bash
docker exec -it boxpilot sh -c '...'
```

distroless 镜像没有 shell，这条路走不通 —— 改用方案 A，或改用 `alpine` 基础镜像。

> **合规提醒**：whatsmeow 走的是个人号多设备协议，属于违反 WhatsApp 服务条款的用法，
> 账号存在被限制/封禁的可能。请自行评估风险、保持低频率，并优先用备用号码。

---

## 6. 上线检查清单

- [ ] `SCHWAB_CLIENT_ID` / `SCHWAB_APP_SECRET` 来自环境变量，**没有**写进 config.yaml 或镜像
- [ ] `.env` 权限 0600，已在 `.gitignore` 中
- [ ] token 存储目录已挂 volume，重启后不掉线
- [ ] 监听地址是 `127.0.0.1`（或容器内 `0.0.0.0` + 宿主机只发布到回环）
- [ ] 公网访问已加认证（反代 Basic Auth / 私网 / 隧道）
- [ ] HTTPS 生效（TLS 证书已签发）
- [ ] 容器以非 root 运行（镜像默认 `nonroot:nonroot`）
- [ ] 反向代理日志不记录 Authorization 头
- [ ] `config.yaml` 在容器内是**只读挂载**（`:ro`）
- [ ] 磁盘监控：token 存储与会话库所在的 volume 有空间
- [ ] 确认零下单：`grep -ri "placeorder\|replaceorder\|cancelorder\|submit" --include="*.go" .` 无输出

---

## 7. 运维

**查看日志**

```bash
docker logs -f boxpilot
journalctl -u boxpilot -f
```

**修改配置**：Web 界面上的修改**立即生效但不落盘**，重启后丢失。
需要持久化的改动请直接编辑 `config.yaml`（容器内是只读挂载，需重建容器或改用非只读挂载 +
手动复制）。

**健康检查**：`GET /healthz` 返回 `{"status":"ok"}`，不触碰行情接口，可直接用作探针。

**数据陈旧**：行情超过 15 分钟未更新时，页面会显示红色「陈旧」并挂起机会报警。
持续陈旧通常意味着 Schwab 凭证过期或网络问题 —— 查日志里的 `round failed`。

---

## 8. 关于 Serverless

**不推荐**。watch 模式需要常驻进程轮询，Serverless 会在空闲时冻结实例，
轮询节奏和报警时效都会失真。如果必须用 Serverless，需要改造成按需触发（cron → API →
缓存），那是另一个架构，不在当前范围内。
