# 房间列表实时同步：客户端同步说明

本次服务端改造让**房间列表的状态变化通过 SSE 实时下发**，客户端不再需要轮询或手动刷新列表。接口路径和请求体没有变化，新增的是 `/api/v1/me/stream` 上的几个事件。请按本文核对客户端实现。

## 一句话总结

以前：房间被创建、成员加入/退出、被踢、改名、新消息……客户端只能靠下次拉 `GET /rooms` 才发现。申请加入被批准后，申请人完全是静默的，得自己重开才知道进了房。

现在：这些变化**都通过你已经连着的那条 SSE（`/api/v1/me/stream`）实时推过来**，客户端收到后直接覆盖本地列表项即可。

## 设计前提（一定要理解，否则会接错）

1. **按用户投递，不是按房间。** 这几个房间事件走的是「发给某个用户的所有在线连接」，**不依赖你的 SSE 是否订阅了这个房间**。这正是为什么「申请被批准」能推到申请人——他连接 SSE 时还不是成员，连接根本没订阅这个房间，但事件照样能到。客户端无需做任何「重连以更新订阅」的操作。

2. **全量快照，整体覆盖。** `room_added` / `room_updated` 带的是房间的**完整公共快照**，客户端拿到后**直接替换**本地对应列表项，不要做增量合并。

3. **快照不含个人字段。** 一条事件会发给房间里很多人，所以快照里**不包含**任何「因人而异」的字段：`my_role`、`unread_count`、`remark_name`（房间备注名）、`notification_level` 都不在里面。这些由客户端自己维护（见下文「客户端要自己维护的字段」）。

4. **内存态，断线自愈。** 连接信息只在服务端内存里，服务器重启会全部断联。客户端重连 SSE 时，服务端会重新按当前成员关系投递，漏掉的状态靠重连后拉一次 `GET /rooms` 补齐即可。

## 新增事件一览

| 事件名 | 接收者 | payload | 客户端动作 |
|--------|--------|---------|-----------|
| `room_added` | 刚获得成员资格的人（含其所有设备） | 完整房间快照 | 把这个房间**插入**本地列表 |
| `room_updated` | 房间当前所有在线成员 | 完整房间快照 | **替换**本地对应列表项 |
| `room_deleted` | 失去房间的人 / 房间被删时的全体成员 | `{"room_id": "..."}` | 从本地列表**移除**该房间 |
| `room_role_changed` | 被改了角色的那个人 | `{"room_id": "...", "role": "admin"}` | 更新本地该房间的 `my_role` |

事件名在 SSE 的 `event:` 行；`data:` 行是 JSON。沿用现有 `liveStream` 的解析方式即可。

### `room_added` / `room_updated` 的 payload 结构

```json
{
  "id": "room_xxx",
  "rid": "100023",
  "name": "房间名",
  "avatar_url": null,
  "default_avatar_key": "room_xxx",
  "visibility": "public",
  "join_policy": "open",
  "ai_voice_announce_enabled": true,
  "message_recall_policy": "time_limited",
  "message_recall_window_seconds": 120,
  "member_count": 5,
  "live_participant_count": 2,
  "live_avatar_preview": [ /* userSummary，最多 5 个，当前在麦的人 */ ],
  "last_message": {
    "id": "msg_xxx",
    "sender_display_name": "某人",
    "body_preview": "最后一条消息预览（最多 80 字）",
    "created_at": "..."
  },
  "created_at": "...",
  "updated_at": "..."
}
```

字段与 `GET /rooms` 返回的房间项**对齐**，但**少了** `my_role`、`notification_level`、`remark_name`、`unread_count`。`last_message` 在房间没有消息时为 `null`。

## 各场景会收到什么

- **创建房间**：创建者收到 `room_added`。
- **直接加入**（open 房 / 超管）：加入者收到 `room_added`；房间原有成员收到 `room_updated`（人数 +1）。
- **申请加入被批准**：申请人收到 `room_added`；原有成员收到 `room_updated`。被拒绝则**不会**收到任何事件。
- **主动退出 / 被移除**：离开者收到 `room_deleted`；其余成员收到 `room_updated`（人数 -1）。
- **房间被删除**：全体成员收到 `room_deleted`。
- **改名 / 改头像 / 改 join_policy / 改撤回策略等设置**：全体成员收到 `room_updated`。
- **角色被改（设为管理员 / 取消管理员）**：被改的人收到 `room_role_changed`。
- **有新消息 / 最后一条消息被撤回或强删**：全体成员收到 `room_updated`（`last_message` 已更新）。

> 注意：发消息也会触发 `room_updated`。活跃房间这个事件会比较频繁，客户端处理要轻量（直接覆盖列表项即可，不要每次都做重排或重渲染整个列表）。

## 客户端要自己维护的字段

因为快照不含个人字段，这几项需要客户端本地处理：

- **`unread_count`（未读数）**：服务端**不在事件里下发**未读数。客户端收到 `room_updated` 且其中带了**新的 `last_message`** 时，自行判断：如果这条消息不是自己发的、且该房间当前不在前台，就把本地未读 +1。真正的「已读」仍走 `POST /rooms/:room_id/read`，该接口返回会带准确的 `unread_count` 用于校准。
- **`my_role`**：靠 `room_role_changed` 更新；首次进入靠 `GET /rooms` 或 `GET /rooms/:id`。
- **`remark_name` / `notification_level`（房间备注名、通知等级）**：由客户端本地或 `GET /rooms/:room_id/me/settings` 维护，房间公共快照不会覆盖它们。

实现建议：客户端列表项 = 「服务端公共快照」 + 「本地个人字段」两层合并。收到 `room_added` / `room_updated` 时只覆盖公共层，保留个人层。

## 接入清单

1. 在已有的 `/api/v1/me/stream` 事件分发里，新增对 `room_added` / `room_updated` / `room_deleted` / `room_role_changed` 四个事件的处理。
2. `room_added` 插入、`room_updated` 替换、`room_deleted` 移除，均以 payload 的 `id`（或 `room_deleted` 的 `room_id`）为键。
3. 合并时只覆盖公共字段，保留 `unread_count` / `my_role` / `remark_name` / `notification_level` 等本地态。
4. 重连 SSE 后拉一次 `GET /rooms` 做一次全量校准（兜底服务端重启或断线期间漏掉的事件）。
5. 移除原来「定时轮询房间列表」的逻辑（如果有）。

## 未变化的部分

- 所有 HTTP 接口路径、请求体、响应体**均未改动**。
- live（语音）相关事件（`live_participant_joined` 等）行为不变。
- `ready` / 心跳（`: keep-alive`）行为不变。
