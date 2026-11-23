# 内网 DNS 管理

提供主控 Web 界面和单区域节点发布程序。CoreDNS 从本地磁盘读区域文件，节点程序通过 HTTPS
接收新版区域文件并查询本机 CoreDNS 确认生效。

## 默认部署方式

使用 **Docker Compose**。主控服务器运行管理界面和发布 API，可同机部署私有镜像仓库。每台 DNS 虚拟机运行 CoreDNS 和 Agent 容器。使用 VIP 时，Keepalived 跑在 DNS 虚拟机的宿主系统上。

节点首次安装拉取固定版本的镜像。重启时使用本地镜像，CoreDNS 从挂载的磁盘目录读取上次成功发布的记录。主控或镜像仓库离线时，节点仍可恢复本地解析。

按 [Docker 部署说明](deploy/docker/README.md) 构建镜像、生成节点配置包并安装。

## 备选部署方式：原生二进制和 systemd

### 构建和运行

在本目录用 Go 1.25 或更高版本执行：

```sh
go build -o dns-node ./node
```

先准备有效的区域文件并启动 CoreDNS。试点可在独立工作目录中使用
[示例 Corefile](examples/Corefile.example) 和 [示例区域文件](examples/zone.db)。把示例配置复制为工作目录下的
`Corefile`，再从该目录执行 `coredns -conf Corefile`。
示例只监听本机的 1053 端口，正式部署时调整监听地址，并保留一个可供节点程序
查询的回环地址。节点程序和 CoreDNS 应以同一服务用户运行，区域文件目录须可写。
CoreDNS 的 file 插件须启用短间隔扫描和 `reload_by_mtime`，以便恢复旧文件。

准备与节点地址匹配的 TLS 证书和私钥，给每个节点设置独立的随机
`DNS_NODE_TOKEN`（至少 32 字节），再启动节点程序：

```sh
./dns-node -zone infra.example.com -file /absolute/path/zone.db \
  -dns 127.0.0.1:1053 -listen 127.0.0.1:8443 \
  -tls-cert /absolute/path/node.crt -tls-key /absolute/path/node.key
```

远程部署时把 `-listen` 设为节点的管理网地址，主控应验证节点证书。
`PUT /v1/zone` 接收完整区域文件（最多 1 MiB），请求头为
`Authorization: Bearer <token>`。每次发布须递增 SOA 序列号。
成功响应返回 CoreDNS 当前应答的序列号，完全相同的发布可以重试。
`GET /v1/zone` 返回本机 CoreDNS 当前应答的序列号。
需要主动回滚时，把历史记录内容配上新的 SOA 序列号后重新发布。

### 主控和中文管理台

用 `go build -o dns-controller ./controller` 构建主控。网页源文件在
`controller/index.html`，修改后重新构建即可。

参考 [主控配置](examples/controller.json) 填写区域及节点。首次接入时，
所有节点须已有匹配的区域文件，`InitialSerial` 和 `InitialRecords`
填入现有发布版本及记录。每个区域使用一个节点 API 实例，同一 VM 上的
多个实例用不同管理端口和区域文件。前向和反向区域分别配置。

主控服务使用 `DNS_ADMIN_PASSWORD`（至少 16 字节）及各节点的 token 环境变量，
网页用户名为 `admin`。启动参数参考 [主控服务文件](deploy/dns-controller.service)
与 [节点服务文件](deploy/dns-node@.service)。双向 TLS 部署时，主控填入节点 CA
及自己的客户端证书和私钥，节点填入签发主控客户端证书的 CA。节点服务器证书须
包含主控用于连接的主机名或 IP。将管理服务限制在管理网。

安装前准备服务账号、环境文件、证书和区域文件目录。节点模板以 `coredns`
用户运行，并使用 `/var/lib/coredns/zones` 作为可写区域目录。
CoreDNS 自己作为独立的 systemd 服务运行，并从该目录加载文件。
主控将历史保存在 `/var/lib/dns-controller/releases.json`，应备份此文件。

同机房节点按配置顺序逐台发布。节点失败后主控保留原因并定期重试最新版本。
回滚使用历史记录生成新版本。待发布节点不会影响其他机房继续发布。
机房 VIP 或 LB 需使用现有设施接入本地两台 DNS，监听 UDP/TCP 53。

### 原生部署文件

- 主控使用 `deploy/controller.env.example` 作为
  `/etc/dns-manager/controller.env` 的模板。
- DNS 节点使用 `deploy/node.env.example` 作为
  `/etc/dns-manager/node-infra.env` 的模板，`infra` 对应服务实例名。
- 使用 `deploy/coredns.service`，将 `deploy/Corefile.example` 安装为 `/etc/dns-manager/Corefile`。
  将初始区域文件放在 `/var/lib/coredns/zones/infra.example.com.db`。
  正式部署时按机房网络调整 CoreDNS 监听及访问范围。
- 把二进制安装到 `/usr/local/bin`，对应 unit 安装到
  `/etc/systemd/system`。在主控启用 `dns-controller.service`，
  在节点启用 `coredns.service` 和 `dns-node@infra.service`。
- 机房上线验收时，分别查询两个节点与 VIP。停掉一个节点后从 VIP 查询，
  再隔离到主控的链路、重启两个节点，确认 VIP 上的 UDP 和 TCP 查询都可用。

已有高可用 LB 时，将两个同机房节点注册为后端。使用 VIP 时，可参考
`deploy/keepalived.conf.example`，同一机房两台节点配置相同的 VIP，
并填写各自的网卡和对端地址。在节点上安装 `dig`，将
`deploy/check-coredns` 安装到 `/usr/local/libexec/check-coredns`。
健康检查通过实际 UDP/TCP 查询确认本地 CoreDNS 能返回权威 SOA。
Keepalived 部署需要机房网络支持 VRRP 和浮动 IP。

### 本地集成测试

用两个本地 CoreDNS 实例验证下发、失败重试、回滚和离线重启。
测试需要 Python 3、OpenSSL 和 dig。

```sh
go build -o /tmp/dns-controller ./controller
go build -o /tmp/dns-node ./node
python3 tests/rollout.py --coredns ../coredns --node /tmp/dns-node --controller /tmp/dns-controller
```

### VIP 故障切换测试

在安装了 Docker 的 Linux 测试机上运行
`dns-manager/tests/vip-failover ./coredns`（从仓库根目录执行）。
脚本在独立网桥部署两台 CoreDNS、两个 Keepalived 实例和一个客户端。
它先验证 VIP 指向优先节点，再停止该节点的 CoreDNS 进程，检查备用节点
接管后仍能通过 UDP/TCP 解析。最后恢复优先节点，并重启隔离网桥中的
两台节点，确认 VIP 仍能用磁盘上的记录应答。
测试需要 Docker 支持为测试容器授予 `NET_ADMIN`，并使用
`VIP_TEST_SUBNET`（默认 `172.31.247`）避开宿主机现有网段。

试点部署时，每个节点管理 IP、对端 IP 与 VIP 要填实际地址，
在两台宿主机上用 root 安装健康检查脚本，使其归 root 所有、权限为 0755。
上述测试验证 Linux 网桥中的 VRRP 行为。实际机房的浮动 IP、交换机或云网络策略，
以及整机断网重启，需要在试点环境按上线验收步骤检查。
