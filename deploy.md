# 部署额外步骤

流水线部署二进制和配置文件之外，以下步骤需要手动执行一次：

## 1. 给 livekit-server 绑定 443 端口权限

```bash
sudo setcap cap_net_bind_service=+ep ./livekit-server
```

（非 root 绑特权端口 443 必须）

## 2. 生成 TLS 证书

**首次获取 Let's Encrypt 证书：**

```bash
sudo apt install -y certbot
sudo docker stop <前端容器名>          # 释放 80 端口
sudo certbot certonly --standalone -d loganz2.cn --key-type rsa --non-interactive --agree-tos --email admin@loganz2.cn
sudo docker start <前端容器名>

sudo cp /etc/letsencrypt/live/loganz2.cn/fullchain.pem /etc/livekit/cert.pem
sudo cp /etc/letsencrypt/live/loganz2.cn/privkey.pem /etc/livekit/key.pem
sudo chmod 644 /etc/livekit/cert.pem /etc/livekit/key.pem
```

## 3. 证书自动续期（cron）

```bash
sudo bash -c 'cat > /etc/cron.monthly/livekit-cert-renew << "EOF"
#!/bin/bash
set -e
docker stop ad91e13633fb
certbot renew --quiet --key-type rsa
cp /etc/letsencrypt/live/loganz2.cn/fullchain.pem /etc/livekit/cert.pem
cp /etc/letsencrypt/live/loganz2.cn/privkey.pem /etc/livekit/key.pem
chmod 644 /etc/livekit/cert.pem /etc/livekit/key.pem
docker start ad91e13633fb
systemctl restart livekit-server
EOF'
sudo chmod +x /etc/cron.monthly/livekit-cert-renew
```

## 4. STUN 服务器（coturn）

```bash
sudo apt install -y coturn

sudo bash -c 'cat > /etc/turnserver.conf << "EOF"
listening-port=3478
listening-ip=0.0.0.0
external-ip=<你的公网IP>
fingerprint
no-auth
no-tls
no-dtls
no-tcp
stun-only
syslog
EOF'

sudo systemctl enable coturn
sudo systemctl start coturn
```

> `external-ip` 填服务器公网 IP，`stun-only` 表示只提供 STUN 探测，不跑 TURN relay（relay 由 livekit 内嵌 TURN 处理）

## 5. 防火墙

外部防火墙开放：

| 端口 | 协议 | 用途 |
|---|---|---|
| 443 | TCP | 网页前端 + TURN over TLS |
| 7880 | TCP | LiveKit 信令 + 后端 API |
| 7881 | TCP | ICE TCP 备用 |
| 3478 | UDP | STUN 探测 |
| 50000-50100 | UDP | ICE 媒体直连 |
