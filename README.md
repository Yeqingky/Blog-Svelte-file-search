# Blog-Svelte 文件搜索中间层

独立 Go 服务, 从 Meilisearch 索引中按分享白名单搜索 OpenList 文件. 浏览器只访问本服务, 不接触 Meilisearch key 或索引中的真实存储路径.

## 环境要求

- 本地开发需要 Go 1.23 或更新版本.
- 容器运行需要 Docker Engine 与 Docker Compose 插件.
- Meilisearch search key 必须能访问配置的索引搜索接口.

## 配置

复制示例配置并限制权限:

```sh
cp .env.example .env
chmod 600 .env
```

在 `.env` 中设置服务端 `MEILI_SEARCH_KEY`, 并配置 `MEILI_URL`, `MEILI_INDEX` 和至少一条 `SHARE_<sid>` 白名单. 分享路径可以是单个绝对路径, 也可以是 JSON 数组:

```dotenv
MEILI_URL=https://meilisearch.example.com
MEILI_INDEX=openlist
SHARE_demo=/example/path
# SHARE_multi=["/example/path/a", "/example/path/b"]
CORS_ORIGINS=https://www.example.com,http://localhost:5173
```

默认 `LISTEN_ADDR` 为 `127.0.0.1:8099`. 系统环境变量优先于 `.env`. 不要提交 `.env`, 也不要把真实 key 写入源码、示例、日志或镜像层.

## 本地运行

```sh
go test ./...
go run .
```

服务启动时会检查分享根目录. 索引中没有匹配结果时会记录告警, 但不会阻止服务启动.

## API

### `GET /search`

必填参数为 `share` 和 `q`. `share` 可传分享 ID 或 `/@s/<sid>` 形式. 可选参数:

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `page` | `1` | 正整数页码 |
| `per_page` | `20` | 每页条数, 最大 `50` |
| `dir` | 空 | 分享内的相对目录, 不允许 `..` 越界 |

```sh
curl --get 'http://127.0.0.1:8099/search' \
  --data-urlencode 'share=demo' \
  --data-urlencode 'q=keyword'
```

成功响应包含分享内相对路径 `path`、目录 `dir`、名称 `name`、目录标记 `isDir` 和大小 `size`; 不会返回 Meilisearch 文档中的真实 `parent` 路径.

### `GET /healthz`

匿名检查 Meilisearch 状态. 正常时返回 `{"status":"available"}`, 上游不可用时返回 503.

CORS 来源由 `CORS_ORIGINS` 限定. `/search` 按客户端 IP 限流, 约每分钟 30 次, burst 为 10.

## Docker Compose

Compose 从本地 `.env` 注入配置. 容器内监听 `0.0.0.0:8099`, 主机端口 `8099` 发布到所有网络接口. 通过主机防火墙或反向代理限制访问; CORS 和 IP 限流不是访问认证.

本地构建并运行:

```sh
docker compose -f docker-compose-dev.yml up --build -d
docker compose -f docker-compose-dev.yml logs -f file-search
docker compose -f docker-compose-dev.yml down
```

运行已发布的 GHCR 镜像:

```sh
docker compose pull
docker compose up -d
```

固定到 `main` 分支的某次提交时, 在两条命令中使用相同的 tag, 例如 `IMAGE_TAG=sha-abcdef0 docker compose pull` 与 `IMAGE_TAG=sha-abcdef0 docker compose up -d`.

主机端口固定为 `8099`. Docker build context 会排除 `.env`、其他 `.env.*`、测试文件和本地文档.

## GitHub 镜像发布

`.github/workflows/docker-publish.yml` 会构建 `linux/amd64` 与 `linux/arm64` 镜像, 并发布到 `ghcr.io/yeqingky/blog-svelte-file-search`:

- 推送到 `main` 时发布 `sha-<7 位提交短 hash>`.
- 推送符合数字格式的 `vX.Y.Z` tag 时发布 `vX.Y.Z` 与 `latest`.
- Pull request 只构建镜像, 不推送.

工作流使用 GitHub 内置的 `GITHUB_TOKEN` 和 `packages: write` 权限发布, 无需单独配置 token secret. 非数字 `vX.Y.Z` tag 会被工作流拒绝.
