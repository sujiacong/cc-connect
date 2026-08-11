# Lanxin Personal Bot Setup Guide / 蓝信个人机器人配置指南

> cc-connect platform: `lanxin`  
> Connection: **WebSocket** (client-initiated, no public IP needed / 客户端主动连接，无需公网 IP)

## Overview / 概述

Lanxin (蓝信) is an enterprise instant-messaging platform used by Chinese government and enterprise organizations. cc-connect supports Lanxin **personal bots** (个人机器人) via the Lanxin Open API gateway.

- **Private chat (P2P)**: ✅ — Real-time message push via WebSocket
- **Group chat**: Code-ready, pending platform-side WebSocket event push verification
- **Auth**: AppToken (GET with `appid` + `secret`, expires in 7200s, refreshed 5 min before expiry)

## Prerequisites / 前置条件

1. Lanxin desktop client (桌面端)
2. Access to the Lanxin developer console → Personal Bots / 智能机器人 → 个人机器人
3. API gateway URL (supplied by enterprise admin for private deployments / 企业私有部署需管理员提供网关地址)

## Credentials / 获取凭证

1. Open Lanxin desktop client → **通讯录** (left nav) → **智能机器人** → **个人机器人**
2. Create a new bot or click ℹ️ on an existing one
3. Copy **App ID** and **App Secret**

App ID format: `orgId-appId` (e.g. `13107200-4218880`)

## Configuration / 配置

```toml
[[projects.platforms]]
type = "lanxin"

[projects.platforms.options]
# Required / 必填
app_id = "${LANXIN_APP_ID}"
app_secret = "${LANXIN_APP_SECRET}"

# Optional / 可选
api_base = "https://apigw.lx.qianxin.com"    # Default: https://openapi.lanxin.cn (public cloud)
allow_from = "*"                               # Allowed staffIds, comma-separated; for personal bots, set to creator staffId
share_session_in_channel = false               # Share one agent session among all group users
```

> **Security**: Use `${ENV_VAR}` to inject credentials at runtime — never commit secrets to config files.  
> **安全**：使用 `${ENV_VAR}` 从环境变量注入凭证，勿提交到配置文件。

## Environment Variables / 环境变量

Set before running cc-connect:

```bash
export LANXIN_APP_ID="your-app-id"
export LANXIN_APP_SECRET="your-app-secret"
```

Or source from a `.env` file:

```bash
set -a; source .env; set +a
```

## API Gateway / API 网关

| Environment | URL |
|---|---|
| Lanxin Public Cloud (蓝信公有云) | `https://openapi.lanxin.cn` |
| Enterprise Private Deploy (企业私有部署) | Supplied by admin (e.g. `https://apigw.lx.qianxin.com`) |

## What Works / 已实现

| Feature | Status |
|---|---|
| Private text messages (私聊文本) | ✅ |
| Private Markdown/formatText | ✅ |
| WebSocket long-connection (WS 长连接) | ✅ |
| Heartbeat (30s ping / 10s pong / 70s dead) | ✅ |
| Auto-reconnect with backoff | ✅ |
| AppToken cache + auto-refresh | ✅ |
| Session persistence | ✅ |
| Group message send API (群消息发送) | ✅ |
| Group message receive (群消息接收) | Code-ready, pending WS push verification |

## Group Chat / 群聊

The group chat receive path is implemented and ready in code:
- WS event parsing for `bot_group_message`
- @mention filter (`isAtMe`)
- Session isolation (`lanxin:group:<groupId>:<from>`)
- Group reply routing via `sendGroupText()` → `POST /v1/messages/group/create`

Desktop client verification is pending — contact your Lanxin platform administrator to confirm WebSocket `bot_group_message` event push is enabled for personal bots.

## Build / 构建

```bash
# Build with lanxin
make build

# Build without lanxin
go build -tags 'no_lanxin' ./cmd/cc-connect
```

## Reference / 参考

- [Lanxin Personal Bot API Docs (attached to spec)](../.project/specs/lanxin-platform/assets/lanxin-personal-bot-api.md)
- cc-connect platform code: [platform/lanxin/](../platform/lanxin/)
