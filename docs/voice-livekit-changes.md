# 语音管控改造：客户端同步说明

本次服务端改造把语音相关的**管控动作真正下发到 LiveKit**，不再只改数据库后等客户端自觉。对客户端而言，**接口路径和请求体没有变化**，但「服务端下发的状态会立即在媒体层生效」这一点需要客户端正确响应。请按本文核对实现。

## 一句话总结

以前：管理员禁麦/封麦 = 服务端改一行数据库 + 推 SSE，靠客户端自己闭麦，且封禁在断线后失效。
现在：管理员动作 = 服务端直接调用 LiveKit API（踢人 / 改发布权限 / 服务端静音轨道），**即时生效、不可被对方绕过**；语音封禁持久化，断线重连依旧有效。

## 客户端必须做的事

### 1. 监听 LiveKit 房间事件，以 LiveKit 为准

真相在 LiveKit，不在你本地的乐观状态。请确保已订阅并正确处理以下 LiveKit SDK 事件：

- **`ParticipantPermissionsChanged`（本地参与者权限变化）**
  服务端 `block_voice` 会把你的 `canPublish` 改为 `false`，`restore_voice` 改回 `true`。收到此事件时：
  - `canPublish` 变 `false`：停止/禁用麦克风采集与发布，UI 上把麦克风按钮置为「被禁言」不可点状态。**不要**再尝试 publish 音轨（会被服务端拒绝）。
  - `canPublish` 变 `true`：恢复麦克风按钮可用（但保持当前静音状态，由用户主动开麦）。

- **本地麦克风轨道被静音（track muted，且非本端触发）**
  服务端 `mute_mic` / `block_voice` 会调用服务端静音。你会收到自己麦克风轨道被设为 muted 的回调。请同步 UI 为「已被管理员静音」，不要自动取消静音。

- **`Disconnected`（本地被断开）**
  服务端 `kick` 会调用 LiveKit `RemoveParticipant`，你会收到断开回调（断开原因为被移除）。请退出语音 UI，并**不要自动重连**该语音会话（除非用户重新主动加入）。

### 2. 麦克风/摄像头/屏幕共享：由客户端直接驱动 LiveKit 轨道

自助状态（开关麦克风、开关摄像头、开关屏幕共享、听筒）依旧是**客户端通过 LiveKit SDK 直接 publish / unpublish / mute 轨道**来实现的。`PATCH /rooms/:room_id/live/me` 只是把状态同步给服务端做投影和给别人展示，**它不代表媒体层真的开了麦**。两边都要做：

1. 调 LiveKit SDK 实际开/关轨道；
2. 调 `PATCH .../live/me` 上报状态。

### 3. 被语音封禁时，自助解除会被服务端拒绝

如果你被 `block_voice`，调用 `PATCH .../live/me` 传 `mic_muted: false` / `headphones_muted: false` 会被服务端**强制改回静音**（返回的 `participant` 里 `voice_blocked: true`、`mic_muted: true`）。客户端应据返回值刷新 UI，不要假设自己上报的值生效了。

## 接口与字段说明（无破坏性变更）

下列接口路径、请求体、响应结构**均保持不变**，只是行为语义增强：

| 接口 | 变化 |
| --- | --- |
| `POST /rooms/:room_id/live/join` | 不变。若该用户在本房间存在持久语音封禁，返回的 `participant` 会直接带 `voice_blocked:true / mic_muted:true`，且签发的 LiveKit token 不含发布权限。 |
| `PATCH /rooms/:room_id/live/me` | 不变。被封禁用户的取消静音请求会被强制覆盖（见上）。 |
| `POST /rooms/:room_id/live/participants/:user_id/moderation` | 请求体不变（`action` ∈ `kick / mute_mic / block_voice / restore_voice`，可选 `reason`）。现在每个 action 会真正调用 LiveKit。新增错误码见下。 |

### moderation 各 action 的服务端行为

- **`kick`**：LiveKit `RemoveParticipant`（真正踢出媒体会话）→ 删除参与者记录。被踢者收到 `Disconnected`。
- **`mute_mic`**：LiveKit 服务端静音其麦克风轨道 → DB 标记 `mic_muted`。被静音者收到本地音轨 muted 回调。
- **`block_voice`**：LiveKit 撤销 `canPublish` + 服务端静音麦克风 → 写入**持久封禁**（房间级，断线不失效）→ DB 投影 `voice_blocked/mic_muted/headphones_muted`。被封者收到 `ParticipantPermissionsChanged`。
- **`restore_voice`**：LiveKit 恢复 `canPublish` → 删除持久封禁 → 清除 DB 投影。被恢复者收到 `ParticipantPermissionsChanged`，麦克风按钮恢复可用（仍为静音，需用户主动开麦）。

### 新增错误码

moderation 接口在 LiveKit 调用失败时返回：

```
HTTP 502
{ "error": { "code": "livekit_error", "message": "..." } }
```

客户端应提示「操作失败，请重试」，**不要**假设动作已生效。

## 语音封禁语义变更（重点）

- 旧行为：封禁状态存在参与者会话行里，用户一断线（主动离开 / 网络掉线 / 进程被杀），该行被删除，**封禁随之消失**——被封的人重连即可正常说话。这是个漏洞。
- 新行为：封禁存于房间级持久表，**跨离开/重连一直有效**，直到管理员显式 `restore_voice`。被封用户重新 `join` 时会自动恢复为封禁态，且拿不到可发布的 token。

客户端无需改调用方式，但请确保上述「以 LiveKit 权限事件为准」的逻辑到位，否则被封用户重连后客户端可能错误地显示可开麦。

## 自测清单

- [ ] 被 `block_voice` 后，本地麦克风立即停发、按钮置灰；收到权限事件。
- [ ] 被 `block_voice` 后断网重连，仍为封禁态，无法开麦。
- [ ] `restore_voice` 后麦克风按钮恢复可用，但默认仍静音。
- [ ] 被 `kick` 后收到断开、退出语音 UI、不自动重连。
- [ ] `mute_mic` 后本地音轨被静音且不能自行恢复，直到管理员或重新加入逻辑允许。
- [ ] moderation 返回 502 `livekit_error` 时有重试提示。
