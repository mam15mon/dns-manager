# Docker 部署

主控同机可独立运行 Distribution 镜像仓库。发布 DNS 记录仍走 Agent HTTPS API。
节点容器通过宿主机网络对外提供 UDP/TCP 53，VIP 继续由宿主机管理。
安装前确认宿主机的 53 端口可用。

## 私有镜像仓库和构建

在主控服务器创建 `registry-data`、`registry-tls` 和 `registry-auth`。
准备 `registry-tls/registry.crt`、`registry-tls/registry.key` 和使用 bcrypt 的
`registry-auth/htpasswd`。若用 IP 访问仓库，证书的 SAN 必须包含该 IP。
将内部 CA 安装到节点 Docker 的 `/etc/docker/certs.d/IP:端口/ca.crt`，
并先登录仓库。仓库访问限制在管理网。

设置 `REGISTRY_LISTEN`（例如 `10.10.0.10:5000`）和固定版本的
`REGISTRY_IMAGE`，用 `docker compose -f registry.compose.yaml up -d` 启动。
从仓库根目录运行：

```sh
dns-manager/deploy/docker/build-images 10.10.0.10:5000 1.0.0 ./coredns
docker push 10.10.0.10:5000/coredns:1.0.0
docker push 10.10.0.10:5000/dns-node:1.0.0
docker push 10.10.0.10:5000/dns-controller:1.0.0
```

CoreDNS 须是与目标架构一致的静态 Linux 二进制，且支持 `reload_by_mtime`。
构建机器使用与节点相同的架构。每次升级使用新标签。

## 节点安装

在安全的签发环境中，用已有内部 CA 生成节点配置包。以下示例假定节点管理
IP 为 `10.10.0.11`，区域是 `infra.example.com`：

```sh
./prepare-node --name dns-a1 --region '机房 A' --ip 10.10.0.11 \
  --zone infra.example.com --zone-file /path/to/initial.zone \
  --registry 10.10.0.10:5000 --tag 1.0.0 \
  --node-ca /path/to/node-ca.crt --node-ca-key /path/to/node-ca.key \
  --controller-ca /path/to/controller-ca.crt ./dns-a1
```

初始区域文件的序列号和记录须与主控为该区域保存的记录一致。签发命令给节点
生成独立随机 token 和带管理 IP 的服务器证书，需要 Python 3 和 OpenSSL。
把 `dns-a1/controller-target.json` 加入主控对应区域的 `Targets`，
将 `dns-a1/controller-node.env` 中的变量加入主控环境文件，再重新创建主控容器。
通过可信渠道把 `dns-a1/bundle/` 及安装脚本和 Compose 模板送到节点。
签发 CA 的私钥留在签发环境。

已安装 Docker Compose 并登录镜像仓库的节点执行：

```sh
./install-node /path/to/dns-a1/bundle /opt/internal-dns
```

节点重启使用本地镜像和 `/opt/internal-dns/zones` 上次保存的区域文件。
以目录为单位挂载区域文件，让 Agent 可以原子替换文件且 CoreDNS 能看到新版。
升级前先手动拉取新标签，再修改 `.env` 并执行 `docker compose up -d --pull never`。

## 主控容器

将 `controller.compose.yaml` 放入主控运行目录。准备 `controller.json`、
`controller.env`、`state/` 和 `tls/`。证书文件名在 Compose 文件中列出。
将用户密码和节点 token 放在 `controller.env`。在 `.env` 设置
`DNS_CONTROLLER_IMAGE` 和 `CONTROLLER_LISTEN`，先拉镜像，然后执行
`docker compose -f controller.compose.yaml up -d --pull never`。

## 本地容器验收

在测试宿主机运行 `dns-manager/tests/docker-rollout ./coredns`。脚本需要
Docker Compose、Go、Python 3、OpenSSL、curl 和 dig。它临时启动一个仅绑定回环
地址的 HTTP 测试仓库，构建并推送镜像，删除节点本地镜像标签，再走安装命令重新拉取。
发布记录后停止主控和仓库，删除并重建节点容器，最后检查 UDP 和 TCP 查询。
用 `REGISTRY_PORT`、`NODE_DNS_PORT`、`NODE_API_PORT` 和 `CONTROLLER_PORT`
环境变量更改测试端口。
